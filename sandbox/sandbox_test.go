package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestProfileKeepsPathsOutOfTheProfileText(t *testing.T) {
	evil := `/tmp/a") (allow default) ("`
	profile, params, err := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, WritableRoots: []string{"/w"}, DenyRead: []string{filepath.Clean(evil)}}, []string{"/private/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(profile, "allow default") {
		t.Fatal("a path was spliced into the profile")
	}
	if !slices.Contains(params, [2]string{"DENY_READ_0", filepath.Clean(evil)}) {
		t.Fatalf("deny path not passed as a parameter: %v", params)
	}
}

func TestProfileOrdersDeniesAfterAllows(t *testing.T) {
	profile, _, err := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, Network: true, WritableRoots: []string{"/w"}, DenyRead: []string{"/w/.env"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"(deny default)", "(allow file-read*)", "(allow file-write*", "(allow network-outbound)", "(deny file-read* file-write*", "(deny file-write-unlink (require-all (vnode-type DIRECTORY)"}
	last := -1
	for _, rule := range order {
		i := strings.Index(profile, rule)
		if i <= last {
			t.Fatalf("%q is missing or out of order", rule)
		}
		last = i
	}
}

// pretendMacOS lets platform-independent sandbox logic run on any host.
func pretendMacOS(t *testing.T) {
	t.Helper()
	saved := seatbeltSupported
	seatbeltSupported = func() error { return nil }
	t.Cleanup(func() { seatbeltSupported = saved })
}

func TestResolveMode(t *testing.T) {
	pretendMacOS(t)
	for _, tc := range []struct {
		mode string
		want Mode
	}{
		{"", ModeWorkspaceWrite},
		{"read-only", ModeReadOnly},
		{"workspace-write", ModeWorkspaceWrite},
		{"full-access", ModeFullAccess},
		{"bogus", ""},
	} {
		got, err := ResolveMode(tc.mode)
		if (err == nil) != (tc.want != "") || got != tc.want {
			t.Errorf("ResolveMode(%q) = %q, %v; want %q", tc.mode, got, err, tc.want)
		}
	}
}

// Off macOS there is no sandbox; running without one must be chosen.
func TestSandboxedModesFailClosedOffMacOS(t *testing.T) {
	seatbeltSupported = func() error { return fmt.Errorf("no sandbox here") }
	t.Cleanup(func() { seatbeltSupported = defaultSeatbeltSupported })
	for _, mode := range []string{"", "read-only", "workspace-write"} {
		if _, err := ResolveMode(mode); err == nil {
			t.Errorf("mode %q accepted without a sandbox", mode)
		}
	}
	if _, err := ResolveMode("full-access"); err != nil {
		t.Errorf("full-access refused: %v", err)
	}
}

