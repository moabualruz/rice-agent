// Command rice-agent provides the cross-project agent toolbelt and instruction writer.
package main

import (
	"github.com/moabualruz/rice-agent/internal/app"
	_ "github.com/moabualruz/rice-agent/internal/commands"
)

func main() { app.Main() }
