package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// HostCommand is a process that runs on the host by design, outside any
// sandbox: a trusted helper such as a browser driver, which a sandbox would
// cut off from what it drives. It still gets the sandbox's supervision: its
// whole process group is killed when it exits or times out, its output is
// capped, and it sees only the base environment plus the named variables.
type HostCommand struct {
	Program string
	Args    []string
	Cwd     string
	// EnvPassthrough names host environment variables it may see, besides
	// the base ones; Env sets variables and wins.
	EnvPassthrough []string
	Env            map[string]string
	Timeout        time.Duration
	// MaxOutputBytes caps stdout and stderr each; 0 is DefaultMaxOutputBytes.
	MaxOutputBytes int
}

// RunHost runs c to completion on the host. A non-zero exit code is a
// result; failing to start, timing out, and cancellation are errors.
func RunHost(ctx context.Context, c HostCommand) (Result, error) {
	if c.Program == "" || c.Timeout <= 0 {
		return Result{}, fmt.Errorf("sandbox: incomplete host command")
	}
	if c.MaxOutputBytes < 0 {
		return Result{}, fmt.Errorf("sandbox: max output bytes must be >= 0")
	}
	if err := ValidateEnvNames(c.EnvPassthrough); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Program, c.Args...)
	cmd.Dir = c.Cwd
	cmd.Env = Environ(os.Environ(), c.EnvPassthrough, c.Env)
	return runProcess(ctx, cmd, c.Program, c.Timeout, c.MaxOutputBytes)
}
