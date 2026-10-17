//go:build unix

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunHostKeepsTheEnvironmentAllowlistAndKillsTheGroup(t *testing.T) {
	t.Setenv("SECRET_TOKEN", "leak")
	t.Setenv("ALLOWED_ONE", "yes")
	dir := t.TempDir()
	marker := filepath.Join(dir, "survivor")
	// A background child that would write the marker after the command ends.
	script := `(sleep 1; echo alive > "` + marker + `") & echo "[$SECRET_TOKEN][$ALLOWED_ONE][$SET_HERE]"`
	res, err := RunHost(context.Background(), HostCommand{
		Program: "/bin/sh", Args: []string{"-c", script}, Cwd: dir,
		EnvPassthrough: []string{"ALLOWED_ONE"}, Env: map[string]string{"SET_HERE": "set"}, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "[][yes][set]" {
		t.Fatalf("environment: %q", res.Stdout)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a background child outlived the command")
	}
	if _, err := RunHost(context.Background(), HostCommand{Program: "/bin/sleep", Args: []string{"5"}, Timeout: 200 * time.Millisecond}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a timeout error, got %v", err)
	}
	res, err = RunHost(context.Background(), HostCommand{Program: "/bin/sh", Args: []string{"-c", "yes | head -c 100000"}, Timeout: 5 * time.Second, MaxOutputBytes: 1000})
	if err != nil || !res.Truncated {
		t.Fatalf("want truncated output, got %v %v", res.Truncated, err)
	}
}
