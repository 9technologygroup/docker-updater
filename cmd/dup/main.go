package main

import (
	"fmt"
	"os"

	"github.com/9technologygroup/docker-updater/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}
