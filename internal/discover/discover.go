package discover

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/9technologygroup/docker-updater/internal/compose"
	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/wire"
)

func Run(ctx context.Context, r *compose.Runner, cfg *config.Config) wire.DiscoverResult {
	var result wire.DiscoverResult

	projects, err := listProjects(ctx, r)
	if err != nil {
		result.Warning = "could not list compose projects: " + err.Error()
	}

	byDir := targetDirs(cfg)
	for i := range projects {
		projects[i].Dir = projectDir(projects[i].ConfigFiles)
		projects[i].Target = byDir[resolve(projects[i].Dir)]
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	result.Projects = projects

	loose, err := looseContainers(ctx, r)
	if err != nil && result.Warning == "" {
		result.Warning = "could not list containers: " + err.Error()
	}
	sort.Slice(loose, func(i, j int) bool { return loose[i].Name < loose[j].Name })
	result.Loose = loose

	return result
}

func targetDirs(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Targets))
	for _, t := range cfg.Targets {
		out[resolve(t.Dir)] = t.Name
	}
	return out
}

func resolve(dir string) string {
	if dir == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return filepath.Clean(dir)
}

func projectDir(configFiles []string) string {
	if len(configFiles) == 0 {
		return ""
	}
	return filepath.Dir(configFiles[0])
}

type projectEntry struct {
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	ConfigFiles string `json:"ConfigFiles"`
}

func listProjects(ctx context.Context, r *compose.Runner) ([]wire.Project, error) {
	out, err := r.Output(ctx, "compose", "ls", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}

	var entries []projectEntry
	if err := decodeJSONStream(out, &entries); err != nil {
		return nil, err
	}

	projects := make([]wire.Project, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		var files []string
		for _, f := range strings.Split(e.ConfigFiles, ",") {
			if f = strings.TrimSpace(f); f != "" {
				files = append(files, f)
			}
		}
		projects = append(projects, wire.Project{Name: e.Name, Status: e.Status, ConfigFiles: files})
	}
	return projects, nil
}

type containerEntry struct {
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	State  string `json:"State"`
	Labels string `json:"Labels"`
}

func looseContainers(ctx context.Context, r *compose.Runner) ([]wire.Container, error) {
	out, err := r.Output(ctx, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}

	var entries []containerEntry
	if err := decodeJSONStream(out, &entries); err != nil {
		return nil, err
	}

	var loose []wire.Container
	for _, e := range entries {
		if e.Names == "" {
			continue
		}
		if project := label(e.Labels, "com.docker.compose.project"); project != "" {
			continue
		}
		name := strings.TrimSpace(strings.Split(e.Names, ",")[0])
		loose = append(loose, wire.Container{Name: name, Image: e.Image, State: e.State})
	}
	return loose, nil
}

func label(labels, key string) string {
	for _, pair := range strings.Split(labels, ",") {
		k, v, found := strings.Cut(pair, "=")
		if found && strings.TrimSpace(k) == key {
			return v
		}
	}
	return ""
}

func decodeJSONStream(out string, target any) error {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	if strings.HasPrefix(out, "[") {
		return json.Unmarshal([]byte(out), target)
	}

	var lines []json.RawMessage
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, json.RawMessage(line))
		}
	}
	joined, err := json.Marshal(lines)
	if err != nil {
		return err
	}
	return json.Unmarshal(joined, target)
}
