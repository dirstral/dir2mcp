package main

import (
	"os"

	"dir2mcp/internal/cli"
)

func main() {
	app := cli.NewApp()
	// Keep the legacy binary as a thin wrapper over the integrated CLI command.
	os.Exit(app.Run(append([]string{"bridge", "elevenlabs"}, os.Args[1:]...)))
}
