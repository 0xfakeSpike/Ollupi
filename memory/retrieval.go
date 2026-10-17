package memory

import (
	"context"
	"sort"
	"strings"
	"unicode"
)

// Recall uses deterministic lexical matching, including Han bigrams, with
// durable preferences and decisions as fallback. It requires no vector service
// or optional SQLite build tags. SQL filters scopes before ranking.
func (m *Manager) Recall(ctx context.Context, workspaceID, query string, budget int) ([]Entry, error) {
	if !m.opts.Use || budget <= 0 {
		return []Entry{}, nil
	}
	entries, err := m.store.MemoryList(ctx, workspaceID, true)
	if err != nil {
		return nil, err
	}
	eligible := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Status == "active" && (e.WorkspaceID == "" || e.WorkspaceID == workspaceID) {
			eligible = append(eligible, e)
		}
	}
	out := []Entry{}
	// Leave room for the context wrapper and entry identifiers in the renderer.
	remaining := budget - 96
	for _, e := range rankEntries(eligible, query, true) {
		cost := estimateTokens(e.Text) + estimateTokens(e.Topic) + estimateTokens(e.ID) + estimateTokens(e.WorkspaceID) + 48
		if cost > remaining {
			continue
		}
		out = append(out, e)
		remaining -= cost
		if len(out) >= 20 {
			break
		}
	}
	return out, nil
}

func rankEntries(entries []Entry, query string, recall bool) []Entry {
	terms := searchTerms(query)
	type ranked struct {
		entry Entry
		score int
	}
	matches := make([]ranked, 0, len(entries))
	for _, e := range entries {
		text := strings.ToLower(e.Topic + " " + e.Text)
		score := 0
		for _, term := range terms {
			if strings.Contains(text, term) {
				score += 1 + min(8, len([]rune(term)))
			}
		}
		if e.Kind == "preference" {
			score += 12
		}
		if e.Kind == "decision" {
			score += 6
		}
		if !recall {
			// Exact source overlap is stronger than a coincidental word match.
			for _, s := range e.Sources {
				if s.ID != "" && strings.Contains(query, s.ID) {
					score += 100
				}
			}
			if e.Status == "forgotten" {
				score += 20
			}
		}
		if recall && score == 0 {
			continue
		}
		matches = append(matches, ranked{e, score})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if !matches[i].entry.UpdatedAt.Equal(matches[j].entry.UpdatedAt) {
			return matches[i].entry.UpdatedAt.After(matches[j].entry.UpdatedAt)
		}
		return matches[i].entry.ID < matches[j].entry.ID
	})
	out := make([]Entry, 0, len(matches))
	for _, r := range matches {
		out = append(out, r.entry)
	}
	return out
}

func searchTerms(text string) []string {
	seen := map[string]bool{}
	terms := []string{}
	add := func(s string) {
		if len([]rune(s)) >= 2 && !seen[s] && len(terms) < 512 {
			seen[s] = true
			terms = append(terms, s)
		}
	}
	var word []rune
	var han rune
	flush := func() {
		if len(word) > 0 {
			add(string(word))
			word = nil
		}
	}
	for _, r := range strings.ToLower(text) {
		if unicode.Is(unicode.Han, r) {
			flush()
			if han != 0 {
				add(string([]rune{han, r}))
			}
			han = r
			continue
		}
		han = 0
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			word = append(word, r)
		} else {
			flush()
		}
	}
	flush()
	return terms
}

func estimateTokens(text string) int {
	ascii, nonASCII := 0, 0
	for _, r := range text {
		if r <= unicode.MaxASCII {
			ascii++
		} else {
			nonASCII++
		}
	}
	return (ascii+3)/4 + nonASCII
}
