package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	MaxDescriptionRunes   = 1024
	MaxInstructionRunes   = 20000
	MaxInstructionLines   = 500
	DefaultResourceRunes  = 12000
	MaxResourceRunes      = 30000
	MaxReadableResourceB  = 1 << 20
	maxListedResources    = 200
	skillInstructionsFile = "SKILL.md"
)

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type Skill struct {
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	SourceFile   string     `json:"source_file"`
	Version      string     `json:"version"`
	Resources    []Resource `json:"resources"`
	AllowedTools []string   `json:"allowed_tools"`
	ByteSize     int64      `json:"byte_size"`
	RootDir      string     `json:"-"`
	SourcePath   string     `json:"-"`
	Instructions string     `json:"-"`
	Digest       string     `json:"-"`
}

type Resource struct {
	Path     string `json:"path"`
	ByteSize int64  `json:"byte_size"`
}

type ResourcePage struct {
	Skill           string `json:"skill"`
	Path            string `json:"path"`
	Text            string `json:"text"`
	OffsetRunes     int    `json:"offset_runes"`
	LimitRunes      int    `json:"limit_runes"`
	TotalRunes      int    `json:"total_runes"`
	NextOffsetRunes int    `json:"next_offset_runes"`
	EOF             bool   `json:"eof"`
}

type Catalog struct {
	root   string
	byName map[string]Skill
}

func New(root string) (*Catalog, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("skills: root directory is required")
	}
	byName, err := scanRoot(filepath.Clean(root))
	if err != nil {
		return nil, err
	}
	return &Catalog{root: filepath.Clean(root), byName: byName}, nil
}

func (c *Catalog) Names() []string {
	names := make([]string, 0, len(c.byName))
	for name := range c.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (c *Catalog) All() []Skill {
	names := c.Names()
	out := make([]Skill, 0, len(names))
	for _, name := range names {
		out = append(out, cloneSkill(c.byName[name]))
	}
	return out
}

func (c *Catalog) Get(name string) (Skill, bool) {
	skill, ok := c.byName[strings.TrimSpace(name)]
	return cloneSkill(skill), ok
}

func (c *Catalog) ReadResource(skillName, resourcePath string, offset, limit int) (ResourcePage, error) {
	skill, ok := c.Get(skillName)
	if !ok {
		return ResourcePage{}, fmt.Errorf("skills: no skill named %q", strings.TrimSpace(skillName))
	}
	resourcePath = filepath.ToSlash(strings.TrimSpace(resourcePath))
	if resourcePath == "" {
		return ResourcePage{}, fmt.Errorf("skills: resource path is required")
	}
	known := false
	for _, resource := range skill.Resources {
		if resource.Path == resourcePath {
			known = true
			break
		}
	}
	if !known {
		return ResourcePage{}, fmt.Errorf("skills: resource %q is not exposed by skill %q", resourcePath, skill.Name)
	}
	if offset < 0 {
		return ResourcePage{}, fmt.Errorf("skills: resource offset must be >= 0")
	}
	if limit <= 0 {
		limit = DefaultResourceRunes
	}
	if limit > MaxResourceRunes {
		return ResourcePage{}, fmt.Errorf("skills: resource limit must be <= %d", MaxResourceRunes)
	}

	fullPath, err := safeResourcePath(skill.RootDir, resourcePath)
	if err != nil {
		return ResourcePage{}, err
	}
	info, err := os.Lstat(fullPath)
	if err != nil {
		return ResourcePage{}, fmt.Errorf("skills: stat resource %q: %w", resourcePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ResourcePage{}, fmt.Errorf("skills: resource %q must be a regular non-symlink file", resourcePath)
	}
	if info.Size() > MaxReadableResourceB {
		return ResourcePage{}, fmt.Errorf("skills: resource %q exceeds %d bytes", resourcePath, MaxReadableResourceB)
	}
	raw, err := os.ReadFile(fullPath)
	if err != nil {
		return ResourcePage{}, fmt.Errorf("skills: read resource %q: %w", resourcePath, err)
	}
	if !utf8.Valid(raw) {
		return ResourcePage{}, fmt.Errorf("skills: resource %q is not valid UTF-8", resourcePath)
	}
	runes := []rune(string(raw))
	if offset > len(runes) {
		offset = len(runes)
	}
	end := offset + limit
	if end > len(runes) {
		end = len(runes)
	}
	return ResourcePage{
		Skill:           skill.Name,
		Path:            resourcePath,
		Text:            string(runes[offset:end]),
		OffsetRunes:     offset,
		LimitRunes:      limit,
		TotalRunes:      len(runes),
		NextOffsetRunes: end,
		EOF:             end >= len(runes),
	}, nil
}

type frontmatter struct {
	Name         string         `yaml:"name"`
	Description  string         `yaml:"description"`
	AllowedTools string         `yaml:"allowed-tools"`
	Metadata     map[string]any `yaml:"metadata"`
}

func scanRoot(root string) (map[string]Skill, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("skills: read root %q: %w", root, err)
	}
	loaded := make(map[string]Skill)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		skillDir := filepath.Join(root, entry.Name())
		sourcePath := filepath.Join(skillDir, skillInstructionsFile)
		info, err := os.Lstat(sourcePath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("skills: stat %q: %w", sourcePath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("skills: %q must be a regular non-symlink file", sourcePath)
		}
		skill, err := parseSkill(root, skillDir, sourcePath, info.Size())
		if err != nil {
			return nil, err
		}
		if skill.Name != entry.Name() {
			return nil, fmt.Errorf("skills: directory %q must match frontmatter name %q", entry.Name(), skill.Name)
		}
		if _, exists := loaded[skill.Name]; exists {
			return nil, fmt.Errorf("skills: duplicate skill name %q", skill.Name)
		}
		loaded[skill.Name] = skill
	}
	return loaded, nil
}

func parseSkill(root, skillDir, sourcePath string, byteSize int64) (Skill, error) {
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return Skill{}, fmt.Errorf("skills: read %q: %w", sourcePath, err)
	}
	if !utf8.Valid(raw) {
		return Skill{}, fmt.Errorf("skills: %q is not valid UTF-8", sourcePath)
	}
	fm, instructions, err := parseDocument(string(raw))
	if err != nil {
		return Skill{}, fmt.Errorf("skills: parse %q: %w", sourcePath, err)
	}
	name := strings.TrimSpace(fm.Name)
	if !skillNamePattern.MatchString(name) || len([]rune(name)) > 64 {
		return Skill{}, fmt.Errorf("invalid name %q; use 1-64 lowercase letters, numbers, and hyphens", name)
	}
	description := strings.TrimSpace(fm.Description)
	if description == "" {
		return Skill{}, fmt.Errorf("description is required")
	}
	if len([]rune(description)) > MaxDescriptionRunes {
		return Skill{}, fmt.Errorf("description exceeds %d runes", MaxDescriptionRunes)
	}
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return Skill{}, fmt.Errorf("instruction body is required")
	}
	if len([]rune(instructions)) > MaxInstructionRunes {
		return Skill{}, fmt.Errorf("instruction body exceeds %d runes; move details into references/", MaxInstructionRunes)
	}
	if lines := strings.Count(instructions, "\n") + 1; lines > MaxInstructionLines {
		return Skill{}, fmt.Errorf("instruction body has %d lines; maximum is %d", lines, MaxInstructionLines)
	}
	resources, err := scanResources(skillDir)
	if err != nil {
		return Skill{}, err
	}
	digestBytes := sha256.Sum256(raw)
	return Skill{
		Name:         name,
		Description:  description,
		AllowedTools: strings.Fields(fm.AllowedTools),
		Version:      version(fm.Metadata),
		RootDir:      skillDir,
		SourcePath:   sourcePath,
		SourceFile:   filepath.ToSlash(mustRelative(root, sourcePath)),
		Instructions: instructions,
		Digest:       hex.EncodeToString(digestBytes[:]),
		ByteSize:     byteSize,
		Resources:    resources,
	}, nil
}

