package main

// Each package registers its own commands from init.
import (
	_ "github.com/moabualruz/agent-dry/internal/audit"
	_ "github.com/moabualruz/agent-dry/internal/dist"
	_ "github.com/moabualruz/agent-dry/internal/gates"
	_ "github.com/moabualruz/agent-dry/internal/registry"
)
