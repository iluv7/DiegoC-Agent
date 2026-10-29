package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/config"
	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// application owns the components assembled for one CLI process.
type application struct {
	agent        *agent.Agent
	workspaceDir string
	logger       *logger.AgentLogger
	memory       *memory.Manager
}

func (app *application) Close() {
	if app.memory != nil {
		app.memory.Close()
	}
}

// buildApplication wires clients, tools, project instructions and optional memory.
func buildApplication(cfg *config.Config, workspaceOverride string) (*application, error) {
	provider := schema.ProviderAnthropic
	if cfg.LLM.Provider == "openai" {
		provider = schema.ProviderOpenAI
	}

	retry := llm.RetryConfig{
		Enabled:         cfg.LLM.Retry.Enabled,
		MaxRetries:      cfg.LLM.Retry.MaxRetries,
		InitialDelay:    time.Duration(cfg.LLM.Retry.InitialDelay * float64(time.Second)),
		MaxDelay:        time.Duration(cfg.LLM.Retry.MaxDelay * float64(time.Second)),
		ExponentialBase: cfg.LLM.Retry.ExponentialBase,
	}

	client, err := llm.NewClient(cfg.LLM.APIKey, cfg.LLM.APIBase, cfg.LLM.Model, provider, retry)
	if err != nil {
		return nil, fmt.Errorf("LLM client error: %w", err)
	}

	// Default fallback aligned with Mini-Agent: when no system_prompt file is found
	systemPrompt := "You are DiegoC Agent, an intelligent assistant powered by MiniMax that can help users complete various tasks."
	if p := config.FindConfigFile(cfg.Agent.SystemPromptPath); p != "" {
		if b, e := os.ReadFile(p); e == nil {
			systemPrompt = string(b)
		}
	}

	workspaceDir := cfg.Agent.WorkspaceDir
	if workspaceOverride != "" {
		workspaceDir = workspaceOverride
	}
	workspaceDir, err = filepath.Abs(expandHome(workspaceDir))
	if err != nil {
		return nil, fmt.Errorf("Workspace error: %w", err)
	}
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		return nil, fmt.Errorf("Workspace error: %w", err)
	}

	toolList, skillLoader := buildTools(cfg, workspaceDir)
	var skillsMetadata string
	if skillLoader != nil {
		skillsMetadata = skillLoader.GetSkillsMetadataPrompt()
	}
	systemPrompt, instructionsPath, err := buildSystemPrompt(systemPrompt, workspaceDir, skillsMetadata)
	if err != nil {
		return nil, fmt.Errorf("Project instructions error: %w", err)
	}
	if instructionsPath != "" {
		fmt.Printf("Loaded project instructions: %s\n", selectorText(instructionsPath, 10000))
	}

	// Phase 11-13: Memory pipeline (optional, opt-in via config)
	var memManager *memory.Manager
	if cfg.Memory.Enabled {
		memCfg := memory.Config{
			WorkingDir:      cfg.Memory.WorkingDir,
			MaxInputLength:  cfg.Memory.MaxInputLength,
			CompactRatio:    cfg.Memory.CompactRatio,
			CompactReserve:  cfg.Memory.CompactReserve,
			ToolResultKeepN: cfg.Memory.ToolResultKeepN,
			Language:        cfg.Memory.Language,
			RetentionDays:   cfg.Memory.RetentionDays,
		}
		memManager = memory.NewManager(memCfg, client)
		if memManager != nil {
			// Phase 9-10: FileStore (ChromaDB + FTS5) + FileWatcher + memory_search tool.
			if fs := buildMemoryFileStore(cfg, memCfg.WorkingDir); fs != nil {
				memManager.SetFileStore(fs)
				if searchTool := memManager.SearchTool(); searchTool != nil {
					toolList = append(toolList, searchTool)
				}
				_ = memManager.Start(context.Background())
			}
		}
	}

	ag := agent.New(client, systemPrompt, cfg.Agent.MaxSteps, cfg.Agent.TokenLimit, toolList)
	goalClient, err := llm.NewClient(cfg.LLM.APIKey, cfg.LLM.APIBase, cfg.Goal.Model, provider, retry)
	if err != nil {
		return nil, fmt.Errorf("Goal evaluator client error: %w", err)
	}
	ag.ConfigureGoals(goalClient, cfg.Goal.Model, cfg.Goal.MaxChecks)
	agentLogger := logger.New()
	ag.Logger = agentLogger

	if memManager != nil {
		ag.SetMemoryManager(memManager)
	}

	return &application{agent: ag, workspaceDir: workspaceDir, logger: agentLogger, memory: memManager}, nil
}

