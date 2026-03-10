package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// RetryConfig holds retry settings.
type RetryConfig struct {
	Enabled         bool    `yaml:"enabled"`
	MaxRetries      int     `yaml:"max_retries"`
	InitialDelay    float64 `yaml:"initial_delay"`
	MaxDelay        float64 `yaml:"max_delay"`
	ExponentialBase float64 `yaml:"exponential_base"`
}

// LLMConfig holds LLM API settings.
type LLMConfig struct {
	APIKey   string      `yaml:"api_key"`
	APIBase  string      `yaml:"api_base"`
	Model    string      `yaml:"model"`
	Provider string      `yaml:"provider"`
	Retry    RetryConfig `yaml:"retry"`
}

// AgentConfig holds agent settings.
type AgentConfig struct {
	MaxSteps         int    `yaml:"max_steps"`
	WorkspaceDir     string `yaml:"workspace_dir"`
	SystemPromptPath string `yaml:"system_prompt_path"`
	TokenLimit       int    `yaml:"token_limit"` // trigger summarization when exceeded (default 80000)
}

// MCPConfig holds MCP timeout settings.
type MCPConfig struct {
	ConnectTimeout   float64 `yaml:"connect_timeout"`
	ExecuteTimeout  float64 `yaml:"execute_timeout"`
	SSEReadTimeout  float64 `yaml:"sse_read_timeout"`
}

// ToolsConfig holds tool feature flags and paths.
type ToolsConfig struct {
	EnableFileTools bool      `yaml:"enable_file_tools"`
	EnableBash      bool      `yaml:"enable_bash"`
	EnableNote      bool      `yaml:"enable_note"`
	EnableSkills    bool      `yaml:"enable_skills"`
	SkillsDir       string    `yaml:"skills_dir"`
	EnableMCP       bool      `yaml:"enable_mcp"`
	MCPConfigPath   string    `yaml:"mcp_config_path"`
	MCP             MCPConfig `yaml:"mcp"`
}

// Config is the root configuration.
type Config struct {
	LLM    LLMConfig    `yaml:"-"`
	Agent  AgentConfig  `yaml:"-"`
	Tools  ToolsConfig  `yaml:"-"`
}

// rawConfig is used to parse YAML with flat and nested keys.
type rawConfig struct {
	APIKey   string `yaml:"api_key"`
	APIBase  string `yaml:"api_base"`
	Model    string `yaml:"model"`
	Provider string `yaml:"provider"`
	Retry    struct {
		Enabled         bool    `yaml:"enabled"`
		MaxRetries      int     `yaml:"max_retries"`
		InitialDelay    float64 `yaml:"initial_delay"`
		MaxDelay        float64 `yaml:"max_delay"`
		ExponentialBase float64 `yaml:"exponential_base"`
	} `yaml:"retry"`
	MaxSteps         int    `yaml:"max_steps"`
	WorkspaceDir     string `yaml:"workspace_dir"`
	SystemPromptPath string `yaml:"system_prompt_path"`
	TokenLimit       int    `yaml:"token_limit"`
	Tools            struct {
		EnableFileTools bool   `yaml:"enable_file_tools"`
		EnableBash      bool   `yaml:"enable_bash"`
		EnableNote      bool   `yaml:"enable_note"`
		EnableSkills    bool   `yaml:"enable_skills"`
		SkillsDir       string `yaml:"skills_dir"`
		EnableMCP       bool   `yaml:"enable_mcp"`
		MCPConfigPath   string `yaml:"mcp_config_path"`
		MCP             struct {
			ConnectTimeout  float64 `yaml:"connect_timeout"`
			ExecuteTimeout  float64 `yaml:"execute_timeout"`
			SSEReadTimeout  float64 `yaml:"sse_read_timeout"`
		} `yaml:"mcp"`
	} `yaml:"tools"`
}

// GetDefaultConfigPath returns the path to config.yaml using priority search.
// 1) ./config/config.yaml (cwd)
// 2) ~/.diegoc-agent/config/config.yaml
// 3) executable dir/config/config.yaml
func GetDefaultConfigPath() string {
	cwd, _ := os.Getwd()
	p1 := filepath.Join(cwd, "config", "config.yaml")
	if pathExists(p1) {
		return p1
	}
	home, _ := os.UserHomeDir()
	p2 := filepath.Join(home, ".diegoc-agent", "config", "config.yaml")
	if pathExists(p2) {
		return p2
	}
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	p3 := filepath.Join(exeDir, "config", "config.yaml")
	if pathExists(p3) {
		return p3
	}
	return p2
}

