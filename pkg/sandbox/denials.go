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

const (
	maxDenials  = 20
	denialWait  = 500 * time.Millisecond
	denialQuiet = 200 * time.Millisecond
)

var logExecutable = "/usr/bin/log"

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

func (l *denialLog) running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.alive
}

func (l *denialLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stdin != nil {
		_ = l.stdin.Close()
		l.stdin = nil
	}
	l.cmd = nil
}

func (l *denialLog) newTag() (id, tag string) {
	id = randomHex(8)
	l.mu.Lock()
	l.pending[id] = &denialSet{seen: map[string]bool{}}
	l.mu.Unlock()
	return id, "OSBX" + id + "_" + l.suffix
}

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

func (l *denialLog) consume(sc *bufio.Scanner) {
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var pending []string
	for sc.Scan() {
		line := sc.Text()
		detail, isDenial := denialDetail(line)
		if isDenial && detail != "" {
			pending = append(pending, detail)
		} else if isDenial {
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

	if i := strings.Index(target, "OSBX"); i >= 0 {
		target = target[:i]
	}
	return strings.TrimSpace(m[1] + ": " + op + " " + strings.TrimSpace(target)), true
}
