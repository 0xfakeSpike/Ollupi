package sandbox

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Seatbelt logs every denial to the unified log as "Sandbox: proc(pid)
// deny(1) operation target", followed by the rule's (with message ...)
// text. Each command's profile tags its deny rules with a per-command tag
// that ends in a per-process suffix; one `log stream` filtered on the suffix
// collects them, and the tag attributes each denial to its command. This is
// how sandbox-runtime (Claude Code) reports violations, and it tells the
// model that a command failed because of the sandbox rather than leaving it
// to guess from "Operation not permitted".

const (
	// maxDenials caps the denials kept for one command.
	maxDenials = 20
	// denialWait bounds how long a failed command waits for its denials to
	// arrive; the log stream delivers them shortly after they happen.
	denialWait = 500 * time.Millisecond
	// denialQuiet ends the wait early once this long passes without a new one.
	denialQuiet = 200 * time.Millisecond
)

// logExecutable is macOS's log tool; a variable so tests can stand one in.
var logExecutable = "/usr/bin/log"

// denialLog streams this process's Seatbelt denials and sorts them by tag.
type denialLog struct {
	suffix string
	tagRe  *regexp.Regexp

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.Closer
	alive   bool
	pending map[string]*denialSet
}

type denialSet struct {
	lines []string
	seen  map[string]bool
	last  time.Time
}

func newDenialLog() *denialLog {
	suffix := randomHex(5) + "SBX"
	return &denialLog{
		suffix:  suffix,
		tagRe:   regexp.MustCompile(`OSBX([0-9a-f]{16})_` + suffix),
		pending: map[string]*denialSet{},
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("sandbox: random: %v", err))
	}
	return hex.EncodeToString(b)
}

// start runs `log stream`. Without it commands still run; they only lose
// the denial report. Streaming needs an administrator account; for other
// users `log` exits at once and denials go unreported.
//
// `log stream` runs under a shell that watches its stdin, a pipe only this
// process holds: when the app exits, even by a crash, the pipe closes, `cat`
// returns, and the shell kills the stream. macOS has no parent-death
// signal, so without this a crashed app would leave one running per launch.
func (l *denialLog) start() error {
	cmd := exec.Command("/bin/sh", "-c", `"$0" stream --style compact --predicate "$1" </dev/null & p=$!; cat >/dev/null; kill $p 2>/dev/null; wait $p`,
		logExecutable, fmt.Sprintf(`eventMessage CONTAINS "%s"`, l.suffix))
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	l.mu.Lock()
	l.cmd, l.stdin, l.alive = cmd, stdin, true
	l.mu.Unlock()
	go func() {
		l.consume(bufio.NewScanner(out))
		_ = cmd.Wait()
		l.mu.Lock()
		l.alive = false
		l.mu.Unlock()
	}()
	return nil
}

// running reports whether denials are being collected.
func (l *denialLog) running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.alive
}

// close ends the stream: closing stdin makes the shell kill it.
func (l *denialLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stdin != nil {
		_ = l.stdin.Close()
		l.stdin = nil
	}
	l.cmd = nil
}

// newTag registers a command and returns the tag its profile carries.
func (l *denialLog) newTag() (id, tag string) {
	id = randomHex(8)
	l.mu.Lock()
	l.pending[id] = &denialSet{seen: map[string]bool{}}
	l.mu.Unlock()
	return id, "OSBX" + id + "_" + l.suffix
}

// collect returns and forgets the denials of id. A failed command waits
// briefly for denials still in flight; a successful one takes what came.
func (l *denialLog) collect(id string, wait bool) []string {
	if wait {
		deadline := time.Now().Add(denialWait)
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			l.mu.Lock()
			set := l.pending[id]
			quiet := set != nil && len(set.lines) > 0 && time.Since(set.last) >= denialQuiet
			l.mu.Unlock()
			if quiet {
				break
			}
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	set := l.pending[id]
	delete(l.pending, id)
	if set == nil {
		return nil
	}
	return set.lines
}

// consume parses `log stream --style compact` output. The stream carries
// only this process's tagged denials. A denial's tag ends its message, on
// the same line or a later one, so a denial waits for the next tag.
func (l *denialLog) consume(sc *bufio.Scanner) {
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var pending []string
	for sc.Scan() {
		line := sc.Text()
		detail, isDenial := denialDetail(line)
		if isDenial && detail != "" {
			pending = append(pending, detail)
		} else if isDenial {
			// A denial not worth reporting still ends what came before it.
			pending = pending[:0]
		}
		if id := l.tagID(line); id != "" {
			for _, d := range pending {
				l.record(id, d)
			}
			pending = pending[:0]
		}
		if len(pending) > maxDenials {
			pending = pending[len(pending)-maxDenials:]
		}
	}
}

func (l *denialLog) tagID(line string) string {
	if m := l.tagRe.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

func (l *denialLog) record(id, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	set := l.pending[id]
	if set == nil || set.seen[detail] || len(set.lines) >= maxDenials {
		return
	}
	set.seen[detail] = true
	set.lines = append(set.lines, detail)
	set.last = time.Now()
}

var denialLine = regexp.MustCompile(`Sandbox: (.+?)\(\d+\) deny\(\d+\) (\S+)\s*(.*)$`)

// denialDetail turns a Seatbelt log line into "process: operation target".
// ok reports a denial line at all; detail is empty for denials that do not
// explain a failed command: Mach and sysctl lookups are denied all the time
// without breaking anything, and name resolution with the network off.
func denialDetail(line string) (detail string, ok bool) {
	m := denialLine.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	op, target := m[2], m[3]
	switch {
	case strings.HasPrefix(op, "file-"), strings.HasPrefix(op, "network-"), strings.HasPrefix(op, "process-exec"):
	default:
		return "", true
	}
	if strings.Contains(target, "mDNSResponder") {
		return "", true
	}
	// The tag, when it ends the line, is not part of the target.
	if i := strings.Index(target, "OSBX"); i >= 0 {
		target = target[:i]
	}
	return strings.TrimSpace(m[1] + ": " + op + " " + strings.TrimSpace(target)), true
}