// FindConfigFile looks for a file named filename in the same priority order.
func FindConfigFile(filename string) string {
	cwd, _ := os.Getwd()
	p1 := filepath.Join(cwd, "config", filename)
	if pathExists(p1) {
		return p1
	}
	home, _ := os.UserHomeDir()
	p2 := filepath.Join(home, ".diegoc-agent", "config", filename)
	if pathExists(p2) {
		return p2
	}
	exe, _ := os.Executable()
	p3 := filepath.Join(filepath.Dir(exe), "config", filename)
	if pathExists(p3) {
		return p3
	}
	return ""
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Load reads config from the default path.
func Load() (*Config, error) {
	path := GetDefaultConfigPath()
	return FromYAML(path)
}

// FromYAML reads and validates config from a YAML file.
func FromYAML(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("configuration file not found: %s", path)
		}
		return nil, err
	}
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("invalid configuration format: %w", err)
	}
	if raw.APIKey == "" || raw.APIKey == "YOUR_API_KEY_HERE" {
		return nil, errors.New("please configure a valid API key in config.yaml")
	}
	// Defaults
	if raw.APIBase == "" {
		raw.APIBase = "https://api.minimax.io"
	}
	if raw.Model == "" {
		raw.Model = "MiniMax-M2.5"
	}
	if raw.Provider == "" {
		raw.Provider = "anthropic"
	}
	if raw.MaxSteps == 0 {
		raw.MaxSteps = 50
	}
	if raw.WorkspaceDir == "" {
		raw.WorkspaceDir = "./workspace"
	}
	if raw.SystemPromptPath == "" {
		raw.SystemPromptPath = "system_prompt.md"
	}
	if raw.TokenLimit <= 0 {
		raw.TokenLimit = 80000
	}
	if raw.Retry.InitialDelay == 0 {
		raw.Retry.InitialDelay = 1.0
	}
	if raw.Retry.MaxDelay == 0 {
		raw.Retry.MaxDelay = 60.0
	}
	if raw.Retry.ExponentialBase == 0 {
		raw.Retry.ExponentialBase = 2.0
	}
	if raw.Tools.SkillsDir == "" {
		raw.Tools.SkillsDir = "./skills"
	}
	if raw.Tools.MCPConfigPath == "" {
		raw.Tools.MCPConfigPath = "mcp.json"
	}
	if raw.Tools.MCP.ConnectTimeout == 0 {
		raw.Tools.MCP.ConnectTimeout = 10.0
	}
	if raw.Tools.MCP.ExecuteTimeout == 0 {
		raw.Tools.MCP.ExecuteTimeout = 60.0
	}
	if raw.Tools.MCP.SSEReadTimeout == 0 {
		raw.Tools.MCP.SSEReadTimeout = 120.0
	}
	if raw.Retry.MaxRetries == 0 {
		raw.Retry.MaxRetries = 3
	}

	cfg := &Config{
		LLM: LLMConfig{
			APIKey:   raw.APIKey,
			APIBase:  raw.APIBase,
			Model:    raw.Model,
			Provider: raw.Provider,
			Retry: RetryConfig{
				Enabled:         raw.Retry.Enabled,
				MaxRetries:      raw.Retry.MaxRetries,
				InitialDelay:    raw.Retry.InitialDelay,
				MaxDelay:        raw.Retry.MaxDelay,
				ExponentialBase: raw.Retry.ExponentialBase,
			},
		},
		Agent: AgentConfig{
			MaxSteps:         raw.MaxSteps,
			WorkspaceDir:     raw.WorkspaceDir,
			SystemPromptPath: raw.SystemPromptPath,
			TokenLimit:       raw.TokenLimit,
		},
		Tools: ToolsConfig{
			EnableFileTools: raw.Tools.EnableFileTools,
			EnableBash:      raw.Tools.EnableBash,
			EnableNote:      raw.Tools.EnableNote,
			EnableSkills:    raw.Tools.EnableSkills,
			SkillsDir:       raw.Tools.SkillsDir,
			EnableMCP:       raw.Tools.EnableMCP,
			MCPConfigPath:   raw.Tools.MCPConfigPath,
			MCP: MCPConfig{
				ConnectTimeout:  raw.Tools.MCP.ConnectTimeout,
				ExecuteTimeout:  raw.Tools.MCP.ExecuteTimeout,
				SSEReadTimeout:  raw.Tools.MCP.SSEReadTimeout,
			},
		},
	}
	return cfg, nil
}
