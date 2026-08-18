package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

func agentUnreachable(socket string, err error) string {
	var b strings.Builder

	info, statErr := os.Stat(socket)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		fmt.Fprintf(&b, "The update agent is not running: there is no socket at %s.\n\n", socket)
		fmt.Fprintf(&b, "  Start it:   sudo systemctl enable --now dup-agent\n")
		fmt.Fprintf(&b, "  Check it:   systemctl status dup-agent\n")
		fmt.Fprintf(&b, "  Why not:    sudo journalctl -u dup-agent -n 30 --no-pager\n")

	case errors.Is(statErr, fs.ErrPermission):
		fmt.Fprintf(&b, "Cannot see %s.\n\n", socket)
		fmt.Fprintf(&b, "  Run as root, or join the group that owns it:\n")
		fmt.Fprintf(&b, "    sudo usermod -aG dup $USER   (then log out and back in)\n")

	case statErr != nil:
		fmt.Fprintf(&b, "Could not check %s: %v\n", socket, statErr)

	case info.Mode()&os.ModeSocket == 0:
		fmt.Fprintf(&b, "%s exists but is not a socket. Remove it and restart the agent:\n\n", socket)
		fmt.Fprintf(&b, "  sudo rm %s && sudo systemctl restart dup-agent\n", socket)

	case errors.Is(err, syscall.ECONNREFUSED):
		fmt.Fprintf(&b, "The socket at %s is stale: nothing is listening on it.\n\n", socket)
		fmt.Fprintf(&b, "  Restart the agent:  sudo systemctl restart dup-agent\n")
		fmt.Fprintf(&b, "  Check why:          sudo journalctl -u dup-agent -n 30 --no-pager\n")

	case errors.Is(err, fs.ErrPermission):
		fmt.Fprintf(&b, "Not allowed to connect to %s.\n\n", socket)
		fmt.Fprintf(&b, "  Run as root, or join the group that owns it:\n")
		fmt.Fprintf(&b, "    sudo usermod -aG dup $USER   (then log out and back in)\n")

	default:
		fmt.Fprintf(&b, "Could not reach the update agent on %s.\n\n  %v\n", socket, err)
	}

	return strings.TrimRight(b.String(), "\n")
}

func apiUnreachable(base string, err error) error {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("the dup API is not running on %s\n\n"+
			"  Start it:   sudo systemctl enable --now dup-agent dup\n"+
			"  Check it:   systemctl status dup\n"+
			"  Why not:    sudo journalctl -u dup -n 30 --no-pager", base)
	}
	return fmt.Errorf("could not reach the dup API on %s: %w", base, err)
}
