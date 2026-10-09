package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/testfx"
	"github.com/rdone44/trading-agent-go/internal/webui"
)

// wireUserSession makes one account's trading session read offline bars so
// the isolation test never touches Binance.
func wireUserSession(t *testing.T, server *webui.Server, username string) {
	t.Helper()
	sess := server.Session(username)
	sess.SeriesLoader = fxLoader
	sess.PriceLoader = func(symbol string) (float64, time.Time, error) {
		series := testfx.Bars(symbol, 5, 42, time.Now().UTC())
		last := series.Len() - 1
		return series.Bars[last].Close, series.Bars[last].Time, nil
	}
}

func postJSONAs(t *testing.T, server *webui.Server, path, body, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "ta_session", Value: cookie})
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func getSessionAs(t *testing.T, server *webui.Server, cookie string) webui.SessionStatus {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "ta_session", Value: cookie})
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/session = %d: %s", rec.Code, rec.Body.String())
	}
	var status webui.SessionStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode session status: %v", err)
	}
	return status
}

// waitForFirstCycle blocks until a session has completed at least one decision
// cycle, and returns that settled status.
//
// The loop runs its first cycle immediately but in the background, and
// /api/session deliberately reports the last published snapshot instead of
// waiting for the cycle (that is what keeps the console responsive during a
// slow model call). A test that compares equity across an action therefore has
// to let the session's own first cycle finish first, or it would measure that
// cycle instead of the action under test.
func waitForFirstCycle(t *testing.T, server *webui.Server, cookie string) webui.SessionStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		status := getSessionAs(t, server, cookie)
		if status.Cycles >= 1 {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatal("the session never completed its first cycle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With the vault enabled, accounts must not be able to see, drive or stop
// each other's trading loops. Regression test for the shared-session audit
// finding: a single global session let any account read or kill another
// account's live position.
func TestSessionsAreIsolatedPerAccount(t *testing.T) {
	server, _ := newAuthServer(t)

	alice := register(t, server, "alice", "pw-alice-1")
	bob := register(t, server, "bob", "pw-bob-1")

	wireUserSession(t, server, "alice")
	wireUserSession(t, server, "bob")

	// Alice starts her paper session; bob's view must not reflect it.
	if rec := postJSONAs(t, server, "/api/session/start",
		`{"symbol":"BTCUSDT","days":120,"interval_seconds":3600}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("alice start = %d: %s", rec.Code, rec.Body.String())
	}
	defer server.Session("alice").Stop()

	if st := getSessionAs(t, server, alice); !st.Running {
		t.Fatal("alice's session must report running after start")
	}
	if st := getSessionAs(t, server, bob); st.Running {
		t.Fatal("bob must NOT see alice's session as running")
	}

	// Bob stepping his OWN session must not consume or advance alice's book.
	if rec := postJSONAs(t, server, "/api/session/start",
		`{"symbol":"BTCUSDT","days":120,"interval_seconds":3600}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("bob start = %d: %s", rec.Code, rec.Body.String())
	}
	defer server.Session("bob").Stop()

	before := waitForFirstCycle(t, server, alice)
	if rec := postJSONAs(t, server, "/api/session/step", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("bob step = %d: %s", rec.Code, rec.Body.String())
	}
	after := getSessionAs(t, server, alice)
	if before.Equity != after.Equity {
		t.Fatalf("bob's step changed alice's book: %v -> %v", before.Equity, after.Equity)
	}

	// Bob stopping must leave alice's loop running.
	if rec := postJSONAs(t, server, "/api/session/stop", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("bob stop = %d: %s", rec.Code, rec.Body.String())
	}
	if st := getSessionAs(t, server, alice); !st.Running {
		t.Fatal("bob's stop must not stop alice's session")
	}
}

// No-vault (desktop / CLI) builds keep exactly one shared session: requests
// with no username all address the same historical session.
func TestSessionSharedWhenAuthDisabled(t *testing.T) {
	server, _ := newTestServer(t)
	if a, b := server.Session(""), server.Session(""); a != b {
		t.Fatal("the default (empty-username) session must be a single instance")
	}
}
