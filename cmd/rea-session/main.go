package main

import (
	"os"

	"rea-session-cli/internal/cli"
)

// version is overridden at build time with -ldflags "-X main.version=..."
var version = "0.1.0"

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, cli.Options{Version: version}))
}
