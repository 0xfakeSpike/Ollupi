package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const DefaultMaxOutputBytes = 1 << 20

const drainDelay = 2 * time.Second

func runProcess(ctx context.Context, cmd *exec.Cmd, program string, limit time.Duration, maxOutput int) (Result, error) {
	stdout := newCappedBuffer(maxOutput)
	stderr := newCappedBuffer(maxOutput)

	outR, outW, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: %s: %w", program, err)
	}
	defer outR.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		outW.Close()
		return Result{}, fmt.Errorf("sandbox: %s: %w", program, err)
	}
	defer errR.Close()
	cmd.Stdout, cmd.Stderr = outW, errW
	ownProcessGroup(cmd)
	err = cmd.Start()

	outW.Close()
	errW.Close()
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: start %s: %w", program, err)
	}
	var copies sync.WaitGroup
	for _, c := range []struct {
		dst *cappedBuffer
		src *os.File
	}{{stdout, outR}, {stderr, errR}} {
		copies.Add(1)
		go func() {
			defer copies.Done()
			_, _ = io.Copy(c.dst, c.src)
		}()
	}
	err = cmd.Wait()
	killProcessGroup(cmd)
	drained := make(chan struct{})
	go func() { copies.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(drainDelay):

		outR.Close()
		errR.Close()
		<-drained
	}
	code, signal := 0, ""
	if ps := cmd.ProcessState; ps != nil {
		code = ps.ExitCode()

		if n, name, ok := exitSignal(ps); ok {
			code, signal = 128+n, name
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Result{}, fmt.Errorf("sandbox: %s timed out after %s and was killed%s", program, limit, outputTail(stdout, stderr))
	}
	if ctx.Err() != nil {
		return Result{}, fmt.Errorf("sandbox: %s was cancelled: %w%s", program, ctx.Err(), outputTail(stdout, stderr))
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || code < 0 {
			return Result{}, fmt.Errorf("sandbox: %s failed: %w%s", program, err, outputTail(stdout, stderr))
		}
	}
	return Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code, Signal: signal, Truncated: stdout.dropped > 0 || stderr.dropped > 0}, nil
}

func outputTail(stdout, stderr *cappedBuffer) string {
	const n = 2000
	var parts []string
	for _, b := range []struct {
		name string
		buf  *cappedBuffer
	}{{"stdout", stdout}, {"stderr", stderr}} {
		s := strings.TrimSpace(b.buf.String())
		if s == "" {
			continue
		}
		if len(s) > n {
			s = "…" + s[len(s)-n:]
		}
		parts = append(parts, b.name+": "+s)
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}

type cappedBuffer struct {
	max     int
	head    []byte
	tail    []byte
	dropped int64
}

func newCappedBuffer(max int) *cappedBuffer {
	if max <= 0 {
		max = DefaultMaxOutputBytes
	}
	return &cappedBuffer{max: max}
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	half := b.max / 2
	if room := half - len(b.head); room > 0 {
		take := min(room, len(p))
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	b.tail = append(b.tail, p...)
	if over := len(b.tail) - (b.max - half); over > 0 {
		b.dropped += int64(over)
		b.tail = append(b.tail[:0], b.tail[over:]...)
	}
	return n, nil
}

func (b *cappedBuffer) String() string {
	if b.dropped == 0 {
		return string(b.head) + string(b.tail)
	}
	return string(b.head) + fmt.Sprintf("\n[… %d bytes omitted …]\n", b.dropped) + string(b.tail)
}
