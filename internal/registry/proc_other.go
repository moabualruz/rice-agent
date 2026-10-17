//go:build !unix

package registry

import "os/exec"

// ownGroup is a no-op where process groups are not available; the context kills the child.
func ownGroup(cmd *exec.Cmd) {}

// killGroup is a no-op where process groups are not available.
func killGroup(cmd *exec.Cmd) {}
