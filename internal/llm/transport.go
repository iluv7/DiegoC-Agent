package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"diegoc-agent/internal/schema"
)

// ClientOption configures the transport shared by both protocol adapters.
type ClientOption func(*httpTransport)

// WithHTTPClient supplies the HTTP client used for requests. A nil client keeps
// the default client and its 120-second timeout. Configure clients before use.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(t *httpTransport) {
		if client != nil {
			t.client = client
		}
	}
}

// APIError retains a non-200 response for inspection with errors.As, including
// through RetryExhaustedError. Its text preserves the existing CLI error format.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error %d: %s", e.StatusCode, e.Body)
}

type httpTransport struct {
	client *http.Client
	retry  RetryConfig
}

func newHTTPTransport(retry RetryConfig, options ...ClientOption) *httpTransport {
	t := &httpTransport{client: &http.Client{Timeout: 120 * time.Second}, retry: retry}
	for _, option := range options {
		if option != nil {
			option(t)
		}
	}
	return t
}

// generate shares request/retry mechanics, leaving protocol decoding with the
// adapter. Parsing remains inside the retry boundary, as in the original clients.
func (t *httpTransport) generate(ctx context.Context, url string, headers http.Header, body any, parse func([]byte) (*schema.LLMResponse, error)) (*schema.LLMResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode LLM request: %w", err)
	}
	var response *schema.LLMResponse
	err = DoWithRetry(ctx, t.retry, func() error {
		data, err := t.postJSON(ctx, url, headers, raw)
		if err != nil {
			return err
		}
		response, err = parse(data)
		return err
	}, nil)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func (t *httpTransport) postJSON(ctx context.Context, url string, headers http.Header, raw []byte) ([]byte, error) {
	// Each attempt gets a fresh request/body so a retry sends the full payload.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(data)}
	}
	return data, nil
}
