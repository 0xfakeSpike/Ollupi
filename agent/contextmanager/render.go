package contextmanager

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/0xfakeSpike/ollupi/tools"
)

// compactTextMaxRunes bounds the rendered output of one tool result.
const compactTextMaxRunes = 16000

// compactText renders a tool result's structured output for the model:
// decorative lines are collapsed and the text is bounded.
func compactText(r tools.Result) string {
	if r.Err != nil {
		return truncateRunes(r.Err.Error(), compactTextMaxRunes)
	}
	v := compactStructured(r.Output)
	if s, ok := v.(string); ok {
		return truncateRunes(s, compactTextMaxRunes)
	}
	// Outputs reach the model only after being recorded as JSON, so they
	// always marshal.
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("contextmanager: render output of %s: %v", r.Tool, err))
	}
	return truncateRunes(string(b), compactTextMaxRunes)
}

func compactStructured(v any) any {
	switch x := v.(type) {
	case string:
		return compactDecorativeLines(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			out[k] = compactStructured(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = compactStructured(x[i])
		}
		return out
	default:
		return v
	}
}

func compactDecorativeLines(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	skipping, first := false, true
	for line := range strings.SplitSeq(s, "\n") {
		if isDecorativeLine(line) {
			if !skipping {
				if !first {
					b.WriteByte('\n')
				}
				b.WriteString("...")
				first = false
				skipping = true
			}
			continue
		}
		skipping = false
		if !first {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		first = false
	}
	return b.String()
}

func isDecorativeLine(line string) bool {
	t := strings.TrimSpace(line)
	var runes, nonSpace, border int
	for _, r := range t {
		runes++
		if unicode.IsSpace(r) {
			continue
		}
		nonSpace++
		if (r >= 0x2500 && r <= 0x257f) || strings.ContainsRune("-=*_+|", r) {
			border++
		}
	}
	return runes >= 3 && nonSpace >= 3 && border*100 >= 90*nonSpace
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}
