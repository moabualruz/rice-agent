package main

// Each package registers its own commands from init.
import (
	_ "github.com/moabualruz/rice-agent/internal/audit"
	_ "github.com/moabualruz/rice-agent/internal/dist"
	_ "github.com/moabualruz/rice-agent/internal/gates"
	_ "github.com/moabualruz/rice-agent/internal/registry"
)
