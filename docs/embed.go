// Package docs embeds the guidance the CLI prints with `agent-dry guide`.
package docs

import _ "embed"

// Guide is the DRY principle in detail: work, orchestration and code.
//
//go:embed guide.md
var Guide string

// ToolContract is the contract every tool follows.
//
//go:embed tool-contract.md
var ToolContract string
