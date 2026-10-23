package llm

import (
	"fmt"
	"strings"
)

// Limits is how many tokens one request to a model may use. Context is the
// model's window, prompt and reply together; Output is kept free for the
// reply, so a prompt may take Input() tokens.
type Limits struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

func (l Limits) Input() int { return l.Context - l.Output }

func (l Limits) check() error {
	if l.Context <= 0 || l.Output <= 0 || l.Output >= l.Context {
		return fmt.Errorf("context %d and output %d must be positive, with output below context", l.Context, l.Output)
	}
	return nil
}

// knownLimits are the published limits of models this library is used with.
// A key matches a model name that equals it or continues it after a dash
// ("gpt-4o" matches "gpt-4o-mini" and "gpt-4o-2024-08-06", not "gpt-4o1");
// the longest matching key wins. Output is the most a reply may take, which
// keeps the input side safe whatever reply length the provider defaults to.
var knownLimits = map[string]Limits{
	"gpt-4o":       {Context: 128_000, Output: 16_384},
	"gpt-4.1":      {Context: 1_047_576, Output: 32_768},
	"o3":           {Context: 200_000, Output: 100_000},
	"o4-mini":      {Context: 200_000, Output: 100_000},
	"gpt-5":        {Context: 400_000, Output: 128_000},
	"gpt-5-chat":   {Context: 128_000, Output: 16_384},
	"gpt-5-pro":    {Context: 400_000, Output: 272_000},
	"gpt-5.1":      {Context: 400_000, Output: 128_000},
	"gpt-5.1-chat": {Context: 128_000, Output: 16_384},
	"gpt-5.2":      {Context: 400_000, Output: 128_000},
	"gpt-5.2-chat": {Context: 128_000, Output: 16_384},

	"deepseek-v4-pro": {Context: 1_048_576, Output: 393_216},
	"deepseek-flash":  {Context: 1_048_576, Output: 393_216},

	"doubao-seed-1-6": {Context: 256_000, Output: 32_000},

	"claude-opus-4":     {Context: 200_000, Output: 32_000},
	"claude-sonnet-4":   {Context: 200_000, Output: 64_000},
	"claude-opus-4-5":   {Context: 200_000, Output: 64_000},
	"claude-sonnet-4-5": {Context: 200_000, Output: 64_000},
	"claude-haiku-4-5":  {Context: 200_000, Output: 64_000},
	"claude-opus-5-5":   {Context: 1_000_000, Output: 128_000},
	"claude-sonnet-5-5": {Context: 1_000_000, Output: 128_000},

	"gemini-2.5-pro":   {Context: 1_048_576 + 65_536, Output: 65_536},
	"gemini-2.5-flash": {Context: 1_048_576 + 65_536, Output: 65_536},
}

// Limits returns the limits of a model on this connection: the ones the
// connection was given for that exact name first, then the known ones. A model
// with neither is an error, not a guess.
func (c *OpenAI) Limits(model string) (Limits, error) {
	if l, ok := c.limits[model]; ok {
		if err := l.check(); err != nil {
			return Limits{}, fmt.Errorf("llm: limits of model %q: %w", model, err)
		}
		return l, nil
	}
	if l, ok := knownLimitsOf(model); ok {
		return l, nil
	}
	return Limits{}, fmt.Errorf("llm: the context limits of model %q are unknown; declare them on its connection", model)
}

func knownLimitsOf(model string) (Limits, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	best, found := "", false
	for key := range knownLimits {
		if (name == key || strings.HasPrefix(name, key+"-")) && len(key) > len(best) {
			best, found = key, true
		}
	}
	return knownLimits[best], found
}
