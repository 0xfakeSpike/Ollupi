//go:build !unix

package sandbox

import (
	"os"
	"os/exec"
)

func ownProcessGroup(*exec.Cmd)  {}
func killProcessGroup(*exec.Cmd) {}

func exitSignal(*os.ProcessState) (int, string, bool) { return 0, "", false }
