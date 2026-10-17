package sandbox

import (
	"fmt"
	"sort"
	"strings"
)

// baseEnv is what most programs need to run and nothing more. The host's
// API keys and tokens stay out unless a config names them in passthrough.
var baseEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TERM", "LANG", "TZ"}

// Environ builds a command's environment: the base variables and the named
// passthrough variables copied from host, then extra, which wins.
func Environ(host []string, passthrough []string, extra map[string]string) []string {
	allowed := map[string]bool{}
	for _, k := range append(append([]string(nil), baseEnv...), passthrough...) {
		allowed[k] = true
	}
	values := map[string]string{}
	for _, pair := range host {
		k, v, ok := strings.Cut(pair, "=")
		if ok && (allowed[k] || strings.HasPrefix(k, "LC_")) {
			values[k] = v
		}
	}
	for k, v := range extra {
		values[k] = v
	}
	out := make([]string, 0, len(values))
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// ValidateEnvNames rejects names that cannot be environment variables.
func ValidateEnvNames(names []string) error {
	for _, k := range names {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("sandbox: invalid environment variable name %q", k)
		}
	}
	return nil
}
