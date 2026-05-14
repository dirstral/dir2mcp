package main

import (
	"os"

	"github.com/dirstral/dir2mcp/internal/cli"
)

func main() {
	app := cli.NewApp()
	os.Exit(app.Run(os.Args[1:]))
}
