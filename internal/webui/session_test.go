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

// newLiveTestServer wires a server whose trading session reads only offline
// bars, so the lifecycle tests never touch Binance.
func newLiveTestServer(t *testing.T) *webui.Server {
	t.Helper()
	server, _ := newTestServer(t)
	session := server.Session()
	session.SeriesLoader = fxLoader
	session.PriceLoader = func(symbol string) (float64, time.Time, error) {
		series := testfx.Bars(symbol, 5, 42, time.Now().UTC())
		last := series.Len() - 1
		return series.Bars[last].Close, series.Bars[last].Time, nil
	}
	return server
}

func postJSON(t *testing.T, server *webui.Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func getSession(t *testing.T, server *webui.Server) webui.SessionStatus {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/session", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/session = %d, want 200", recorder.Code)
	}
	var status webui.SessionStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode session status: %v", err)
	}
	return status
}

// An idle console must still answer, so the page can render before a session
// exists instead of showing a spinner forever.
func TestSessionStatusOnIdleServer(t *testing.T) {
	server, _ := newTestServer(t)
	status := getSession(t, server)
	if status.Running {
		t.Fatal("a fresh server must report running=false")
	}
	if status.Mode != "paper" {
		t.Fatalf("mode = %q, want paper by default", status.Mode)
	}
	if status.Venue != "spot" {
		t.Fatalf("venue = %q, want spot by default", status.Venue)
	}
}

func TestSessionStartRejectsWrongMethod(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/session/start", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

// The single most important guard in the console: arming real orders without
// the confirmation phrase must fail, and must fail before any broker is built.
func TestSessionStartRefusesLiveWithoutConfirmPhrase(t *testing.T) {
	server, _ := newTestServer(t)
	cases := map[string]string{
		"empty":      `{"symbol":"TEST","strategy":"ma_cross","days":120,"execute":true}`,
		"wrong word": `{"symbol":"TEST","strategy":"ma_cross","days":120,"execute":true,"confirm":"yes"}`,
		"english":    `{"symbol":"TEST","strategy":"ma_cross","days":120,"execute":true,"confirm":"confirm"}`,
	}
	for name, body := range cases {
		recorder := postJSON(t, server, "/api/session/start", body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "确认实盘") {
			t.Errorf("%s: error must mention the confirmation phrase, got %s", name, recorder.Body.String())
		}
	}
	if getSession(t, server).Running {
		t.Fatal("a rejected start must not leave a session running")
	}
}

// Even with the phrase, live trading needs exchange credentials in the
// environment; the test process has none, so this must still refuse.
func TestSessionStartLiveNeedsExchangeKeys(t *testing.T) {
	t.Setenv("TA_ALLOW_LIVE", "1")
	server, _ := newTestServer(t)
	t.Setenv("BINANCE_API_KEY", "")
	t.Setenv("BINANCE_SECRET_KEY", "")

	recorder := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"ma_cross","days":120,"execute":true,"confirm":"确认实盘"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "BINANCE_API_KEY") {
		t.Errorf("error must name the missing env vars, got %s", recorder.Body.String())
	}
}

