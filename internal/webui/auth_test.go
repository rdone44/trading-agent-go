package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The server edition is reachable over a network, so every route must demand
// the token; a regression here would expose the dashboard to anyone.
func TestTokenIsRequiredOnEveryRoute(t *testing.T) {
	server, _ := newTestServer(t)
	server.Token = "s3cret"

	for _, path := range []string{"/", "/app.js", "/api/config", "/api/runs", "/healthz"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, recorder.Code)
		}
	}
}

func TestBearerTokenIsAccepted(t *testing.T) {
	server, _ := newTestServer(t)
	server.Token = "s3cret"

	request := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	request.Header.Set("Authorization", "Bearer s3cret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestWrongTokenIsRejected(t *testing.T) {
	server, _ := newTestServer(t)
	server.Token = "s3cret"

	request := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	request.Header.Set("Authorization", "Bearer wrong")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}

// A ?token= visit sets the cookie and redirects, so the token does not stay
// in the address bar where it would land in history and bookmarks.
func TestQueryTokenSetsCookieAndStripsURL(t *testing.T) {
	server, _ := newTestServer(t)
	server.Token = "s3cret"

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/?token=s3cret", nil))
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/" {
		t.Fatalf("Location = %q, want / (token stripped)", location)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != "ta_token" || cookies[0].Value != "s3cret" {
		t.Fatalf("cookies = %v, want ta_token=s3cret", cookies)
	}
	if !cookies[0].HttpOnly {
		t.Error("auth cookie must be HttpOnly")
	}
}

func TestCookieTokenIsAccepted(t *testing.T) {
	server, _ := newTestServer(t)
	server.Token = "s3cret"

	request := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	request.AddCookie(&http.Cookie{Name: "ta_token", Value: "s3cret"})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

// The desktop build runs on loopback and must stay frictionless.
func TestNoTokenMeansOpenAccess(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when no token is configured", recorder.Code)
	}
}

func TestHealthzReportsOK(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("healthz is not JSON: %v", err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("status field = %v, want ok", payload["status"])
	}
}

// The desktop UI needs to know it may show the 退出 button.
func TestConfigExposesDesktopFlag(t *testing.T) {
	server, _ := newTestServer(t)

	read := func() bool {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		var payload struct {
			Desktop bool `json:"desktop"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode /api/config: %v", err)
		}
		return payload.Desktop
	}

	if read() {
		t.Fatal("desktop flag must default to false")
	}
	server.Desktop = true
	if !read() {
		t.Fatal("desktop flag must be true once set")
	}
}

// The shutdown route exists only when the desktop shell wired a cancel func,
// so a networked server can never be stopped by an HTTP request.
func TestShutdownRouteIsAbsentWithoutCancel(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/shutdown", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no cancel func is wired", recorder.Code)
	}
}

func TestShutdownRouteCancelsContext(t *testing.T) {
	server, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	server.Cancel = cancel

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/shutdown", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	// The cancel is deliberately delayed so the response can flush first.
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context was not cancelled after POST /api/shutdown")
	}
}

func TestShutdownRejectsGET(t *testing.T) {
	server, _ := newTestServer(t)
	server.Cancel = func() {}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/shutdown", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

// Serve must report the address it bound and stop when the context ends.
func TestServeReportsAddressAndStopsOnCancel(t *testing.T) {
	server, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())

	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, "127.0.0.1:0", func(addr string) { ready <- addr })
	}()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("Serve returned before becoming ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve never reported a ready address")
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("addr = %q, want a 127.0.0.1 address", addr)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}
