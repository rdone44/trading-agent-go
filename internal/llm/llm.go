// Package llm wires an OpenAI-compatible chat model into the agent: as a
// strategy that emits target positions, as a second-opinion veto on new live
// entries, as a post-mortem reviewer of a finished run, and as the proposer
// in the parameter-tuning loop.
//
// The client is a thin REST wrapper on net/http so the project keeps its single
// non-stdlib dependency. The API key is never stored in config: it is read
// from the environment at call time. Every feature degrades gracefully when
// the model is unreachable — the veto fails open, the strategy and review
// report the error to the caller.
package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// Client talks to one OpenAI-compatible /chat/completions endpoint.
type Client struct {
	baseURL   string
	model     string
	maxTokens int
	temp      float64
	APIKey    string // resolved at construction; empty disables the model
	http      *http.Client
}

// New builds a client from config. The API key is taken from cfg.APIKey when
// set (the webui injects the logged-in user's vault credential there),
// falling back to the environment (see config.LLMAPIKey). A nil-safe
// client: when the key is empty, Complete returns an error rather than
// dialing.
func New(cfg config.LLM) *Client {
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := cfg.Model
	if model == "" {
		model = "gpt-4o-mini"
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	temp := cfg.Temperature
	if temp == 0 {
		temp = 0.2
	}
	apiKey := cfg.APIKey
	if apiKey == "" && !cfg.NoEnvKey {
		// Account-mode requests set NoEnvKey: the env fallback is skipped so
		// the deployer's LLM_API_KEY can never be sent to a user-controlled
		// endpoint. The model is simply disabled (Complete errors) instead.
		apiKey = config.LLMAPIKey()
	}
	return &Client{
		baseURL:   strings.TrimRight(base, "/"),
		model:     model,
		maxTokens: maxTokens,
		temp:      temp,
		APIKey:    apiKey,
		http:      &http.Client{Timeout: timeout},
	}
}

// Enabled reports whether a key is present and a model is configured.
func (c *Client) Enabled() bool { return c.APIKey != "" }

// Model returns the configured model name (useful for logs and reports).
func (c *Client) Model() string { return c.model }

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
}

// Complete sends one chat completion and returns the assistant's content.
func (c *Client) Complete(system, user string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("LLM 需要 API key（设置 LLM_API_KEY 或 OPENAI_API_KEY）")
	}
	req := chatRequest{
		Model: c.model, MaxTokens: c.maxTokens, Temperature: c.temp,
		Messages: []message{{Role: "system", Content: system}, {Role: "user", Content: user}},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequest(http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("请求 LLM 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析 LLM 响应失败: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("LLM 没有返回任何候选")
	}
	return out.Choices[0].Message.Content, nil
}

// CompleteJSON is Complete plus a parse of the answer into out. Models
// frequently wrap JSON in markdown fences, so the payload is extracted from
// the first '{' to the last '}' before unmarshalling.
func (c *Client) CompleteJSON(system, user string, out any) error {
	content, err := c.Complete(system, user)
	if err != nil {
		return err
	}
	payload := extractJSON(content)
	if payload == "" {
		return fmt.Errorf("LLM 的回答里找不到 JSON 对象: %q", truncate(content, 120))
	}
	if err := json.Unmarshal([]byte(payload), out); err != nil {
		return fmt.Errorf("解析 LLM 的 JSON 失败: %w", err)
	}
	return nil
}

// extractJSON finds the first balanced top-level object in the text.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}

// Truncate shortens a model answer for display in an error or a log line. It
// slices on a rune boundary so a multi-byte character is never split.
func Truncate(s string, n int) string {
	return truncate(s, n)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Slice on a rune boundary: cutting bytes would split a multi-byte
	// character and emit invalid UTF-8 into the error message.
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
