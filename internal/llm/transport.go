package llm

// This file holds the HTTP plumbing shared by every call this package makes.
//
// The model endpoint is almost always reached through whatever proxy the
// operator runs (a local v2rayN/xray or Clash instance, a corporate gateway),
// and two failure modes show up there constantly. Neither is the model's
// fault, and both look identical to the user: "获取模型失败".
//
//   - The proxy silently closes idle keep-alive connections. Go only notices
//     when it writes the next request onto the dead socket, and reports
//     "An existing connection was forcibly closed by the remote host"
//     (Windows) or "connection reset by peer" (Linux). The first call after a
//     quiet period then fails even though the gateway is perfectly healthy.
//   - A flaky node drops the TLS handshake or answers 502/503/504.
//
// Go's own transport only retries a request it considers replayable, and a
// POST without an Idempotency-Key is not one — so the chat completions call
// surfaces the reset to the caller. The fixes here are a transport with a
// short idle timeout (so a dead pooled connection is rarely reused) plus a
// small retry loop for the transport errors and 5xx answers that remain.
//
// Retrying a chat completion is safe in this codebase: the model only ever
// produces a decision (a target position, a veto, a review, a parameter set),
// never an order. A retried call can at worst bill the same prompt twice; the
// alternative is a trade cycle that fails because a proxy blinked.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// llmAttempts is how many times one request is sent before giving up.
	llmAttempts = 3
	// llmIdleTimeout retires pooled connections quickly. The LLM is called
	// minutes apart (one decision per cycle), so a long-lived idle connection
	// buys nothing and is exactly what a proxy kills behind our back.
	llmIdleTimeout = 15 * time.Second
)

// llmBackoffs is the wait before retries 2 and 3. It is short on purpose: the
// user is watching a settings panel, and a reset connection fails instantly
// rather than after a timeout.
var llmBackoffs = []time.Duration{400 * time.Millisecond, 1200 * time.Millisecond}

// llmSleep performs the wait between attempts. It is a variable so a test can
// replace it with a no-op and keep the suite fast.
var llmSleep = func(d time.Duration) { time.Sleep(d) }

var (
	sharedTransportOnce sync.Once
	sharedTransport     *http.Transport
)

// transport returns the process-wide transport used by every LLM request.
//
// It is a clone of http.DefaultTransport, so the environment's proxy settings
// (HTTPS_PROXY / NO_PROXY) keep applying — a gateway that is only reachable
// through the operator's proxy must not stop working. Only the pool
// behaviour is tightened, and the pool is shared so repeated calls in one
// process reuse a live connection instead of paying a new TLS handshake.
func transport() *http.Transport {
	sharedTransportOnce.Do(func() {
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			// DefaultTransport was replaced (tests do this). Fall back to a
			// plain transport that still honours the proxy environment.
			base = &http.Transport{Proxy: http.ProxyFromEnvironment}
		}
		clone := base.Clone()
		if clone.Proxy == nil {
			// Never disable proxying: on many networks the endpoint is only
			// reachable through it.
			clone.Proxy = http.ProxyFromEnvironment
		}
		clone.IdleConnTimeout = llmIdleTimeout
		clone.MaxIdleConns = 4
		clone.MaxIdleConnsPerHost = 2
		clone.DialContext = (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		sharedTransport = clone
	})
	return sharedTransport
}

// newClient builds the http.Client every LLM call goes through.
func newClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transport()}
}

// retryableStatus reports whether a response is worth sending again. 5xx and
// 429 come from a gateway or a node in front of it, not from a decision the
// model made, so a second attempt is meaningful. A 4xx is the caller's own
// mistake (bad key, bad URL) and is returned immediately.
func retryableStatus(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests
}

// doWithRetry sends the request produced by build, retrying transient
// failures. build is called once per attempt because a request body can only
// be read once.
//
// The returned response (when err is nil) is always the caller's to close. A
// retryable status that survived every attempt is returned as-is, so the
// caller can still report what the provider actually said.
func doWithRetry(client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= llmAttempts; attempt++ {
		if attempt > 1 {
			llmSleep(llmBackoff(attempt))
		}
		request, err := build()
		if err != nil {
			return nil, err
		}
		response, err := client.Do(request)
		if err != nil {
			// An attempt that already burned the client's whole budget will
			// not succeed on the next one; retrying only makes the user wait
			// longer for the same answer. Dial and TLS-handshake timeouts are
			// NOT in this category: those fail early, usually because a proxy
			// node is flaky, and the next attempt often succeeds.
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			lastErr = err
			continue
		}
		if !retryableStatus(response.StatusCode) || attempt == llmAttempts {
			return response, nil
		}
		lastErr = fmt.Errorf("模型服务返回 HTTP %d", response.StatusCode)
		response.Body.Close()
	}
	return nil, fmt.Errorf("%w（已重试 %d 次）", lastErr, llmAttempts)
}

// llmBackoff returns the wait before the attempt-numbered retry.
func llmBackoff(attempt int) time.Duration {
	if attempt-1 >= len(llmBackoffs) {
		return llmBackoffs[len(llmBackoffs)-1]
	}
	return llmBackoffs[attempt-1]
}

// proxyHintMarkers are the substrings Windows and Linux put in a
// reset-by-proxy error. They are matched to decide whether to append the one
// piece of advice that actually helps.
var proxyHintMarkers = []string{
	"forcibly closed",
	"connection reset",
	"broken pipe",
	"unexpected eof",
	"wsarecv",
}

// timeoutHintMarkers are the substrings of an exhausted deadline. They are
// matched separately from the reset markers because the advice is different:
// a reset means "the proxy blinked, try again", a deadline means "the model
// did not answer in the time allowed".
var timeoutHintMarkers = []string{
	"deadline exceeded",
	"timeout exceeded",
	"tls handshake timeout",
	"i/o timeout",
}

// describeTransportError turns a connection-level failure into a message the
// user can act on. Without it the raw text ("An existing connection was
// forcibly closed by the remote host") reads like the model endpoint itself is
// broken, which sends the user looking in the wrong place; a deadline reads
// like a broken model rather than an exhausted budget.
func describeTransportError(err error) string {
	text := strings.ToLower(err.Error())
	for _, marker := range proxyHintMarkers {
		if strings.Contains(text, marker) {
			return fmt.Sprintf("%v（连接被中断；若通过本地代理/加速器访问，请确认代理在线，或稍后重试）", err)
		}
	}
	for _, marker := range timeoutHintMarkers {
		if strings.Contains(text, marker) {
			return fmt.Sprintf("%v（模型在超时时间内没有返回；推理模型或长提示词需要更长的 timeout_sec，可在配置文件中调大）", err)
		}
	}
	return err.Error()
}
