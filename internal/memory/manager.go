package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/schema"
)

// Config holds memory pipeline configuration.
// Defined here (not in config package) to avoid circular imports.
type Config struct {
	WorkingDir      string // e.g. ".reme"
	MaxInputLength  int    // model max input tokens (default 128000)
	CompactRatio    float64 // threshold ratio before compaction triggers (default 0.7)
	CompactReserve  int    // tokens to reserve for recent context (default 10000)
	ToolResultKeepN int    // keep N most recent tool_results un-truncated (default 3)
	Language        string // "zh" or "" for en
	RetentionDays   int    // tool_result file retention (default 3)
}

// DefaultConfig returns a Config populated with sensible defaults.
func DefaultConfig() Config {
	return Config{
		WorkingDir:      ".reme",
		MaxInputLength:  128000,
		CompactRatio:    0.7,
		CompactReserve:  10000,
		ToolResultKeepN: 3,
		Language:        "zh",
		RetentionDays:   3,
	}
}

// Manager orchestrates the full memory pipeline:
//
//	compact_tool_result → check_context → compact_memory → summary_memory
//
// It is the single entry point that the Agent calls before each LLM request.
type Manager struct {
	cfg Config

	inMemory       *InMemoryMemory
	contextChecker *ContextChecker
	toolCompactor  *ToolResultCompactor
	compactor      *Compactor
	summarizer     *Summarizer
	msgHandler     *MsgHandler
	tokenCounter   TokenCounter

	fileStore   *FileStore   // Phase 9: memory file index + search
	fileWatcher *FileWatcher // Phase 9: watches memory dir for changes

	llmClient llm.Client
	mu        sync.Mutex
}

// NewManager creates a MemoryManager, wiring together all memory components.
// Returns nil if cfg.WorkingDir is empty (graceful degradation).
func NewManager(cfg Config, llmClient llm.Client) *Manager {
	if cfg.WorkingDir == "" {
		return nil
	}

	tc := NewRuleTokenCounter()
	handler := NewMsgHandler(tc)

	workingDir := cfg.WorkingDir
	toolResultDir := filepath.Join(workingDir, "tool_results")
	dialogDir := filepath.Join(workingDir, "dialog")
	memoryDir := filepath.Join(workingDir, "memory")

	// Ensure directories exist
	os.MkdirAll(toolResultDir, 0755)
	os.MkdirAll(dialogDir, 0755)
	os.MkdirAll(memoryDir, 0755)

	inMem := NewInMemoryMemory(dialogDir, tc)

	// ContextChecker threshold: trigger compaction when context exceeds
	// maxInputLength * compactRatio minus reserve for system + summary.
	compactThreshold := int(float64(cfg.MaxInputLength) * cfg.CompactRatio)

	cc := NewContextChecker(handler, compactThreshold, cfg.CompactReserve)
	trc := NewToolResultCompactor(toolResultDir, cfg.RetentionDays,
		DefaultOldMaxBytes, DefaultRecentMaxBytes, cfg.ToolResultKeepN)

	// Compactor threshold: max tokens to feed into the LLM for summarization.
	compactorThreshold := cfg.MaxInputLength / 2
	comp := NewCompactor(handler, compactorThreshold, cfg.Language, false)

	// Summarizer threshold: same as compactor.
	sum := NewSummarizer(handler, llmClient, compactorThreshold, cfg.Language, memoryDir)

	return &Manager{
		cfg:            cfg,
		inMemory:       inMem,
		contextChecker: cc,
		toolCompactor:  trc,
		compactor:      comp,
		summarizer:     sum,
		msgHandler:     handler,
		tokenCounter:   tc,
		llmClient:      llmClient,
	}
}

