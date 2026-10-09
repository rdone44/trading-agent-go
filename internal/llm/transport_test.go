package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// fastRetries removes the backoff wait for the duration of one test.
func fastRetries(t *testing.T) {
	t.Helper()
	previous := llmSleep
	llmSleep = func(time.Duration) {}
	t.Cleanup(func() { llmSleep = previous })
}

// A proxy that drops an idle keep-alive connection makes the first request
// after a quiet period fail with a reset. The request must be sent again
// instead of surfacing "connection forcibly closed" to the settings panel.
// Regression test for the reported 获取模型失败 error.
func TestListModelsRetriesDroppedConnection(t *testing.T) {
	fastRetries(t)
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// Simulate the proxy tearing the connection down mid-request.
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer server.Close()

	models, err := ListModels(server.URL, "sk-test")
	if err != nil {
		t.Fatalf("ListModels after a dropped connection: %v", err)
	}
	if len(models) != 1 || models[0] != "model-a" {
		t.Fatalf("models = %v, want [model-a]", models)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("provider saw %d requests, want 2 (one reset, one retry)", got)
	}
}

// A gateway that is briefly unavailable (a proxy node switching, an upstream
// 502) is retried; the user does not have to press 获取模型 again.
func TestListModelsRetriesServerErrors(t *testing.T) {
	fastRetries(t)
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"bad gateway"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-b"}]}`))
	}))
	defer server.Close()

	models, err := ListModels(server.URL, "sk-test")
	if err != nil {
		t.Fatalf("ListModels after a 502: %v", err)
	}
	if len(models) != 1 || models[0] != "model-b" {
		t.Fatalf("models = %v, want [model-b]", models)
	}
}

// A wrong token is the user's mistake and must come back immediately: the
// provider is answering, so retrying only delays the message.
func TestListModelsDoesNotRetryAuthFailures(t *testing.T) {
	fastRetries(t)
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	defer server.Close()

	_, err := ListModels(server.URL, "bad")
	if err == nil {
		t.Fatal("want an error for a 401")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("provider saw %d requests, want 1 (4xx is not retried)", got)
	}
}

// A persistent 5xx still ends with the provider's own answer, so the panel
// can show what the gateway said rather than a generic failure.
func TestListModelsReportsProviderErrorAfterRetries(t *testing.T) {
	fastRetries(t)
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream overloaded"}}`))
	}))
	defer server.Close()

	_, err := ListModels(server.URL, "k")
	if err == nil {
		t.Fatal("want an error when every attempt fails")
	}
	if !strings.Contains(err.Error(), "upstream overloaded") {
		t.Errorf("provider message lost: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != llmAttempts {
		t.Fatalf("provider saw %d requests, want %d", got, llmAttempts)
	}
}

// Complete is the trading path: a dropped connection must not fail the cycle.
func TestCompleteRetriesDroppedConnection(t *testing.T) {
	fastRetries(t)
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()

	c := New(config.LLM{BaseURL: server.URL})
	c.APIKey = "test-key"
	got, err := c.Complete("system", "user")
	if err != nil {
		t.Fatalf("Complete after a dropped connection: %v", err)
	}
	if got != "ok" {
		t.Fatalf("Complete = %q, want ok", got)
	}
}

// A request that cannot even be built is not retried: the caller gets the
// construction error instead of three identical failures.
func TestDoWithRetryReturnsBuildErrorImmediately(t *testing.T) {
	fastRetries(t)
	var attempts int32
	_, err := doWithRetry(&http.Client{Timeout: time.Second}, func() (*http.Request, error) {
		atomic.AddInt32(&attempts, 1)
		return nil, errors.New("构造失败")
	})
	if err == nil {
		t.Fatal("want the build error")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("build called %d times, want 1", got)
	}
}

