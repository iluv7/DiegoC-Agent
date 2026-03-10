package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"diegoc-agent/internal/schema"
)

// AgentLogger records each agent run to ~/.diegoc-agent/log/ (one file per run).
type AgentLogger struct {
	logDir   string
	logFile  *os.File
	logIndex int
}

// New creates an AgentLogger. Log directory is ~/.diegoc-agent/log/.
func New() *AgentLogger {
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, ".diegoc-agent", "log")
	_ = os.MkdirAll(logDir, 0755)
	return &AgentLogger{logDir: logDir}
}

// StartNewRun creates a new log file agent_run_YYYYMMDD_HHMMSS.log and writes the header.
func (l *AgentLogger) StartNewRun() {
	if l.logFile != nil {
		l.logFile.Close()
		l.logFile = nil
	}
	timestamp := time.Now().Format("20060102_150405")
	name := fmt.Sprintf("agent_run_%s.log", timestamp)
	path := filepath.Join(l.logDir, name)
	f, err := os.Create(path)
	if err != nil {
		return
	}
	l.logFile = f
	l.logIndex = 0
	header := "===============================================================================\n"
	header += fmt.Sprintf("Agent Run Log - %s\n", time.Now().Format("2006-01-02 15:04:05"))
	header += "===============================================================================\n\n"
	f.WriteString(header)
}

// LogRequest appends a REQUEST block: messages + tool names as JSON.
func (l *AgentLogger) LogRequest(messages []schema.Message, toolNames []string) {
	if l.logFile == nil {
		return
	}
	l.logIndex++
	requestData := map[string]interface{}{
		"messages": messages,
		"tools":    toolNames,
	}
	body, _ := json.MarshalIndent(requestData, "", "  ")
	content := "LLM Request:\n\n" + string(body)
	l.writeBlock("REQUEST", content)
}

// LogResponse appends a RESPONSE block: content, thinking, tool_calls, finish_reason.
func (l *AgentLogger) LogResponse(content string, thinking string, toolCalls []schema.ToolCall, finishReason string) {
	if l.logFile == nil {
		return
	}
	l.logIndex++
	responseData := map[string]interface{}{"content": content}
	if thinking != "" {
		responseData["thinking"] = thinking
	}
	if len(toolCalls) > 0 {
		responseData["tool_calls"] = toolCalls
	}
	if finishReason != "" {
		responseData["finish_reason"] = finishReason
	}
	body, _ := json.MarshalIndent(responseData, "", "  ")
	logContent := "LLM Response:\n\n" + string(body)
	l.writeBlock("RESPONSE", logContent)
}

// LogToolResult appends a TOOL_RESULT block.
func (l *AgentLogger) LogToolResult(toolName string, arguments map[string]interface{}, success bool, resultContent, resultError string) {
	if l.logFile == nil {
		return
	}
	l.logIndex++
	toolResultData := map[string]interface{}{
		"tool_name": toolName,
		"arguments": arguments,
		"success":   success,
	}
	if success {
		toolResultData["result"] = resultContent
	} else {
		toolResultData["error"] = resultError
	}
	body, _ := json.MarshalIndent(toolResultData, "", "  ")
	content := "Tool Execution:\n\n" + string(body)
	l.writeBlock("TOOL_RESULT", content)
}

func (l *AgentLogger) writeBlock(logType, content string) {
	if l.logFile == nil {
		return
	}
	sep := "--------------------------------------------------------------------------------\n"
	ts := time.Now().Format("2006-01-02 15:04:05.000")[:23]
	block := "\n" + sep
	block += fmt.Sprintf("[%d] %s\n", l.logIndex, logType)
	block += fmt.Sprintf("Timestamp: %s\n", ts)
	block += sep
	block += content + "\n"
	l.logFile.WriteString(block)
}

// GetLogFilePath returns the path of the current log file, or empty string if none.
func (l *AgentLogger) GetLogFilePath() string {
	if l.logFile == nil {
		return ""
	}
	return l.logFile.Name()
}

// LogDir returns the log directory (~/.diegoc-agent/log).
func (l *AgentLogger) LogDir() string {
	return l.logDir
}
