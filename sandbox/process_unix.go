//go:build unix

package sandbox

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// ownProcessGroup starts cmd as the leader of a new process group, and makes
// cancellation kill that whole group rather than only its first process.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// exitSignal reports the signal that killed a process, if one did.
func exitSignal(ps *os.ProcessState) (int, string, bool) {
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, "", false
	}
	sig := ws.Signal()
	name := unix.SignalName(sig)
	if name == "" {
		name = sig.String()
	}
	return int(sig), name, true
}