// A request that exhausted the client's whole deadline is not retried: the
// attempt already waited the full budget, so a second one only doubles the
// delay before the user sees the error.
func TestDoWithRetryDoesNotRetryExhaustedDeadline(t *testing.T) {
	fastRetries(t)
	var attempts int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&attempts, 1)
		return nil, fmt.Errorf("Post %q: %w", "https://x/v1/chat/completions", context.DeadlineExceeded)
	})}
	_, err := doWithRetry(client, func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, "https://example.invalid/v1/models", nil)
	})
	if err == nil {
		t.Fatal("want the timeout error")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("transport called %d times, want 1 (an exhausted deadline is not retried)", got)
	}
}

// A dial or TLS-handshake timeout fails early and is usually a flaky proxy
// node, so it IS retried — unlike an exhausted overall deadline.
func TestDoWithRetryRetriesDialTimeouts(t *testing.T) {
	fastRetries(t)
	var attempts int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			return nil, fmt.Errorf("dial tcp 127.0.0.1:10808: %w", &net.DNSError{IsTimeout: true})
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"m"}]}`)),
			Header:     make(http.Header),
		}, nil
	})}
	response, err := doWithRetry(client, func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, "https://example.invalid/v1/models", nil)
	})
	if err != nil {
		t.Fatalf("dial timeout should be retried: %v", err)
	}
	response.Body.Close()
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("transport called %d times, want 2", got)
	}
}

// The proxy hint turns the opaque Windows message into something the user can
// act on, and is not added to unrelated errors.
func TestDescribeTransportErrorAddsProxyHint(t *testing.T) {
	reset := errors.New("read tcp 127.0.0.1:63326->127.0.0.1:10808: wsarecv: An existing connection was forcibly closed by the remote host")
	got := describeTransportError(reset)
	if !strings.Contains(got, "代理") {
		t.Errorf("a reset connection should mention the proxy: %s", got)
	}
	if !strings.Contains(got, "forcibly closed") {
		t.Errorf("the original error text should be preserved: %s", got)
	}

	plain := errors.New("dial tcp: connection refused")
	if got := describeTransportError(plain); strings.Contains(got, "代理") {
		t.Errorf("a refused connection is not a proxy reset: %s", got)
	}
}

// A deadline is a different problem from a reset, so it gets different
// advice: raise the budget instead of checking the proxy.
func TestDescribeTransportErrorExplainsDeadline(t *testing.T) {
	deadline := errors.New(`Post "https://sub.example.cc/v1/chat/completions": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`)
	got := describeTransportError(deadline)
	if !strings.Contains(got, "timeout_sec") {
		t.Errorf("a deadline should point at the timeout setting: %s", got)
	}
	if strings.Contains(got, "代理在线") {
		t.Errorf("a deadline is not a proxy reset: %s", got)
	}
}

// The transport keeps the proxy settings the environment provides: an
// endpoint reachable only through a local proxy must keep working.
func TestTransportHonoursProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:10808")
	transport := transport()
	if transport.Proxy == nil {
		t.Fatal("transport has no proxy function; a proxy-only gateway would break")
	}
	request := httptest.NewRequest(http.MethodGet, "https://sub.example.cc/v1/models", nil)
	proxyURL, err := transport.Proxy(request)
	if err != nil {
		t.Fatalf("proxy lookup: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "127.0.0.1:10808" {
		t.Fatalf("proxy = %v, want the environment's proxy", proxyURL)
	}
}

// A pooled connection is retired well before a proxy would consider it
// stale, which is the actual fix for the reported failure.
func TestTransportRetiresIdleConnectionsQuickly(t *testing.T) {
	if got := transport().IdleConnTimeout; got != llmIdleTimeout {
		t.Fatalf("IdleConnTimeout = %s, want %s", got, llmIdleTimeout)
	}
	if llmIdleTimeout >= 90*time.Second {
		t.Fatalf("llmIdleTimeout = %s, too close to the default idle window", llmIdleTimeout)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
