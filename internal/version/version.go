package version

import (
	"fmt"
	"runtime"
	"strings"
)

const SourceURL = "https://github.com/9technologygroup/docker-updater"

var (
	Version   = "dev"
	Commit    = "none"
	Date      = "unknown"
	GoVersion = runtime.Version()
)

func Short() string { return Version }

func Info(binary string) string {
	return fmt.Sprintf("%s %s (%s) built %s with %s", binary, Version, ShortCommit(), Date, GoVersion)
}

func Full(binary string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s\n", binary, Version)
	fmt.Fprintf(&sb, "  commit:  %s\n", Commit)
	fmt.Fprintf(&sb, "  built:   %s\n", Date)
	fmt.Fprintf(&sb, "  go:      %s\n", GoVersion)
	fmt.Fprintf(&sb, "  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&sb, "  licence: AGPL-3.0-or-later\n")
	fmt.Fprintf(&sb, "  source:  %s\n", SourceURL)
	return sb.String()
}

func ShortCommit() string {
	if len(Commit) > 7 {
		return Commit[:7]
	}
	return Commit
}
