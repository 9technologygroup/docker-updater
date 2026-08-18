package audit

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/9technologygroup/docker-updater/internal/config"
)

type Identity struct {
	Name string
	UID  uint32
	GIDs map[uint32]bool
}

type Finding struct {
	Path   string
	Reason string
}

func LookupIdentity(name string) (*Identity, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("look up user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("user %q has a non-numeric uid", name)
	}

	groupIDs, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("read groups for %q: %w", name, err)
	}

	id := &Identity{Name: name, UID: uint32(uid), GIDs: make(map[uint32]bool, len(groupIDs))}
	for _, g := range groupIDs {
		gid, err := strconv.ParseUint(g, 10, 32)
		if err != nil {
			continue
		}
		id.GIDs[uint32(gid)] = true
	}
	return id, nil
}

func Run(cfg *config.Config, id *Identity) []Finding {
	var findings []Finding
	seen := make(map[string]bool)

	for _, t := range cfg.Targets {
		paths := []string{t.Dir}
		if t.ComposeFile != "" {
			paths = append(paths, filepath.Join(t.Dir, t.ComposeFile))
		} else {
			for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
				candidate := filepath.Join(t.Dir, name)
				if _, err := os.Stat(candidate); err == nil {
					paths = append(paths, candidate)
					break
				}
			}
		}
		if t.EnvFile != "" {
			paths = append(paths, filepath.Join(t.Dir, t.EnvFile))
		}
		paths = append(paths, filepath.Join(t.Dir, ".env"))
		if t.PreUpdate.Configured() {
			paths = append(paths, t.PreUpdate.Command)
		}

		for _, p := range paths {
			chains := [][]string{withAncestors(p)}
			if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != p {
				chains = append(chains, withAncestors(resolved))
			}

			for _, chain := range chains {
				for _, candidate := range chain {
					if seen[candidate] {
						continue
					}
					seen[candidate] = true
					if reason, bad := writableBy(candidate, id); bad {
						findings = append(findings, Finding{Path: candidate, Reason: reason})
					}
				}
			}
		}
	}
	return findings
}

func withAncestors(path string) []string {
	var out []string
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		out = append(out, p)
		if p == "/" || p == "." {
			break
		}
	}
	return out
}

func writableBy(path string, id *Identity) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}

	perm := info.Mode().Perm()

	// On a sticky directory only the owner of an entry may replace or remove it,
	// so group and world write bits grant nothing. /tmp is the usual example.
	sticky := info.IsDir() && info.Mode()&os.ModeSticky != 0

	switch {
	case stat.Uid == id.UID && perm&0o200 != 0:
		return fmt.Sprintf("owned by %s and owner writable (mode %04o)", id.Name, perm), true
	case sticky:
		return "", false
	case perm&0o002 != 0:
		return fmt.Sprintf("world writable (mode %04o)", perm), true
	case id.GIDs[stat.Gid] && perm&0o020 != 0:
		return fmt.Sprintf("group writable (mode %04o) by a group %s belongs to", perm, id.Name), true
	}
	return "", false
}
