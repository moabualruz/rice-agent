// Package docs embeds the guidance the CLI prints with `agent-dry guide`.
package docs

import (
	"embed"
	"sort"
	"strings"
)

//go:embed *.md
var files embed.FS

// Guide is the DRY principle in detail: work, orchestration and code.
//
//go:embed guide.md
var Guide string

// ToolContract is the contract every tool follows.
//
//go:embed tool-contract.md
var ToolContract string

// Topic returns docs/<name>.md; "tools" is an alias for the tool contract.
func Topic(name string) (string, bool) {
	if name == "tools" {
		name = "tool-contract"
	}
	if name == "" || strings.ContainsAny(name, "/\\.") {
		return "", false
	}
	b, err := files.ReadFile(name + ".md")
	if err != nil {
		return "", false
	}
	return string(b), true
}

// Topics lists the topic names, excluding the main guide.
func Topics() []string {
	entries, _ := files.ReadDir(".")
	var out []string
	for _, e := range entries {
		n := strings.TrimSuffix(e.Name(), ".md")
		if n == e.Name() || n == "guide" {
			continue
		}
		if n == "tool-contract" {
			n = "tools"
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
