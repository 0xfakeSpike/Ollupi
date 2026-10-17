package sandbox

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDenialLogAttributesDenialsToTheirCommand(t *testing.T) {
	l := newDenialLog()
	id, tag := l.newTag()
	other, otherTag := l.newTag()
	stream := strings.Join([]string{
		"Filtering the log data using \"eventMessage CONTAINS ...\"",
		"2026-10-14 12:00:00.1 E  kernel[0:1] Sandbox: touch(812) deny(1) file-write-create /Users/u/.zshrc " + tag,
		"2026-10-14 12:00:00.2 E  kernel[0:1] Sandbox: sh(811) deny(1) file-write-create /Users/u/.zshrc",
		tag,
		"2026-10-14 12:00:00.3 E  kernel[0:1] Sandbox: curl(813) deny(1) network-outbound 1.2.3.4:443 " + otherTag,
		"2026-10-14 12:00:00.4 E  kernel[0:1] Sandbox: git(814) deny(1) mach-lookup com.apple.diagnosticd " + tag,
		"2026-10-14 12:00:00.45 E  kernel[0:1] Sandbox: Google Chrome Helper(816) deny(1) file-write-data /Users/u/x",
		"   continued detail line",
		tag,
		"2026-10-14 12:00:00.5 E  kernel[0:1] Sandbox: cat(815) deny(1) file-read-data /Users/u/.ssh/id_rsa " + tag,
		"2026-10-14 12:00:00.6 E  kernel[0:1] Sandbox: touch(812) deny(1) file-write-create /Users/u/.zshrc " + tag,
	}, "\n")
	l.consume(bufio.NewScanner(strings.NewReader(stream)))
	got := l.collect(id, false)
	want := []string{
		"touch: file-write-create /Users/u/.zshrc",
		"sh: file-write-create /Users/u/.zshrc",
		"Google Chrome Helper: file-write-data /Users/u/x",
		"cat: file-read-data /Users/u/.ssh/id_rsa",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := l.collect(other, false); len(got) != 1 || got[0] != "curl: network-outbound 1.2.3.4:443" {
		t.Fatalf("other command got %q", got)
	}
	if got := l.collect(id, false); got != nil {
		t.Fatalf("collected twice: %q", got)
	}
}

func TestDenialLogIgnoresOtherSessions(t *testing.T) {
	a, b := newDenialLog(), newDenialLog()
	id, _ := a.newTag()
	_, foreign := b.newTag()
	a.consume(bufio.NewScanner(strings.NewReader("kernel Sandbox: rm(9) deny(1) file-write-unlink /x " + foreign)))
	if got := a.collect(id, false); got != nil {
		t.Fatalf("took another session's denial: %q", got)
	}
}

// The stream must end when the app does, crash included: closing the pipe
// the shell watches kills it. A stand-in for `log` records its pid.
func TestDenialLogStreamEndsWithItsPipe(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	fake := filepath.Join(dir, "log")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho $$ > "+pidFile+"\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	saved := logExecutable
	logExecutable = fake
	t.Cleanup(func() { logExecutable = saved })
	l := newDenialLog()
	if err := l.start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	for deadline := time.Now().Add(3 * time.Second); pid == 0; {
		raw, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		if time.Now().After(deadline) {
			t.Fatal("the stream never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.close()
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the stream outlived its pipe")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for deadline := time.Now().Add(3 * time.Second); l.running(); {
		if time.Now().After(deadline) {
			t.Fatal("still marked running")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