func TestProtectedAncestorsStopAtTheWritableRoot(t *testing.T) {
	got := protectedAncestors([]string{"/w/a/b/.env", "/elsewhere/key", "/w"}, []string{"/w"})
	want := []string{"/w/a/b", "/w/a", "/w"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestCanonicalPathResolvesExistingPrefix(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalPath(filepath.Join(dir, "missing", "file"), "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(real, "missing", "file"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	got, err = CanonicalPath("~/.ssh", "/nonexistent-ollupi/u")
	if err != nil || got != "/nonexistent-ollupi/u/.ssh" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// The generated profile must load: sandbox-exec rejects a malformed one
// before running anything.
func TestGeneratedProfilesLoad(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS only")
	}
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	home, _ := os.UserHomeDir()
	for _, p := range []Policy{
		{Mode: ModeReadOnly},
		{Mode: ModeWorkspaceWrite, WritableRoots: []string{root}, DenyRead: append(DefaultDenyRead(home), filepath.Join(root, "x"))},
		{Mode: ModeWorkspaceWrite, Network: true, WritableRoots: []string{root}},
		{Mode: ModeWorkspaceWrite, Network: true, WritableRoots: []string{root}, ReadOnly: []string{filepath.Join(root, "ro")}, UnixSockets: []string{filepath.Join(root, "sock")}},
		{Mode: ModeReadOnly, Network: true, WritableRoots: []string{root}},
	} {
		args, err := SeatbeltArgs(p, SeatbeltTempDirs(), []string{"/usr/bin/true"})
		if err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(SeatbeltExecutable, args...).CombinedOutput(); err != nil {
			t.Fatalf("%+v: %v: %s", p, err, out)
		}
	}
}

func TestProfileClosesUnixSocketsAfterTheNetworkAllow(t *testing.T) {
	profile, _, err := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, Network: true, WritableRoots: []string{"/w"}, UnixSockets: []string{"/run/my.app/sock"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{
		"(allow system-socket (socket-domain AF_UNIX))",
		"(allow network-outbound)",
		"(deny network-outbound (remote unix-socket))",
		`(allow network-outbound (remote unix-socket (regex #"^/private/var/run/mDNSResponder$")))`,
		`(allow network-outbound (remote unix-socket (regex #"^/run/my\.app/sock(/|$)")))`,
	}
	last := -1
	for _, rule := range order {
		i := strings.Index(profile, rule)
		if i <= last {
			t.Fatalf("%q is missing or out of order in\n%s", rule, profile)
		}
		last = i
	}
	off, _, err := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, WritableRoots: []string{"/w"}, UnixSockets: []string{"/run/sock"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, "network-outbound") {
		t.Fatal("network off still opens sockets")
	}
}

// With an allowlist, the profile opens only the proxy's port: no other
// outbound rule, no DNS, no Unix sockets.
func TestProfileWithAProxyAllowsOnlyItsPort(t *testing.T) {
	profile, _, err := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, Network: true, ProxyPort: 40123, WritableRoots: []string{"/w"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, `(allow network-outbound (remote ip "localhost:40123"))`) {
		t.Fatalf("proxy port not allowed:\n%s", profile)
	}
	for _, open := range []string{"(allow network-outbound)\n", "(allow network-inbound)", "mDNSResponder", "remote unix-socket"} {
		if strings.Contains(profile, open) {
			t.Errorf("proxied profile contains %q", open)
		}
	}
	if _, _, err := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, ProxyPort: 1, WritableRoots: []string{"/w"}}, nil); err == nil {
		t.Error("a proxy port without network was accepted")
	}
}

func TestProfileTagsDenialsForTheLogMonitor(t *testing.T) {
	profile, _, err := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, Network: true, WritableRoots: []string{"/w"}, DenyRead: []string{"/s"}, LogTag: "OSBX0123456789abcdef_x1SBX"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		`(deny default (with message "OSBX0123456789abcdef_x1SBX"))`,
		`(deny file-read* file-write* (subpath (param "DENY_READ_0")) (with message "OSBX0123456789abcdef_x1SBX"))`,
		`(deny network-outbound (remote unix-socket) (with message "OSBX0123456789abcdef_x1SBX"))`,
	} {
		if !strings.Contains(profile, rule) {
			t.Errorf("missing %s", rule)
		}
	}
	if _, _, err := SeatbeltProfile(Policy{Mode: ModeReadOnly, LogTag: `x") (allow default`}, nil); err == nil {
		t.Error("an unsafe log tag was accepted")
	}
}

func TestSocketPathsThatCannotBeQuotedAreRefused(t *testing.T) {
	for _, path := range []string{`/tmp/a"b`, `/tmp/a\b`, "/tmp/a\nb"} {
		if _, err := seatbeltPathRegex(path); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	pretendMacOS(t)
	if _, err := New(Config{Roots: []string{t.TempDir()}, UnixSockets: []string{`/tmp/x")) (allow default`}}); err == nil {
		t.Error("New accepted an unquotable socket path")
	}
}

func TestProfileProtectsGitMetadataAndReadOnlyPaths(t *testing.T) {
	profile, params, err := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, WritableRoots: []string{"/w"}, ReadOnly: []string{"/w/app/AGENTS.md"}, ProtectedNames: DefaultProtectedNames}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		`(deny file-write* (subpath (param "READ_ONLY_0")))`,
		`(deny file-write* (regex #"^/[wW]/(.*/)?\.[gG][iI][tT]/[cC][oO][nN][fF][iI][gG]$"))`,
		`(deny file-write* (regex #"^/[wW]/(.*/)?\.[gG][iI][tT]/[hH][oO][oO][kK][sS](/|$)"))`,
		`(deny file-write-unlink (regex #"^/[wW]/(.*/)?\.[gG][iI][tT]$"))`,
		`(deny file-write* (regex #"^/[wW]/(.*/)?\.[vV][sS][cC][oO][dD][eE](/|$)"))`,
		`(deny file-write* (regex #"^/[wW]/(.*/)?\.[eE][nN][vV][rR][cC](/|$)"))`,
		`(allow file-write* (regex #"^/[wW]/(.*/)?[nN][oO][dD][eE]_[mM][oO][dD][uU][lL][eE][sS]/"))`,
	} {
		if !strings.Contains(profile, rule) {
			t.Errorf("missing %s in\n%s", rule, profile)
		}
	}
	if got := rootRule("file-write*", `/w/a"b`, ".git$", false, ""); got != `(deny file-write* (regex #"/\.[gG][iI][tT]$"))` {
		t.Errorf("an unquotable root must fall back to the stricter rule, got %q", got)
	}
	if got := rootRule("file-write*", "/w/my.app", "x$", true, ""); got != `(deny file-write* (require-all (vnode-type REGULAR-FILE) (regex #"^/[wW]/[mM][yY]\.[aA][pP][pP]/(.*/)?[xX]$")))` {
		t.Errorf("got %q", got)
	}
	// The dependency-folder allow must not reopen git metadata, denied
	// paths, or read-only ones: those come after it.
	allowAt := strings.Index(profile, "(allow file-write* (regex")
	for _, later := range []string{`\.[gG][iI][tT]/[hH][oO][oO][kK][sS]`, `(param "READ_ONLY_0")`} {
		if i := strings.Index(profile, later); i < allowAt {
			t.Errorf("%s comes before the dependency allow", later)
		}
	}
	if !slices.Contains(params, [2]string{"PROTECTED_ANCESTOR_0", "/w/app"}) {
		t.Errorf("a read-only path's parent can still be renamed: %v", params)
	}
	readOnly, _, err := SeatbeltProfile(Policy{Mode: ModeReadOnly}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readOnly, `\.git`) {
		t.Error("read-only mode needs no git rules")
	}
}

