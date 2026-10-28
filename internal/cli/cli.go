// Package cli holds the command registry and the exit-code convention shared by every command.
package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
)

// Exit codes shared by every rice-agent command.
const (
	ExitOK       = 0 // success
	ExitNegative = 1 // negative result: nothing found, a gate failed, drift detected
	ExitUsage    = 2 // usage error
	ExitEnv      = 3 // environment or setup failure: unreadable index, missing config
)

// Env is what a command receives; tests substitute the streams and Getenv.
type Env struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
}

// Command is one subcommand. Run returns the process exit code.
type Command struct {
	Name    string
	Summary string // one line for the top-level help
	Run     func(env *Env) int
}

var registry = map[string]Command{}

// Register adds a command; each command file calls it from init.
func Register(c Command) {
	if _, dup := registry[c.Name]; dup {
		panic("rice-agent: duplicate command " + c.Name)
	}
	registry[c.Name] = c
}

// Lookup returns a registered command.
func Lookup(name string) (Command, bool) {
	c, ok := registry[name]
	return c, ok
}

// Names returns registered command names in order.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Usagef prints a usage error and returns ExitUsage.
func Usagef(env *Env, format string, a ...any) int {
	fmt.Fprintf(env.Stderr, "rice-agent: "+format+"\n", a...)
	return ExitUsage
}

// Envf prints an environment error and returns ExitEnv.
func Envf(env *Env, format string, a ...any) int {
	fmt.Fprintf(env.Stderr, "rice-agent: "+format+"\n", a...)
	return ExitEnv
}

// OSEnv builds an Env from the process.
func OSEnv(args []string) *Env {
	return &Env{Args: args, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv}
}
