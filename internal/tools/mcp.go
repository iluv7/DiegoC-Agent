package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"diegoc-agent/internal/permission"
)

// MCPTimeoutConfig holds timeouts for MCP operations.
type MCPTimeoutConfig struct {
	ConnectTimeout float64
	ExecuteTimeout float64
	SSEReadTimeout float64
}

// ConnectionType represents the type of MCP connection
type ConnectionType string

const (
	ConnectionTypeStdio          ConnectionType = "stdio"
	ConnectionTypeSSE            ConnectionType = "sse"
	ConnectionTypeHTTP           ConnectionType = "http"
	ConnectionTypeStreamableHTTP ConnectionType = "streamable_http"
)

// MCPServerConfig represents configuration for a single MCP server
type MCPServerConfig struct {
	Description string            `json:"description"`
	Type        string            `json:"type"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Disabled    bool              `json:"disabled"`
	// Per-server timeout overrides
	ConnectTimeout *float64 `json:"connect_timeout"`
	ExecuteTimeout *float64 `json:"execute_timeout"`
	SSEReadTimeout *float64 `json:"sse_read_timeout"`
}

// MCPConfig represents the mcp.json configuration file
type MCPConfig struct {
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
}

// MCPConnection manages a connection to a single MCP server
type MCPConnection struct {
	name           string
	connectionType ConnectionType
	config         MCPServerConfig
	timeoutConfig  MCPTimeoutConfig

	// MCP client session
	session *mcp.ClientSession

	// Tools loaded from this server
	tools []Tool

	// Lifecycle
	mu     sync.Mutex
	closed bool
}

// MCPTool wraps an MCP tool as a Tool interface implementation
type MCPTool struct {
	name        string
	description string
	parameters  map[string]interface{}
	conn        *MCPConnection
}

// Name returns the tool name
func (t *MCPTool) Name() string {
	return t.name
}

// Description returns the tool description
func (t *MCPTool) Description() string {
	return t.description
}

// Parameters returns the tool's JSON schema
func (t *MCPTool) Parameters() map[string]interface{} {
	return t.parameters
}

// Execute calls the MCP tool via the connection
func (t *MCPTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	// Apply execute timeout
	timeout := time.Duration(t.conn.timeoutConfig.ExecuteTimeout * float64(time.Second))
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Call the tool via MCP client
	result, err := t.conn.callTool(execCtx, t.name, args)
	if err != nil {
		return &ToolResult{
			Success: false,
			Error:   fmt.Sprintf("MCP tool execution failed: %v", err),
		}, nil
	}

	return result, nil
}

// —— 权限 & 元数据 (HITL) ——

func (t *MCPTool) CheckPermissions(args map[string]interface{}, pCtx *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorPASSTHROUGH} // MCP 工具交给引擎处理
}
func (t *MCPTool) IsConcurrencySafe() bool { return true }
func (t *MCPTool) IsReadOnly() bool        { return false }
func (t *MCPTool) IsExternalTool() bool    { return false }

// Connect establishes connection to the MCP server
func (c *MCPConnection) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("connection already closed")
	}

	// Apply connect timeout
	timeout := time.Duration(c.timeoutConfig.ConnectTimeout * float64(time.Second))
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch c.connectionType {
	case ConnectionTypeStdio:
		return c.connectStdio(connectCtx)
	case ConnectionTypeSSE, ConnectionTypeHTTP, ConnectionTypeStreamableHTTP:
		return fmt.Errorf("connection type %s not yet implemented (only STDIO supported in Phase 6)", c.connectionType)
	default:
		return fmt.Errorf("unknown connection type: %s", c.connectionType)
	}
}

// connectStdio establishes a STDIO connection to an MCP server
func (c *MCPConnection) connectStdio(ctx context.Context) error {
	// Prepare command with environment variables
	cmd := exec.CommandContext(ctx, c.config.Command, c.config.Args...)
	cmd.Env = os.Environ()
	for k, v := range c.config.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	// Create MCP transport
	transport := &mcp.CommandTransport{
		Command:           cmd,
		TerminateDuration: 5 * time.Second,
	}

	// Create MCP client
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "diegoc-agent",
		Version: "0.1.0",
	}, nil)

	// Connect
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("MCP connection failed: %w", err)
	}
	c.session = session

	// List available tools
	toolsResp, err := session.ListTools(ctx, nil)
	if err != nil {
		c.cleanup()
		return fmt.Errorf("failed to list tools: %w", err)
	}

	// Wrap each tool
	for _, tool := range toolsResp.Tools {
		// Convert InputSchema to map[string]interface{}
		inputSchema := make(map[string]interface{})
		if tool.InputSchema != nil {
			// tool.InputSchema is already a jsonschema.Schema, convert it
			schemaBytes, _ := json.Marshal(tool.InputSchema)
			json.Unmarshal(schemaBytes, &inputSchema)
		}

		mcpTool := &MCPTool{
			name:        tool.Name,
			description: tool.Description,
			parameters:  inputSchema,
			conn:        c,
		}
		c.tools = append(c.tools, mcpTool)
	}

	fmt.Printf("✓ Connected to MCP server '%s' (stdio: %s) - loaded %d tools\n",
		c.name, c.config.Command, len(c.tools))
	for _, tool := range c.tools {
		desc := tool.Description()
		if len(desc) > 60 {
			desc = desc[:60] + "..."
		}
		fmt.Printf("  - %s: %s\n", tool.Name(), desc)
	}

	return nil
}

// callTool invokes an MCP tool by name with arguments
func (c *MCPConnection) callTool(ctx context.Context, name string, args map[string]interface{}) (*ToolResult, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("connection closed")
	}
	session := c.session
	c.mu.Unlock()

	if session == nil {
		return nil, fmt.Errorf("no active session")
	}

	// Call tool via MCP client
	resp, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}

	// Extract content from response
	var contentParts []string
	for _, item := range resp.Content {
		// Check content type and extract text
		if textContent, ok := item.(*mcp.TextContent); ok {
			contentParts = append(contentParts, textContent.Text)
		}
	}

	content := ""
	if len(contentParts) > 0 {
		for _, part := range contentParts {
			content += part
		}
	}

	// Check for errors in response
	isError := resp.IsError
	if isError {
		return &ToolResult{
			Success: false,
			Error:   content,
		}, nil
	}

	return &ToolResult{
		Success: true,
		Content: content,
	}, nil
}

// Close gracefully shuts down the MCP connection
func (c *MCPConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	return c.cleanup()
}

// cleanup closes resources (must be called with lock held or during error recovery)
func (c *MCPConnection) cleanup() error {
	if c.session != nil {
		return c.session.Close()
	}
	return nil
}

// Global registry of active MCP connections
var (
	mcpConnections []*MCPConnection
	mcpMutex       sync.Mutex
)

// LoadMCPTools reads mcp.json and loads tools from configured servers.
// Supports STDIO, SSE, HTTP, and Streamable HTTP connections with timeout control.
func LoadMCPTools(configPath string, timeoutCfg MCPTimeoutConfig) ([]Tool, error) {
	// Read config file
	data, err := os.ReadFile(configPath)
	if err != nil {
		// Config file not found is not an error - just return empty tools
		return nil, nil
	}

	var config MCPConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse mcp.json: %w", err)
	}

	if len(config.MCPServers) == 0 {
		fmt.Println("No MCP servers configured")
		return nil, nil
	}

	var allTools []Tool
	ctx := context.Background()

	// Connect to each enabled server
	for serverName, serverConfig := range config.MCPServers {
		if serverConfig.Disabled {
			fmt.Printf("Skipping disabled MCP server: %s\n", serverName)
			continue
		}

		// Determine connection type
		connType := determineConnectionType(serverConfig)

		// Apply per-server timeout overrides
		effectiveTimeout := timeoutCfg
		if serverConfig.ConnectTimeout != nil {
			effectiveTimeout.ConnectTimeout = *serverConfig.ConnectTimeout
		}
		if serverConfig.ExecuteTimeout != nil {
			effectiveTimeout.ExecuteTimeout = *serverConfig.ExecuteTimeout
		}
		if serverConfig.SSEReadTimeout != nil {
			effectiveTimeout.SSEReadTimeout = *serverConfig.SSEReadTimeout
		}

		// Validate configuration
		if connType == ConnectionTypeStdio && serverConfig.Command == "" {
			fmt.Printf("✗ No command specified for STDIO server: %s\n", serverName)
			continue
		}
		if connType != ConnectionTypeStdio && serverConfig.URL == "" {
			fmt.Printf("✗ No URL specified for %s server: %s\n", connType, serverName)
			continue
		}

		// Create connection
		conn := &MCPConnection{
			name:           serverName,
			connectionType: connType,
			config:         serverConfig,
			timeoutConfig:  effectiveTimeout,
		}

		// Attempt to connect
		if err := conn.Connect(ctx); err != nil {
			fmt.Printf("✗ Failed to connect to MCP server '%s': %v\n", serverName, err)
			continue
		}

		// Register connection for cleanup
		mcpMutex.Lock()
		mcpConnections = append(mcpConnections, conn)
		mcpMutex.Unlock()

		// Add tools
		allTools = append(allTools, conn.tools...)
	}

	if len(allTools) > 0 {
		fmt.Printf("\n✓ Total MCP tools loaded: %d\n\n", len(allTools))
	}

	return allTools, nil
}

// determineConnectionType infers the connection type from server config
func determineConnectionType(config MCPServerConfig) ConnectionType {
	// Explicit type specified
	if config.Type != "" {
		switch config.Type {
		case "stdio":
			return ConnectionTypeStdio
		case "sse":
			return ConnectionTypeSSE
		case "http":
			return ConnectionTypeHTTP
		case "streamable_http":
			return ConnectionTypeStreamableHTTP
		}
	}

	// Auto-detect: if URL exists, default to streamable_http; otherwise stdio
	if config.URL != "" {
		return ConnectionTypeStreamableHTTP
	}
	return ConnectionTypeStdio
}

// CleanupMCPConnections gracefully shuts down all MCP connections
func CleanupMCPConnections() {
	mcpMutex.Lock()
	defer mcpMutex.Unlock()

	for _, conn := range mcpConnections {
		if err := conn.Close(); err != nil {
			fmt.Printf("Warning: error closing MCP connection '%s': %v\n", conn.name, err)
		}
	}
	mcpConnections = nil
}
