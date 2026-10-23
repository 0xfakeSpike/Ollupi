package shell

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/0xfakeSpike/ollupi/pkg/sandbox"
)

type Config struct {
	WorkspaceDirs        []string `json:"workspace_dirs"`
	CommandTimeoutSec    int      `json:"command_timeout_sec"`
	MaxCommandTimeoutSec int      `json:"max_command_timeout_sec,omitempty"`
	CommandBlacklist     []string `json:"command_blacklist"`
	SandboxMode          string   `json:"sandbox_mode,omitempty"`
	Network              bool     `json:"network,omitempty"`
	AllowedDomains       []string `json:"allowed_domains,omitempty"`
	DeniedDomains        []string `json:"denied_domains,omitempty"`
	DenyRead             []string `json:"deny_read,omitempty"`
	ReadOnly             []string `json:"read_only,omitempty"`
	ExtraWritable        []string `json:"extra_writable,omitempty"`
	UnixSockets          []string `json:"unix_sockets,omitempty"`
	EnvPassthrough       []string `json:"env_passthrough,omitempty"`
	MaxOutputBytes       int      `json:"max_output_bytes,omitempty"`
}

func (c *Config) Normalize(base string) error {
	if len(c.WorkspaceDirs) == 0 {
		if base == "" {
			return fmt.Errorf("workspace_dirs: at least one command directory is required")
		}
		c.WorkspaceDirs = []string{base}
	} else {
		resolved := make([]string, 0, len(c.WorkspaceDirs))
		for _, entry := range c.WorkspaceDirs {
			if entry == "" {
				return fmt.Errorf("workspace_dirs contains empty path")
			}
			if !filepath.IsAbs(entry) && base != "" {
				entry = filepath.Join(base, entry)
			}
			resolved = append(resolved, filepath.Clean(entry))
		}
		c.WorkspaceDirs = resolved
	}
	if c.CommandTimeoutSec <= 0 {
		c.CommandTimeoutSec = 30
	}
	if c.MaxCommandTimeoutSec < 0 {
		return fmt.Errorf("max_command_timeout_sec must be >= 0")
	}
	if c.MaxCommandTimeoutSec == 0 {
		c.MaxCommandTimeoutSec = max(600, c.CommandTimeoutSec)
	}
	if c.MaxCommandTimeoutSec < c.CommandTimeoutSec {
		return fmt.Errorf("max_command_timeout_sec must be >= command_timeout_sec")
	}
	c.SandboxMode = strings.TrimSpace(c.SandboxMode)
	if _, err := sandbox.ResolveMode(c.SandboxMode); err != nil {
		return err
	}
	for _, list := range []*[]string{&c.AllowedDomains, &c.DeniedDomains} {
		parsed, err := sandbox.ParseDomains(*list)
		if err != nil {
			return err
		}
		if len(parsed) == 0 {
			parsed = nil
		}
		*list = parsed
	}
	if len(c.DeniedDomains) > 0 && len(c.AllowedDomains) == 0 {
		return fmt.Errorf("denied_domains needs allowed_domains")
	}
	if c.MaxOutputBytes < 0 {
		return fmt.Errorf("max_output_bytes must be >= 0")
	}
	if err := sandbox.ValidateEnvNames(c.EnvPassthrough); err != nil {
		return err
	}
	return nil
}

type AgentFiles struct {
	Home      string
	SkillsDir string
}

var AgentProtectedNames = []string{
	"AGENTS.md", ".agents", ".codex", ".claude", "CLAUDE.md", ".cursor", ".mcp.json",
}

const dataDir = "data"

func sandboxConfig(o Options, c Config) sandbox.Config {
	denyRead := append([]string(nil), o.DenyRead...)
	var readOnly []string
	if home := o.Agent.Home; home != "" {
		denyRead = append(denyRead, filepath.Join(home, dataDir))
	}
	for _, dir := range []string{o.Agent.SkillsDir} {
		if dir != "" {
			readOnly = append(readOnly, dir)
		}
	}
	return sandbox.Config{
		Mode:           c.SandboxMode,
		Network:        c.Network,
		Domains:        sandbox.DomainRules{Allowed: c.AllowedDomains, Denied: c.DeniedDomains},
		Roots:          c.WorkspaceDirs,
		DenyRead:       append(denyRead, c.DenyRead...),
		ReadOnly:       append(readOnly, c.ReadOnly...),
		ProtectedNames: AgentProtectedNames,
		EnvMarker:      o.EnvMarker,
		ExtraWritable:  c.ExtraWritable,
		UnixSockets:    c.UnixSockets,
		EnvPassthrough: c.EnvPassthrough,
		MaxOutputBytes: c.MaxOutputBytes,
	}
}
