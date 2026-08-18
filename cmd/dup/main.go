package main

import (
	"fmt"
	"os"

	"github.com/PatchMon/docker-updater/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}
