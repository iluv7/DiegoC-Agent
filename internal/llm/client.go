package llm

import (
	"context"
	"strings"

	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// Client is the LLM client interface.
type Client interface {
	Generate(ctx context.Context, messages []schema.Message, toolList []tools.Tool) (*schema.LLMResponse, error)
}

// MiniMax domains that get automatic path suffix.
var minimaxDomains = []string{"api.minimaxi.com", "api.minimax.io"}

func normaliseAPIBase(apiBase string, provider schema.LLMProvider) string {
	apiBase = strings.TrimSuffix(apiBase, "/")
	apiBase = strings.TrimSuffix(apiBase, "/anthropic")
	apiBase = strings.TrimSuffix(apiBase, "/v1")
	isMinimax := false
	for _, d := range minimaxDomains {
		if strings.Contains(apiBase, d) {
			isMinimax = true
			break
		}
	}
	if !isMinimax {
		return apiBase
	}
	switch provider {
	case schema.ProviderAnthropic:
		return apiBase + "/anthropic"
	case schema.ProviderOpenAI:
		return apiBase + "/v1"
	default:
		return apiBase + "/anthropic"
	}
}

// NewClient returns an LLM client for the given provider.
func NewClient(apiKey, apiBase, model string, provider schema.LLMProvider, retry RetryConfig) (Client, error) {
	base := normaliseAPIBase(apiBase, provider)
	switch provider {
	case schema.ProviderAnthropic:
		return NewAnthropicClient(apiKey, base, model, retry), nil
	case schema.ProviderOpenAI:
		return NewOpenAIClient(apiKey, base, model, retry), nil
	default:
		return NewAnthropicClient(apiKey, base, model, retry), nil
	}
}
