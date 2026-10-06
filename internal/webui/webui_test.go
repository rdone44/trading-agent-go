package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/webui"
)

func newTestServer(t *testing.T) (*webui.Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Agent.Symbol = "TEST"
	cfg.Agent.HistoryDays = 300
	cfg.Backtest.WarmupBars = 30
	cfg.Backtest.OutputDir = dir
	return webui.New(cfg), dir
}

func TestDashboardPageIsServed(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{"trading-agent", "开始回测", "/app.js", "/app.css"} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html is missing %q", want)
		}
	}
}

func TestStaticAssetsAreEmbedded(t *testing.T) {
	server, _ := newTestServer(t)
	for _, path := range []string{"/app.css", "/app.js"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("%s returned %d, want 200 (embedded asset missing?)", path, recorder.Code)
		}
		if recorder.Body.Len() == 0 {
			t.Errorf("%s is empty", path)
		}
	}
}

func TestConfigEndpointDescribesStrategies(t *testing.T) {
	server, dir := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/config", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var payload struct {
		Symbol     string `json:"symbol"`
		Strategy   string `json:"strategy"`
		OutputDir  string `json:"output_dir"`
		Strategies []struct {
			Name   string `json:"name"`
			Title  string `json:"title"`
			Params []struct {
				Key     string  `json:"key"`
				Default float64 `json:"default"`
			} `json:"params"`
		} `json:"strategies"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Symbol != "TEST" {
		t.Errorf("symbol = %q, want TEST", payload.Symbol)
	}
	if payload.OutputDir != dir {
		t.Errorf("output_dir = %q, want %q", payload.OutputDir, dir)
	}
	if len(payload.Strategies) != 3 {
		t.Fatalf("strategies = %d, want 3", len(payload.Strategies))
	}
	for _, spec := range payload.Strategies {
		if spec.Name == "" || spec.Title == "" || len(spec.Params) == 0 {
			t.Errorf("incomplete strategy spec: %+v", spec)
		}
	}
}

func TestBacktestEndpointRunsAndPersists(t *testing.T) {
	server, dir := newTestServer(t)
	body := `{"symbol":"TEST","strategy":"ma_cross","provider":"synthetic","days":300,
	          "seed":42,"initial_cash":100000,"warmup_bars":30,
	          "risk":{"stop_loss_pct":0.05,"slippage_bps":2}}`

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/backtest", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Symbol  string `json:"symbol"`
		Bars    int    `json:"bars"`
		RunName string `json:"run_name"`
		Equity  []struct {
			T      string  `json:"t"`
			Equity float64 `json:"equity"`
		} `json:"equity"`
		Metrics struct {
			FinalEquity *float64 `json:"final_equity"`
			NumTrades   int      `json:"num_trades"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Symbol != "TEST" || payload.Bars != 300 {
		t.Errorf("unexpected result: symbol=%q bars=%d", payload.Symbol, payload.Bars)
	}
	if len(payload.Equity) < 2 {
		t.Error("expected an equity curve in the response")
	}
	if payload.Metrics.FinalEquity == nil {
		t.Error("final equity is missing")
	}
	if payload.RunName == "" {
		t.Fatal("run name is missing")
	}

	// The run must be persisted so the dashboard can list and reopen it.
	for _, name := range []string{"run.json", "report.html", "equity.csv", "trades.csv"} {
		path := filepath.Join(dir, payload.RunName, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}
}

func TestRunsEndpointListsAndServesDetail(t *testing.T) {
	server, _ := newTestServer(t)

	// Empty to begin with.
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("empty runs list = %d %q", recorder.Code, recorder.Body.String())
	}

	// Create one run through the API.
	create := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/backtest",
		strings.NewReader(`{"symbol":"TEST","strategy":"breakout","days":250,"seed":7}`))
	server.Handler().ServeHTTP(create, request)
	if create.Code != http.StatusOK {
		t.Fatalf("create run: %d %s", create.Code, create.Body.String())
	}
	var created struct {
		RunName string `json:"run_name"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// It must now appear in the list.
	list := httptest.NewRecorder()
	server.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	var runs []struct {
		Name   string `json:"name"`
		Symbol string `json:"symbol"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Name != created.RunName {
		t.Fatalf("runs = %+v, want the created run %q", runs, created.RunName)
	}

	// And its detail must be loadable.
	detail := httptest.NewRecorder()
	server.Handler().ServeHTTP(detail,
		httptest.NewRequest(http.MethodGet, "/api/run?name="+created.RunName, nil))
	if detail.Code != http.StatusOK {
		t.Fatalf("run detail: %d %s", detail.Code, detail.Body.String())
	}
	if !strings.Contains(detail.Body.String(), "equity") {
		t.Error("run detail is missing the equity curve")
	}
}

func TestRunDetailRejectsPathTraversal(t *testing.T) {
	server, _ := newTestServer(t)
	for _, name := range []string{"..", "../secrets", "a/b", `a\b`} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder,
			httptest.NewRequest(http.MethodGet, "/api/run?name="+name, nil))
		if recorder.Code == http.StatusOK {
			t.Errorf("name %q was accepted, want a rejection", name)
		}
	}
}

func TestBacktestEndpointRejectsBadInput(t *testing.T) {
	server, _ := newTestServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"invalid JSON", `{"symbol":`},
		{"unknown strategy", `{"symbol":"TEST","strategy":"nope","days":100}`},
		{"unknown provider", `{"symbol":"TEST","provider":"bloomberg","days":100}`},
		{"csv without a path", `{"symbol":"TEST","provider":"csv"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder,
				httptest.NewRequest(http.MethodPost, "/api/backtest", strings.NewReader(tc.body)))
			if recorder.Code == http.StatusOK {
				t.Errorf("expected an error, got 200: %s", recorder.Body.String())
			}
		})
	}
}

func TestBacktestEndpointServesCsvFixture(t *testing.T) {
	server, _ := newTestServer(t)
	fixture, err := filepath.Abs(filepath.Join("..", "..", "testdata", "bars.csv"))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"symbol":"TEST","strategy":"ma_cross","provider":"csv","data_path":` +
		strconv.Quote(fixture) + `}`

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/api/backtest", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Bars    int `json:"bars"`
		Metrics struct {
			NumTrades int `json:"num_trades"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	// The frozen fixture has a known trade count; this pins the UI path to the
	// same engine results the CLI produces.
	if payload.Bars != 730 || payload.Metrics.NumTrades != 46 {
		t.Errorf("bars=%d trades=%d, want 730/46", payload.Bars, payload.Metrics.NumTrades)
	}
}
