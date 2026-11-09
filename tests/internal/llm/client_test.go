package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testClient(t *testing.T, provider schema.LLMProvider, base string, retry RetryConfig, httpClient *http.Client) Client {
	t.Helper()
	client, err := NewClient("test-key", base, "test-model", provider, retry, WithHTTPClient(httpClient))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func minimalResponse(provider schema.LLMProvider) string {
	if provider == schema.ProviderOpenAI {
		return `{"choices":[{"message":{"content":"ok"}}]}`
	}
	return `{"content":[{"type":"text","text":"ok"}]}`
}

func TestClientsPreserveProtocolRequestsAndResponses(t *testing.T) {
	for _, tc := range []struct {
		provider                         schema.LLMProvider
		path, messages, response, finish string
	}{
		{
			provider: schema.ProviderOpenAI, path: "/chat/completions", finish: "tool_calls",
			messages: `[{"role":"system","content":"rules"},{"role":"user","content":"question"},{"role":"assistant","content":"working","reasoning_details":[{"text":"reason"}],"tool_calls":[{"id":"call","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"input.go\"}"}}]},{"role":"tool","tool_call_id":"call","content":"result"}]`,
			response: `{"choices":[{"message":{"content":"done","reasoning_details":[{"text":"thought"}],"tool_calls":[{"id":"next","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"next.go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
		},
		{
			provider: schema.ProviderAnthropic, path: "/v1/messages", finish: "tool_use",
			messages: `[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"thinking","thinking":"reason"},{"type":"text","text":"working"},{"type":"tool_use","id":"call","name":"read_file","input":{"path":"input.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":"result"}]}]`,
			response: `{"content":[{"type":"text","text":"done"},{"type":"thinking","thinking":"thought"},{"type":"tool_use","id":"next","name":"read_file","input":{"path":"next.go"}}],"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":3}}`,
		},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			type request struct {
				method, path string
				header       http.Header
				body         map[string]any
				err          error
			}
			requests := make(chan request, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				err := json.NewDecoder(r.Body).Decode(&body)
				requests <- request{r.Method, r.URL.Path, r.Header.Clone(), body, err}
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			httpClient := server.Client()
			base := httpClient.Transport
			httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				r.Header.Set("X-Injected-Client", "yes")
				return base.RoundTrip(r)
			})
			messages := []schema.Message{
				{Role: "system", Content: "rules"}, {Role: "user", Content: "question"},
				{Role: "assistant", Content: "working", Thinking: "reason", ToolCalls: []schema.ToolCall{{ID: "call", Function: schema.FunctionCall{Name: "read_file", Arguments: map[string]any{"path": "input.go"}}}}},
				{Role: "tool", ToolCallID: "call", Content: "result"},
			}
			tool := &tools.ReadTool{}
			out, err := testClient(t, tc.provider, server.URL+"/", RetryConfig{}, httpClient).Generate(context.Background(), messages, []tools.Tool{tool})
			if err != nil {
				t.Fatal(err)
			}
			req := <-requests
			if req.err != nil || req.method != http.MethodPost || req.path != tc.path || req.header.Get("Content-Type") != "application/json" || req.header.Get("X-Injected-Client") != "yes" || req.body["model"] != "test-model" {
				t.Fatalf("request transport changed: %+v", req)
			}
			var wantMessages any
			if err := json.Unmarshal([]byte(tc.messages), &wantMessages); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(req.body["messages"], wantMessages) {
				t.Fatalf("protocol messages changed: %#v", req.body["messages"])
			}
			definition := req.body["tools"].([]any)[0].(map[string]any)
			schemaKey := "input_schema"
			if tc.provider == schema.ProviderOpenAI {
				if req.header.Get("Authorization") != "Bearer test-key" || req.header.Get("x-api-key") != "" || req.body["system"] != nil || definition["type"] != "function" {
					t.Fatal("OpenAI headers or tool envelope changed")
				}
				definition = definition["function"].(map[string]any)
				schemaKey = "parameters"
			} else if req.header.Get("x-api-key") != "test-key" || req.header.Get("anthropic-version") != "2023-06-01" || req.header.Get("Authorization") != "" || req.body["system"] != "rules" || req.body["max_tokens"] != float64(16384) {
				t.Fatal("Anthropic headers, system extraction or output budget changed")
			}
			if definition["name"] != "read_file" || definition["description"] != tool.Description() || definition[schemaKey].(map[string]any)["type"] != "object" {
				t.Fatal("tool schema changed")
			}
			want := &schema.LLMResponse{Content: "done", Thinking: "thought", FinishReason: tc.finish, Usage: &schema.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}, ToolCalls: []schema.ToolCall{{ID: "next", Type: "function", Function: schema.FunctionCall{Name: "read_file", Arguments: map[string]any{"path": "next.go"}}}}}
			if !reflect.DeepEqual(out, want) {
				t.Fatalf("response conversion changed: %+v", out)
			}
		})
	}
}

type countedBody struct {
	io.Reader
	closed *atomic.Int32
}

func (b countedBody) Close() error { b.closed.Add(1); return nil }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestClientsRetryWithCompletePayloadAndCloseEveryBody(t *testing.T) {
	for _, provider := range []schema.LLMProvider{schema.ProviderOpenAI, schema.ProviderAnthropic} {
		for _, failure := range []string{"status", "parse", "read", "network"} {
			t.Run(string(provider)+"/"+failure, func(t *testing.T) {
				var closed atomic.Int32
				var payloads []string
				httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					data, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					payloads = append(payloads, string(data))
					status, body := http.StatusOK, io.Reader(strings.NewReader(minimalResponse(provider)))
					if len(payloads) == 1 {
						switch failure {
						case "status":
							status = http.StatusServiceUnavailable
						case "parse":
							body = strings.NewReader("invalid JSON")
						case "read":
							body = failingReader{}
						case "network":
							return nil, errors.New("temporary network failure")
						}
					}
					return &http.Response{StatusCode: status, Body: countedBody{body, &closed}}, nil
				})}
				out, err := testClient(t, provider, "https://example.invalid", RetryConfig{Enabled: true, MaxRetries: 1}, httpClient).Generate(context.Background(), []schema.Message{{Role: "user", Content: "question"}}, nil)
				wantClosed := int32(2)
				if failure == "network" {
					wantClosed = 1
				}
				if err != nil || out == nil || out.Content != "ok" || len(payloads) != 2 || payloads[0] == "" || payloads[0] != payloads[1] || closed.Load() != wantClosed {
					t.Fatalf("retry/payload/body cleanup failed: out=%+v err=%v requests=%v closed=%d", out, err, payloads, closed.Load())
				}
				var body map[string]any
				if err := json.Unmarshal([]byte(payloads[0]), &body); err != nil || body["tools"] != nil {
					t.Fatal("empty tool list was not omitted")
				}
				wantFinish := "stop"
				if provider == schema.ProviderAnthropic {
					wantFinish = "end_turn"
				}
				if out.FinishReason != wantFinish || out.Usage != nil {
					t.Fatal("default finish reason or absent usage changed")
				}
			})
		}
	}
}

func TestClientsExposeStatusAndUnderlyingErrorsAfterRetries(t *testing.T) {
	for _, provider := range []schema.LLMProvider{schema.ProviderOpenAI, schema.ProviderAnthropic} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusCreated} {
			t.Run(fmt.Sprintf("%s/%d", provider, status), func(t *testing.T) {
				var closed atomic.Int32
				calls := 0
				httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: status, Body: countedBody{strings.NewReader("denied"), &closed}}, nil
				})}
				out, err := testClient(t, provider, "https://example.invalid", RetryConfig{Enabled: true, MaxRetries: 2}, httpClient).Generate(context.Background(), nil, nil)
				var exhausted *RetryExhaustedError
				var apiErr *APIError
				if out != nil || !errors.As(err, &exhausted) || exhausted.Attempts != 3 || !errors.As(err, &apiErr) || apiErr.StatusCode != status || apiErr.Body != "denied" || err.Error() != fmt.Sprintf("api error %d: denied", status) || calls != 3 || closed.Load() != 3 {
					t.Fatalf("status/retry error contract changed: %v, calls=%d closed=%d", err, calls, closed.Load())
				}
			})
		}
		for _, failure := range []string{"network", "read"} {
			t.Run(string(provider)+"/"+failure, func(t *testing.T) {
				want := io.ErrUnexpectedEOF
				var closed atomic.Int32
				httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					if failure == "network" {
						return nil, want
					}
					return &http.Response{StatusCode: 200, Body: countedBody{failingReader{}, &closed}}, nil
				})}
				_, err := testClient(t, provider, "https://example.invalid", RetryConfig{}, httpClient).Generate(context.Background(), nil, nil)
				if !errors.Is(err, want) {
					t.Fatalf("underlying failure was lost: %v", err)
				}
				if failure == "read" && closed.Load() != 1 {
					t.Fatal("failed read leaked the response body")
				}
			})
		}
	}
}

func TestClientsCancelRequestsAndRetryBackoff(t *testing.T) {
	for _, provider := range []schema.LLMProvider{schema.ProviderOpenAI, schema.ProviderAnthropic} {
		for _, backoff := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/backoff=%v", provider, backoff), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				entered := make(chan struct{})
				var calls atomic.Int32
				httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					close(entered)
					if !backoff {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("retry later"))}, nil
				})}
				retry := RetryConfig{}
				if backoff {
					retry = RetryConfig{Enabled: true, MaxRetries: 3, InitialDelay: time.Hour, MaxDelay: time.Hour}
				}
				client := testClient(t, provider, "https://example.invalid", retry, httpClient)
				done := make(chan error, 1)
				go func() { _, err := client.Generate(ctx, nil, nil); done <- err }()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("request did not start")
				}
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
						t.Fatalf("cancellation retried or was hidden: %v, calls=%d", err, calls.Load())
					}
				case <-time.After(5 * time.Second):
					t.Fatal("cancellation did not stop the call")
				}
			})
		}
	}
}

func TestClientsRejectUnencodableRequestBeforeSending(t *testing.T) {
	for _, provider := range []schema.LLMProvider{schema.ProviderOpenAI, schema.ProviderAnthropic} {
		calls := 0
		httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") })}
		_, err := testClient(t, provider, "https://example.invalid", RetryConfig{MaxRetries: 2}, httpClient).Generate(context.Background(), []schema.Message{{Role: "user", Content: make(chan int)}}, nil)
		var unsupported *json.UnsupportedTypeError
		if !errors.As(err, &unsupported) || calls != 0 {
			t.Fatalf("unencodable request reached HTTP: %v, calls=%d", err, calls)
		}
	}
}

func TestClientDefaultTimeoutRemainsAvailableWithoutOptions(t *testing.T) {
	openai := NewOpenAIClient("key", "https://example.invalid/", "model", RetryConfig{})
	anthropic := NewAnthropicClient("key", "https://example.invalid/", "model", RetryConfig{}, WithHTTPClient(nil), nil)
	for _, transport := range []*httpTransport{openai.transport, anthropic.transport} {
		if transport.client == nil || transport.client.Timeout != 120*time.Second {
			t.Fatal("default timeout changed or nil options broke construction")
		}
	}
}

func TestClientsSupportConcurrentCallsWithoutMixingResponses(t *testing.T) {
	for _, provider := range []schema.LLMProvider{schema.ProviderOpenAI, schema.ProviderAnthropic} {
		t.Run(string(provider), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct{ Content string } `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 1 {
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				_, _ = io.WriteString(w, strings.Replace(minimalResponse(provider), "ok", body.Messages[0].Content, 1))
			}))
			defer server.Close()
			client := testClient(t, provider, server.URL, RetryConfig{}, server.Client())
			done := make(chan error, 8)
			for i := 0; i < 8; i++ {
				go func() {
					want := fmt.Sprintf("request-%d", i)
					out, err := client.Generate(context.Background(), []schema.Message{{Role: "user", Content: want}}, nil)
					if err == nil && (out == nil || out.Content != want) {
						err = fmt.Errorf("response did not match %s: %+v", want, out)
					}
					done <- err
				}()
			}
			for i := 0; i < 8; i++ {
				if err := <-done; err != nil {
					t.Error(err)
				}
			}
		})
	}
}