func parseDocument(text string) (frontmatter, string, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 2 || lines[0] != "---" {
		return frontmatter{}, "", fmt.Errorf("YAML frontmatter is required")
	}
	closing := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			closing = i
			break
		}
	}
	if closing < 0 {
		return frontmatter{}, "", fmt.Errorf("unterminated YAML frontmatter")
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:closing], "\n")), &fm); err != nil {
		return frontmatter{}, "", err
	}
	return fm, strings.Join(lines[closing+1:], "\n"), nil
}

func scanResources(skillDir string) ([]Resource, error) {
	resources := make([]Resource, 0)
	err := filepath.WalkDir(skillDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == skillDir {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == skillInstructionsFile || !isReadableResource(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(skillDir, path)
		if err != nil {
			return err
		}
		resources = append(resources, Resource{Path: filepath.ToSlash(rel), ByteSize: info.Size()})
		if len(resources) > maxListedResources {
			return fmt.Errorf("skill %q exposes more than %d readable resources", skillDir, maxListedResources)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("skills: scan resources in %q: %w", skillDir, err)
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Path < resources[j].Path })
	return resources, nil
}

func isReadableResource(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".txt", ".json", ".yaml", ".yml", ".toml", ".go", ".py", ".js", ".ts", ".sh":
		return true
	default:
		return false
	}
}

func safeResourcePath(root, resourcePath string) (string, error) {
	if filepath.IsAbs(resourcePath) {
		return "", fmt.Errorf("skills: resource path must be relative")
	}
	clean := filepath.Clean(filepath.FromSlash(resourcePath))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("skills: resource path escapes the skill root")
	}
	fullPath := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, fullPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("skills: resource path escapes the skill root")
	}
	current := root
	for _, part := range strings.Split(clean, string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("skills: stat resource path component %q: %w", part, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("skills: resource path contains symlink component %q", part)
		}
	}
	return fullPath, nil
}

func mustRelative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.Base(path)
	}
	return rel
}

func cloneSkill(in Skill) Skill {
	in.AllowedTools = append([]string(nil), in.AllowedTools...)
	in.Resources = append([]Resource(nil), in.Resources...)
	return in
}

func version(metadata map[string]any) string {
	v, ok := metadata["version"]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}
