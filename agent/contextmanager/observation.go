package contextmanager

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
)

const compactTextMaxRunes = 16000

func compactText(r tools.Result) string {
	if r.Err != nil {
		return truncateRunes(r.Err.Error(), compactTextMaxRunes)
	}
	v := compactStructured(r.Output)
	if s, ok := v.(string); ok {
		return truncateRunes(s, compactTextMaxRunes)
	}

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
	head := max / 3
	tail := max - head
	return string(runes[:head]) + fmt.Sprintf("\n[... %d characters omitted ...]\n", len(runes)-max) + string(runes[len(runes)-tail:])
}

func callResult(it *repo.ItemRow, outputBudget int) (string, error) {
	switch {
	case it.Result != nil:
		return renderObservation(it, outputBudget)
	case it.Status == repo.ItemFailed || it.Status == repo.ItemDeclined:
		return "error: " + it.Error, nil
	default:
		return "error: the call did not finish", nil
	}
}

const (
	maxObservationTokens = 6000
	minOutputTokens      = 256
)

// renderObservation renders one tool result within budget. Its artifacts
// count against the budget too: the output text gets what they leave, and at
// least minOutputTokens. The artifacts themselves are never cut (a skill cut
// in half misleads); CheckArtifacts bounded them when the tool ran.
func renderObservation(it *repo.ItemRow, budget int) (string, error) {
	result, err := it.LiveResult()
	if err != nil {
		return "", fmt.Errorf("context: %w", err)
	}
	if result.Err != nil {
		return "error: " + clipText(result.Err.Error(), budget), nil
	}
	artifacts := renderArtifacts(result.Artifacts)
	text := strings.TrimSpace(compactText(result))
	if text == "" {
		text = "(no output)"
	} else {
		text = clipText(text, max(minOutputTokens, budget-llm.EstimateTokens(artifacts)))
	}
	if artifacts != "" {
		text += "\n\n" + artifacts
	}
	return text, nil
}

func clipText(s string, budget int) string {
	s = strings.TrimSpace(s)
	if s == "" || budget <= 0 || llm.EstimateTokens(s) <= budget {
		return s
	}
	runes := []rune(s)
	marker := func(n int) string { return fmt.Sprintf("\n[... %d characters omitted ...]\n", n) }

	lo, hi := 0, len(runes)/2
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if llm.EstimateTokens(string(runes[:mid])+marker(len(runes)-2*mid)+string(runes[len(runes)-mid:])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	head, tail := 2*lo/3, 2*lo-2*lo/3
	return strings.TrimSpace(string(runes[:head])) + marker(len(runes)-head-tail) + strings.TrimSpace(string(runes[len(runes)-tail:]))
}
