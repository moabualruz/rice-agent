//go:build !unix

package gates

import "os/exec"

func setProcGroup(*exec.Cmd)  {}
func killProcGroup(*exec.Cmd) {}
