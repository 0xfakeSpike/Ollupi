package skills

import (
	"context"
	"fmt"
	"strings"

	"github.com/0xfakeSpike/ollupi/agent/tools"
)

const (
	ProviderName      = "skills"
	ToolGetRootDir    = "get_skills_root_dir"
	ToolListSkills    = "list_skills"
	ToolActivateSkill = "activate_skill"
	ToolReadResource  = "read_skill_resource"
)

func (c *Catalog) Name() (string, string) {
	return ProviderName, "Directory-based Agent Skills with explicit activation and on-demand resources."
}

func (c *Catalog) Invoke(_ context.Context, tool string, arguments map[string]any) (tools.Result, error) {
	switch strings.TrimSpace(tool) {
	case ToolGetRootDir:
		return tools.Result{Output: map[string]any{"skills_root_dir": c.root}}, nil
	case ToolListSkills:
		return tools.Result{Output: map[string]any{"skills": c.All()}}, nil
	case ToolActivateSkill:
		return c.activate(arguments)
	case ToolReadResource:
		return c.readResource(arguments)
	default:
		err := fmt.Errorf("skills: unknown tool %q", tool)
		return tools.Result{Err: err}, err
	}
}

func (c *Catalog) ToolList(context.Context) ([]*tools.Spec, error) {
	names := c.Names()

	read := &tools.Annotations{ReadOnlyHint: true, OpenWorldHint: tools.Hint(false)}
	return []*tools.Spec{
		{
			Name:        ToolGetRootDir,
			Annotations: read,
			Description: "Return the workspace Agent Skills root directory.",
			InputSchema: emptyObjectSchema(),
		},
		{
			Name:        ToolListSkills,
			Annotations: read,
			Description: "List available Agent Skills and their supporting resource files.",
			InputSchema: emptyObjectSchema(),
		},
		{
			Name:        ToolActivateSkill,
			Annotations: read,
			Description: "Activate one exact Agent Skill. Its complete SKILL.md instructions remain active for the conversation.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"enum":        names,
						"description": "Exact skill name from AVAILABLE SKILLS.",
					},
				},
				"required":             []string{"name"},
				"additionalProperties": false,
			},
		},
		{
			Name:        ToolReadResource,
			Annotations: read,
			Description: "Read a supporting UTF-8 resource from an activated Agent Skill. Use only paths listed for that skill.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"skill": map[string]any{"type": "string", "enum": names},
					"path": map[string]any{
						"type":        "string",
						"description": "Resource path relative to the skill directory, as listed in AVAILABLE SKILLS.",
					},
					"offset_runes": map[string]any{"type": "integer", "minimum": 0},
					"limit_runes":  map[string]any{"type": "integer", "minimum": 1, "maximum": MaxResourceRunes},
				},
				"required":             []string{"skill", "path"},
				"additionalProperties": false,
			},
		},
	}, nil
}

func (c *Catalog) activate(arguments map[string]any) (tools.Result, error) {
	name, _ := arguments["name"].(string)
	skill, ok := c.Get(strings.TrimSpace(name))
	if !ok {
		err := fmt.Errorf("skills: no skill named %q", strings.TrimSpace(name))
		return tools.Result{Err: err}, err
	}
	return tools.Result{
		Output: map[string]any{
			"activated":   skill.Name,
			"description": skill.Description,
			"version":     skill.Version,
			"digest":      skill.Digest,
			"resources":   skill.Resources,
		},
		Artifacts: []tools.ContextArtifact{{
			Kind:    tools.ArtifactAgentSkill,
			Name:    skill.Name,
			Content: skill.Instructions,
			Source:  skill.SourcePath,
			Digest:  skill.Digest,
		}},
	}, nil
}

func (c *Catalog) readResource(arguments map[string]any) (tools.Result, error) {
	skillName, _ := arguments["skill"].(string)
	path, _ := arguments["path"].(string)
	offset, err := integerArgument(arguments, "offset_runes", 0)
	if err != nil {
		return tools.Result{Err: err}, err
	}
	limit, err := integerArgument(arguments, "limit_runes", DefaultResourceRunes)
	if err != nil {
		return tools.Result{Err: err}, err
	}
	page, err := c.ReadResource(skillName, path, offset, limit)
	if err != nil {
		return tools.Result{Err: err}, err
	}
	return tools.Result{Output: page}, nil
}

func emptyObjectSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

func integerArgument(arguments map[string]any, key string, fallback int) (int, error) {
	value, ok := arguments[key]
	if !ok || value == nil {
		return fallback, nil
	}
	switch n := value.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("skills: %s must be an integer", key)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("skills: %s must be an integer", key)
	}
}
