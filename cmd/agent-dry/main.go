// Command agent-dry finds, scaffolds, checks, installs and audits reusable agent tools.
package main

import (
	"fmt"
	"os"

	"github.com/moabualruz/agent-dry/internal/cli"
	_ "github.com/moabualruz/agent-dry/internal/commands"
)

func main() {
	os.Exit(run(cli.OSEnv(os.Args[1:])))
}

func run(env *cli.Env) int {
	if len(env.Args) == 0 || env.Args[0] == "--help" || env.Args[0] == "-h" || env.Args[0] == "help" {
		usage(env)
		if len(env.Args) == 0 {
			return cli.ExitUsage
		}
		return cli.ExitOK
	}
	c, ok := cli.Lookup(env.Args[0])
	if !ok {
		return cli.Usagef(env, "unknown command %q; run agent-dry --help", env.Args[0])
	}
	env.Args = env.Args[1:]
	return c.Run(env)
}

func usage(env *cli.Env) {
	fmt.Fprintln(env.Stdout, "Usage: agent-dry <command> [flags]")
	fmt.Fprintln(env.Stdout, "Decide reuse before acting: find existing tools, build verified ones when work repeats.")
	fmt.Fprintln(env.Stdout)
	for _, n := range cli.Names() {
		c, _ := cli.Lookup(n)
		fmt.Fprintf(env.Stdout, "  %-9s %s\n", n, c.Summary)
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Exit codes: 0 success, 1 negative result, 2 usage, 3 environment. Details: agent-dry guide")
}