func TestValidateRoot(t *testing.T) {
	home := "/Users/u"
	for root, ok := range map[string]bool{
		"/":                                      false,
		"/Users":                                 false,
		"/Users/u":                               false,
		"/Users/u/Library":                       false,
		"/Users/u/code/app":                      true,
		"/Users/u/Library/Application Support/X": true,
		"/private/tmp/x":                         true,
	} {
		if err := ValidateRoot(root, home); (err == nil) != ok {
			t.Errorf("ValidateRoot(%q) = %v", root, err)
		}
	}
}

func TestNewRejectsTheHomeDirectoryAsARoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	pretendMacOS(t)
	if _, err := New(Config{Roots: []string{home}}); err == nil {
		t.Fatal("a sandbox accepted the home directory as a writable root")
	}
	if _, err := New(Config{Mode: "full-access", Roots: []string{home}}); err != nil {
		t.Fatalf("without a sandbox there is nothing to protect: %v", err)
	}
}

func TestDefaultCachesLeaveDirectlyRunCodeOut(t *testing.T) {
	caches := DefaultWritableCaches("/h")
	for _, bad := range []string{"/h/Library/pnpm", "/h/.cache", "/h/Library/Caches/ms-playwright"} {
		if slices.Contains(caches, bad) {
			t.Errorf("%s is writable", bad)
		}
	}
	if !slices.Contains(caches, "/h/Library/pnpm/store") {
		t.Error("pnpm's store should stay writable")
	}
}

// The caller's protected names join the defaults in the profile; names the
// profile's regex cannot hold are refused.
func TestProtectedNamesReachTheProfile(t *testing.T) {
	names := append(append([]string(nil), DefaultProtectedNames...), "AGENTS.md")
	profile, _, err := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, WritableRoots: []string{"/w"}, ProtectedNames: names}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{caselessPattern(protectedNamePattern("AGENTS.md")), caselessPattern(protectedNamePattern(".vscode"))} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile lacks %s", want)
		}
	}
	without, _, err := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, WritableRoots: []string{"/w"}, ProtectedNames: DefaultProtectedNames}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without, caselessPattern(protectedNamePattern("AGENTS.md"))) {
		t.Error("an application's file is protected by default")
	}
	for _, bad := range []string{"", "..", "a/b", `x")(allow default`, "a*"} {
		if _, _, err := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, WritableRoots: []string{"/w"}, ProtectedNames: []string{bad}}, nil); err == nil {
			t.Errorf("accepted protected name %q", bad)
		}
	}
}
