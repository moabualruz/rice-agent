package app

import (
	"fmt"
	"os"

	"github.com/moabualruz/rice-agent/internal/cli"
)

func Run(env *cli.Env) int {
	if len(env.Args) == 0 || env.Args[0] == "--help" || env.Args[0] == "-h" || env.Args[0] == "help" {
		usage(env)
		if len(env.Args) == 0 {
			return cli.ExitUsage
		}
		return cli.ExitOK
	}
	command, ok := cli.Lookup(env.Args[0])
	if !ok {
		return cli.Usagef(env, "unknown command %q; run rice-agent --help", env.Args[0])
	}
	env.Args = env.Args[1:]
	return command.Run(env)
}

func usage(env *cli.Env) {
	fmt.Fprintln(env.Stdout, "Usage: rice-agent <command> [flags]")
	fmt.Fprintln(env.Stdout, "Decide reuse before acting: find existing tools, build verified ones when work repeats.")
	fmt.Fprintln(env.Stdout)
	for _, name := range cli.Names() {
		command, _ := cli.Lookup(name)
		fmt.Fprintf(env.Stdout, "  %-12s %s\n", name, command.Summary)
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Exit codes: 0 success, 1 negative result, 2 usage, 3 environment. Details: rice-agent guide")
}

func Main() {
	os.Exit(Run(cli.OSEnv(os.Args[1:])))
}