func buildTools(cfg *config.Config, workspaceDir string) ([]tools.Tool, *tools.SkillLoader) {
	list := append([]tools.Tool{&tools.AskUserTool{}}, tools.LoopTools()...)
	var skillLoader *tools.SkillLoader

	if cfg.Tools.EnableFileTools {
		list = append(list,
			&tools.ReadTool{WorkspaceDir: workspaceDir},
			&tools.WriteTool{WorkspaceDir: workspaceDir},
			&tools.EditTool{WorkspaceDir: workspaceDir},
		)
	}
	list = append(list, &tools.WritePlanTool{WorkspaceDir: workspaceDir})
	if cfg.Tools.EnableBash {
		list = append(list,
			&tools.BashTool{WorkspaceDir: workspaceDir},
			&tools.BashOutputTool{},
			&tools.BashKillTool{},
		)
	}
	if cfg.Tools.EnableNote {
		memoryFile := filepath.Join(workspaceDir, ".agent_memory.json")
		list = append(list,
			&tools.RecordNoteTool{MemoryFile: memoryFile},
			&tools.RecallNoteTool{MemoryFile: memoryFile},
		)
	}
	if cfg.Tools.EnableSkills {
		skillsDir := resolveSkillsDir(cfg.Tools.SkillsDir)
		loader := tools.NewSkillLoader(skillsDir)
		skills := loader.DiscoverSkills()
		if len(skills) > 0 {
			list = append(list, &tools.GetSkillTool{Loader: loader})
			skillLoader = loader
		}
	}
	if cfg.Tools.EnableMCP {
		mcpPath := config.FindConfigFile(cfg.Tools.MCPConfigPath)
		if mcpPath != "" {
			mcpTools, err := tools.LoadMCPTools(mcpPath, tools.MCPTimeoutConfig{
				ConnectTimeout: cfg.Tools.MCP.ConnectTimeout,
				ExecuteTimeout: cfg.Tools.MCP.ExecuteTimeout,
				SSEReadTimeout: cfg.Tools.MCP.SSEReadTimeout,
			})
			if err == nil && len(mcpTools) > 0 {
				list = append(list, mcpTools...)
			}
		}
	}
	return list, skillLoader
}

// buildMemoryFileStore constructs the Phase 9-10 memory index stack:
// embedding client + ChromaDB vector store + SQLite FTS5 keyword store.
// Returns nil if any component fails to initialize (memory pipeline
// still works without index/search).
func buildMemoryFileStore(cfg *config.Config, workingDir string) *memory.FileStore {
	embedder := memory.NewOpenAIEmbeddingClient(cfg.LLM.APIKey, cfg.LLM.APIBase, cfg.Memory.EmbeddingModel)
	chroma := memory.NewChromaClient(cfg.Memory.ChromaEndpoint)

	fileStoreDir := filepath.Join(workingDir, "file_store")
	if err := os.MkdirAll(fileStoreDir, 0755); err != nil {
		fmt.Printf("✗ Failed to create file_store dir: %v\n", err)
		return nil
	}
	kwStore, err := memory.NewFTS5Store(filepath.Join(fileStoreDir, "fts5_index.db"))
	if err != nil {
		fmt.Printf("✗ Failed to open FTS5 index: %v\n", err)
		return nil
	}

	return memory.NewFileStore(
		embedder,
		chroma,
		kwStore,
		memory.NewRuleTokenCounter(),
		"diego_memory",
		400, // chunkTokens
		80,  // overlap
	)
}

func resolveSkillsDir(configured string) string {
	abs := filepath.Clean(configured)
	if filepath.IsAbs(abs) {
		return abs
	}
	cwd, _ := os.Getwd()
	p := filepath.Join(cwd, abs)
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		return p
	}
	return p
}

func expandHome(p string) string {
	if len(p) >= 2 && p[:2] == "~/" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func printConfigHelp() {
	fmt.Println()
	fmt.Println("Configuration search order:")
	fmt.Println("  1) ./config/config.yaml (development)")
	fmt.Println("  2) ~/.diegoc-agent/config/config.yaml (user)")
	fmt.Println("  3) <executable dir>/config/config.yaml")
	fmt.Println()
	fmt.Println("Quick setup:")
	fmt.Println("  mkdir -p ~/.diegoc-agent/config")
	fmt.Println("  cp config/config-example.yaml ~/.diegoc-agent/config/config.yaml")
	fmt.Println("  # Edit ~/.diegoc-agent/config/config.yaml and add your API key")
	fmt.Println()
}