// PreReasoningHook runs the full memory pipeline before each LLM call.
//
// Pipeline:
//  1. Compact tool results (skip latest N)
//  2. Calculate effective threshold (accounting for system prompt + existing summary)
//  3. ContextCheck → split messages into toCompact / toKeep
//  4. If nothing to compact → return original messages unchanged
//  5. Async: write long-term memory via Summarizer
//  6. Sync: generate structured summary via Compactor
//  7. Prepend summary to kept messages, return
//
// Returns (messages, newSummary, error). The caller should replace its message
// list with the returned messages and store the summary for the next call.
func (m *Manager) PreReasoningHook(ctx context.Context, messages []schema.Message, systemPrompt string) ([]schema.Message, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(messages) <= 1 {
		// Only system prompt (or empty), nothing to compact.
		return messages, "", nil
	}

	// ---- Step 1: Compact tool results ----
	keepN := m.cfg.ToolResultKeepN
	splitIdx := len(messages) - keepN
	if splitIdx < 1 {
		splitIdx = 1 // always keep the system prompt
	}

	oldMsgs := make([]schema.Message, splitIdx)
	copy(oldMsgs, messages[:splitIdx])
	oldMsgs = m.toolCompactor.Compact(oldMsgs)

	// Rebuild: compacted old + untouched recent
	processed := make([]schema.Message, 0, len(messages))
	processed = append(processed, oldMsgs...)
	processed = append(processed, messages[splitIdx:]...)

	// ---- Step 2: Calculate effective threshold ----
	systemTokens := m.tokenCounter.CountMessages([]schema.Message{{Role: "system", Content: systemPrompt}})
	summaryTokens := m.tokenCounter.CountText(m.inMemory.CompressedSummary())
	effectiveThreshold := int(float64(m.cfg.MaxInputLength)*m.cfg.CompactRatio) - systemTokens - summaryTokens
	if effectiveThreshold < m.cfg.CompactReserve {
		effectiveThreshold = m.cfg.CompactReserve
	}

	// Update context checker's threshold for this call
	m.contextChecker.threshold = effectiveThreshold

	// ---- Step 3: Context check ----
	toCompact, toKeep, valid := m.contextChecker.Check(processed)
	if !valid || len(toCompact) == 0 {
		// Nothing to compact — return processed (with compacted tool results) unchanged.
		return processed, "", nil
	}

	// ---- Step 4: Async long-term memory ----
	// Fire-and-forget: Summarizer writes to memory/YYYY-MM-DD.md.
	compactCopy := make([]schema.Message, len(toCompact))
	copy(compactCopy, toCompact)
	go func() {
		_ = m.summarizer.Summarize(context.Background(), compactCopy)
	}()

	// ---- Step 5: Sync structured summary ----
	prevSummary := m.inMemory.CompressedSummary()
	newSummary := m.compactor.Compact(ctx, m.llmClient, toCompact, prevSummary)
	if newSummary != "" {
		m.inMemory.SetCompressedSummary(newSummary)
	}

	// ---- Step 6: Build final message list ----
	// Prepend summary as a system block (matching InMemoryMemory.GetMemory format).
	result := make([]schema.Message, 0, len(toKeep)+2)

	// system prompt is always first
	if len(toKeep) > 0 && toKeep[0].Role == "system" {
		result = append(result, toKeep[0])
		toKeep = toKeep[1:]
	}

	// Insert summary if available
	if newSummary != "" {
		result = append(result, schema.Message{
			Role:    "system",
			Content: "# Summary of previous conversation\n" + newSummary,
		})
	}

	result = append(result, toKeep...)
	return result, newSummary, nil
}

// CompressedSummary returns the current compressed summary.
// Useful for persisting/restoring state.
func (m *Manager) CompressedSummary() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inMemory.CompressedSummary()
}

// SetCompressedSummary restores a previously saved summary.
func (m *Manager) SetCompressedSummary(summary string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inMemory.SetCompressedSummary(summary)
}

// AddUserMessage appends a user message to the internal InMemoryMemory store.
// This is called by the Agent instead of directly appending to Messages.
func (m *Manager) AddUserMessage(content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inMemory.AddMessage(schema.Message{Role: "user", Content: content})
}

// AddMessage appends any message to the internal store.
func (m *Manager) AddMessage(msg schema.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inMemory.AddMessage(msg)
}

// SetFileStore injects a FileStore for memory indexing and search (Phase 9).
// When set, the Manager also creates a FileWatcher to automatically keep the
// index in sync with the memory directory.
func (m *Manager) SetFileStore(fs *FileStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fileStore = fs
	m.fileWatcher = NewFileWatcher(m.memoryDir(), fs)
}

// Start begins background operations (currently: the FileWatcher polling loop).
// Must be called after SetFileStore if indexing/search is desired.
// Safe to call even if no FileStore is configured — it is a no-op in that case.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	fw := m.fileWatcher
	m.mu.Unlock()

	if fw != nil {
		return fw.Start(ctx)
	}
	return nil
}

// SearchMemory performs a hybrid (vector + BM25 keyword) search over indexed
// memory files. Returns nil if no FileStore is configured.
func (m *Manager) SearchMemory(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	m.mu.Lock()
	fs := m.fileStore
	m.mu.Unlock()

	if fs == nil {
		return nil, nil
	}
	return fs.HybridSearchWithDate(ctx, query, limit)
}

// memoryDir returns the memory directory path derived from WorkingDir.
func (m *Manager) memoryDir() string {
	return filepath.Join(m.cfg.WorkingDir, "memory")
}

// Close performs cleanup: stops the FileWatcher and removes expired tool_result files.
func (m *Manager) Close() error {
	m.mu.Lock()
	fw := m.fileWatcher
	m.mu.Unlock()

	// Stop file watcher outside the lock to avoid deadlock with poll goroutine.
	if fw != nil {
		fw.Stop()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	deleted := m.toolCompactor.CleanupExpiredFiles()
	if deleted > 0 {
		_ = deleted
	}
	return nil
}

// WorkingDir returns the memory working directory.
func (m *Manager) WorkingDir() string {
	return m.cfg.WorkingDir
}

// EstimateTokens estimates token usage of the current message list.
func (m *Manager) EstimateTokens(messages []schema.Message) (used int, free int) {
	used = m.tokenCounter.CountMessages(messages)
	free = m.cfg.MaxInputLength - used
	if free < 0 {
		free = 0
	}
	return
}

// InMemory returns the underlying InMemoryMemory for direct access
// (e.g., for loading dialog from disk).
func (m *Manager) InMemory() *InMemoryMemory {
	return m.inMemory
}

// ensureDir creates a directory if it doesn't exist. Exported for testing.
func ensureDir(path string) error {
	return os.MkdirAll(path, 0755)
}

// formatMemSize returns a human-readable representation (used in debug/logging).
func formatMemSize(bytes int) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
