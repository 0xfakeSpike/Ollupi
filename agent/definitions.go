package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Definition is one subagent definition, in Claude Code's format: a
// Markdown file <name>.md in the workspace agents directory, whose YAML
// frontmatter has name and description (when to use the agent), and
// optionally tools (comma-separated registry tool names) and model; the body
// is the agent's instructions.
//
//	---
//	name: researcher
//	description: Researches one question on the web and reports with sources.
//	tools: opencli_google_search, opencli_web_read
//	model: inherit
//	---
//	You research one question ...
type Definition struct {
	Name        string
	Description string
	// Tools are the registry tools the subagent may call. Nil means the main
	// agent's tools.
	Tools []string
	// Model is the model name; empty means the main agent's.
	Model        string
	Instructions string
}

// Subagent is what the main agent is told about one subagent it can
// launch.
type Subagent struct {
	Name        string
	Description string
}

var definitionName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// LoadDefinitions reads every <name>.md in dir, sorted by name. Other files
// are ignored. A missing directory holds no definitions.
func LoadDefinitions(dir string) ([]Definition, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agents: %w", err)
	}
	var defs []Definition
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("agents: %w", err)
		}
		d, err := ParseDefinition(string(raw))
		if err != nil {
			return nil, fmt.Errorf("agents: %s: %w", path, err)
		}
		if want := strings.TrimSuffix(e.Name(), ".md"); d.Name != want {
			return nil, fmt.Errorf("agents: %s: name %q must match the file name %q", path, d.Name, want)
		}
		defs = append(defs, d)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs, nil
}

// ParseDefinition parses one definition file. Unknown frontmatter fields
// are errors.
func ParseDefinition(text string) (Definition, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Definition{}, fmt.Errorf("YAML frontmatter is required")
	}
	front, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		if front, ok = strings.CutSuffix(rest, "\n---"); !ok {
			return Definition{}, fmt.Errorf("unterminated YAML frontmatter")
		}
		body = ""
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Tools       string `yaml:"tools"`
		Model       string `yaml:"model"`
	}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(front)))
	dec.KnownFields(true)
	if err := dec.Decode(&fm); err != nil && !errors.Is(err, io.EOF) {
		return Definition{}, fmt.Errorf("frontmatter: %w", err)
	}
	d := Definition{
		Name:         strings.TrimSpace(fm.Name),
		Description:  strings.TrimSpace(fm.Description),
		Model:        strings.TrimSpace(fm.Model),
		Instructions: strings.TrimSpace(body),
	}
	if !definitionName.MatchString(d.Name) {
		return Definition{}, fmt.Errorf("name %q must be lowercase letters, digits, and hyphens, starting with a letter", d.Name)
	}
	if d.Description == "" {
		return Definition{}, fmt.Errorf("agent %s: description is required; it tells the main agent when to use it", d.Name)
	}
	if d.Instructions == "" {
		return Definition{}, fmt.Errorf("agent %s: the instructions (the text after the frontmatter) are required", d.Name)
	}
	if d.Model == "inherit" {
		d.Model = ""
	}
	if strings.TrimSpace(fm.Tools) != "" {
		d.Tools = []string{}
		for _, name := range strings.Split(fm.Tools, ",") {
			if name = strings.TrimSpace(name); name == "" {
				return Definition{}, fmt.Errorf("agent %s: tools has an empty entry", d.Name)
			}
			d.Tools = append(d.Tools, name)
		}
	}
	return d, nil
}

// Agent is the subagent defined by d, launched by main: it inherits the
// main agent's model settings, approval policy, and permission rules, and
// its model and tools unless d sets them. A subagent launches no subagents.
func (d Definition) Agent(main Agent) Agent {
	a := Agent{
		Name:           d.Name,
		Instructions:   d.Instructions,
		Model:          main.Model,
		ModelProfileID: main.ModelProfileID,
		ModelSettings:  main.ModelSettings,
		InheritModel:   d.Model == "",
		Tools:          main.Tools,
		ApprovalPolicy: main.ApprovalPolicy,
		Permissions:    main.Permissions,
	}
	if d.Model != "" {
		a.Model = d.Model
	}
	if d.Tools != nil {
		a.Tools = d.Tools
	}
	return a
}