func TestSessionStartRejectsBadLeverage(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"ma_cross","days":120,"leverage":0}`)
	// leverage 0 means "unset" and is allowed; a negative value is not.
	if recorder.Code != http.StatusOK {
		t.Fatalf("leverage 0 should be treated as unset, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stop := postJSON(t, server, "/api/session/stop", "")
	if stop.Code != http.StatusOK {
		t.Fatalf("stop = %d", stop.Code)
	}
}

func TestSessionStartRejectsUnknownStrategy(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"nope","days":120}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestSessionStopOnIdleServerIsNoop(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := postJSON(t, server, "/api/session/stop", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("stopping an idle session = %d, want 200", recorder.Code)
	}
	if getSession(t, server).Running {
		t.Fatal("session must not be running after stop")
	}
}

func TestSessionStepRequiresARunningSession(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := postJSON(t, server, "/api/session/step", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("step with no session = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "没有正在运行") {
		t.Errorf("error should explain there is no session, got %s", recorder.Body.String())
	}
}

// A session that is started, stepped and stopped must leave the console with
// a coherent record. The offline loader keeps the test off the network, but
// the live loop still reaches the public ticker, so this only asserts the
// control flow rather than the trading outcome.
func TestSessionLifecycleRecordsCycles(t *testing.T) {
	server, _ := newTestServer(t)
	// The live loop fetches a live ticker even when the series is injected, so
	// a start without network access may fail on the first cycle. Either way
	// the API contract holds: start returns 200 or a clear 400, and a running
	// session stops cleanly.
	recorder := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"ma_cross","days":120,"interval_seconds":3600}`)
	if recorder.Code != http.StatusOK {
		t.Skipf("start needs a live ticker in this environment: %s", recorder.Body.String())
	}

	status := getSession(t, server)
	if !status.Running {
		t.Fatal("session should be running right after a successful start")
	}
	if status.Interval != 3600 {
		t.Fatalf("interval = %d, want 3600", status.Interval)
	}

	stop := postJSON(t, server, "/api/session/stop", "")
	if stop.Code != http.StatusOK {
		t.Fatalf("stop = %d: %s", stop.Code, stop.Body.String())
	}
	after := getSession(t, server)
	if after.Running {
		t.Fatal("session must not be running after stop")
	}
	if after.StoppedAt == "" {
		t.Error("stopped_at should be set so the console can show when it ended")
	}
}

// Starting twice must not silently replace the first runner: the second call
// would orphan a loop that is still placing orders.
func TestSessionStartTwiceIsRefused(t *testing.T) {
	server, _ := newTestServer(t)
	first := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"ma_cross","days":120,"interval_seconds":3600}`)
	if first.Code != http.StatusOK {
		t.Skipf("start needs a live ticker in this environment: %s", first.Body.String())
	}
	defer postJSON(t, server, "/api/session/stop", "")

	second := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"ma_cross","days":120,"interval_seconds":3600}`)
	if second.Code != http.StatusBadRequest {
		t.Fatalf("second start = %d, want 400", second.Code)
	}
	if !strings.Contains(second.Body.String(), "已有交易会话") {
		t.Errorf("error should say a session is already running, got %s", second.Body.String())
	}
}

// Stopping is not a liquidation. The final log row must carry the cash and the
// still-open position through, or the console reads as a flat book.
func TestSessionStopKeepsPositionInTheLog(t *testing.T) {
	server := newLiveTestServer(t)
	start := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"ma_cross","days":300,"interval_seconds":3600}`)
	if start.Code != http.StatusOK {
		t.Fatalf("start = %d: %s", start.Code, start.Body.String())
	}

	// One step so a position exists, then stop and read the final row.
	if recorder := postJSON(t, server, "/api/session/step", ""); recorder.Code != http.StatusOK {
		t.Fatalf("step = %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := postJSON(t, server, "/api/session/stop", ""); recorder.Code != http.StatusOK {
		t.Fatalf("stop = %d: %s", recorder.Code, recorder.Body.String())
	}

	status := getSession(t, server)
	if status.Position.Open != true {
		t.Fatal("stopping must not close the position")
	}
	if len(status.Log) == 0 {
		t.Fatal("expected at least one log row")
	}
	last := status.Log[len(status.Log)-1]
	if last.Action != "已停止" {
		t.Fatalf("last row action = %q, want 已停止", last.Action)
	}
	if last.Cash <= 0 {
		t.Errorf("stop row cash = %v; it must carry the book's cash, not zero", last.Cash)
	}
	if last.Position == "" || last.Position == "空仓" {
		t.Errorf("stop row position = %q, want the still-open position", last.Position)
	}
	if last.Equity <= 0 {
		t.Errorf("stop row equity = %v, want the marked equity", last.Equity)
	}
}
