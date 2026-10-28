// Package commands registers every rice-agent subcommand. Each command lives in its own file and
// registers itself from init, so parallel work never edits a shared dispatch table.
package commands

import (
	"fmt"

	"github.com/moabualruz/rice-agent/internal/cli"
)

// Version is set at build time with -ldflags "-X github.com/moabualruz/rice-agent/internal/commands.Version=v0.1.0".
var Version = "dev"

func init() {
	cli.Register(cli.Command{Name: "version", Summary: "print the build version", Run: func(env *cli.Env) int {
		fmt.Fprintln(env.Stdout, Version)
		return cli.ExitOK
	}})
}
