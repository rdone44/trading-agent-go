package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/strategy"
	"github.com/rdone44/trading-agent-go/internal/testfx"
	"github.com/rdone44/trading-agent-go/internal/tune"
	"github.com/rdone44/trading-agent-go/internal/webui"
)

// fxLoader is the offline SeriesLoader: deterministic bars, no network.
func fxLoader(symbol string, days int, end time.Time) (model.Series, error) {
	return testfx.Bars(symbol, days, 42, end), nil
}

func newTestServer(t *testing.T) (*webui.Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Agent.Symbol = "TEST"
	cfg.Agent.HistoryDays = 300
	cfg.Backtest.WarmupBars = 30
	cfg.Backtest.OutputDir = dir
	// Keep the session's state file inside the test's temp dir. Without this a
	// lifecycle test drops trade-state.json into the package directory, where
	// it leaks into `git status` and can be picked up by the next test run.
	cfg.Live.StateFile = filepath.Join(dir, "trade-state.json")
	server := webui.New(cfg)
	server.SeriesLoader = fxLoader
	return server, dir
}

func TestDashboardPageIsServed(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{"trading-agent", "开始交易", "/app.js", "/app.css"} {
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

// TestUIPageAndScriptAgree guards the seam between index.html and app.js. The
// page and the script are edited independently, and a selector that no longer
// matches the markup fails silently in the browser — the page renders, then a
// lookup returns null and the whole render throws. Asserting every id and
// field name the script reaches for actually exists in the served page turns
// that runtime failure into a test failure.
func TestUIPageAndScriptAgree(t *testing.T) {
	server, _ := newTestServer(t)
	page := fetchBody(t, server, "/")
	script := fetchBody(t, server, "/app.js")

	ids := matchSet(page, `id="([^"]+)"`)
	names := matchSet(page, `name="([^"]+)"`)
	if len(ids) == 0 || len(names) == 0 {
		t.Fatal("index.html exposed no ids or field names; the extraction is broken")
	}

	// Selector strings passed to $() and $$() are the script's handles on the
	// markup; every #id inside one must resolve.
	for selector := range matchSet(script, `\$\$?\(\s*["']([^"']+)["']`) {
		for _, id := range matchAll(selector, `#([A-Za-z][A-Za-z0-9_-]*)`) {
			if !ids[id] {
				t.Errorf("app.js selects #%s but index.html has no such id", id)
			}
		}
	}

	// The form is read by field name, so a renamed input breaks the request
	// the same silent way. Skip interpolated selectors like [name="${x}"].
	for name := range matchSet(script, `\[name="([^"$]+)"\]`) {
		if !names[name] {
			t.Errorf("app.js reads field %q but index.html has no such input", name)
		}
	}

	// The two log tabs are addressed by data-tab, which is neither an id nor a
	// name, so check them explicitly.
	tabs := matchSet(page, `data-tab="([^"]+)"`)
	for _, tab := range []string{"cycles", "trades", "orders"} {
		if !tabs[tab] {
			t.Errorf("index.html is missing the %q log tab", tab)
		}
		if !strings.Contains(script, `"`+tab+`"`) {
			t.Errorf("app.js never handles the %q log tab", tab)
		}
	}
}

func fetchBody(t *testing.T, server *webui.Server, path string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, recorder.Code)
	}
	return recorder.Body.String()
}

// matchSet returns the first capture group of every match, as a set.
func matchSet(text, pattern string) map[string]bool {
	out := map[string]bool{}
	for _, group := range matchAll(text, pattern) {
		out[group] = true
	}
	return out
}

func matchAll(text, pattern string) []string {
	re := regexp.MustCompile(pattern)
	var out []string
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			out = append(out, m[1])
		}
	}
	return out
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
	if len(payload.Strategies) != len(strategy.Specs()) {
		t.Fatalf("strategies = %d, want %d (len of strategy.Specs)", len(payload.Strategies), len(strategy.Specs()))
	}
	for _, spec := range payload.Strategies {
		if spec.Name == "" || spec.Title == "" || len(spec.Params) == 0 {
			t.Errorf("incomplete strategy spec: %+v", spec)
		}
	}
}

func TestBacktestEndpointRunsAndPersists(t *testing.T) {
	server, dir := newTestServer(t)
	body := `{"symbol":"TEST","strategy":"ma_cross","days":300,
	          "initial_cash":100000,"warmup_bars":30,
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
		strings.NewReader(`{"symbol":"TEST","strategy":"breakout","days":250}`))
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

func TestBacktestEndpointRunsOffline(t *testing.T) {
	server, _ := newTestServer(t)
	body := `{"symbol":"TEST","strategy":"ma_cross","days":200,"warmup_bars":30}`

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/api/backtest", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Bars int `json:"bars"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Bars != 200 {
		t.Errorf("bars = %d, want 200", payload.Bars)
	}
}

// TestTuneEndpointReturnsReport exercises /api/tune with an injected
// TuneRunner (no real model call) and verifies the handler threads the
// request's options through and returns the report as JSON.
func TestTuneEndpointReturnsReport(t *testing.T) {
	server, _ := newTestServer(t)
	// Stub the tuning runner so the test never dials a model.
	server.TuneRunner = func(cfg config.Config, series model.Series, opts tune.Options) (tune.Report, error) {
		return tune.Report{
			Objective: opts.Objective,
			Strategy:  cfg.Strategy.Name,
			Symbol:    cfg.Agent.Symbol,
			Rounds: []tune.RoundResult{
				{Index: 0, ObjectiveValue: 0.5, Improved: false, Note: "baseline"},
				{Index: 1, ObjectiveValue: 0.9, Improved: true, Rationale: "tighter exit"},
			},
			Baseline:   0.5,
			BestValue:  0.9,
			BestParams: map[string]float64{"fast": 8, "slow": 24},
		}, nil
	}

	body := `{"symbol":"TEST","strategy":"ma_cross","days":200,"objective":"sharpe","rounds":4,"stall":3}`
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/api/tune", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Objective string  `json:"objective"`
		Strategy  string  `json:"strategy"`
		Baseline  float64 `json:"baseline"`
		BestValue float64 `json:"best_value"`
		Rounds    []struct {
			Index    int  `json:"index"`
			Improved bool `json:"improved"`
		} `json:"rounds"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Objective != "sharpe" || payload.Strategy != "ma_cross" {
		t.Errorf("objective/strategy = %q/%q, want sharpe/ma_cross", payload.Objective, payload.Strategy)
	}
	if len(payload.Rounds) != 2 || !payload.Rounds[1].Improved {
		t.Errorf("rounds not threaded through: %+v", payload.Rounds)
	}
}

// TestTuneEndpointRejectsWrongMethod guards the POST-only contract.
func TestTuneEndpointRejectsWrongMethod(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/tune", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

// TestTunePromptEndpointReturnsReport exercises /api/tune-prompt with an
// injected PromptTuneRunner (no real model call) and verifies the handler
// threads the request options through and returns the report as JSON.
func TestTunePromptEndpointReturnsReport(t *testing.T) {
	server, _ := newTestServer(t)
	server.PromptTuneRunner = func(cfg config.Config, series model.Series, opts tune.PromptOptions) (tune.PromptReport, error) {
		return tune.PromptReport{
			Objective:  opts.Objective,
			Symbol:     cfg.Agent.Symbol,
			LLMEnabled: true,
			Rounds:     []tune.PromptRoundResult{{Index: 0, ObjectiveValue: 0.5}, {Index: 1, ObjectiveValue: 0.9, Improved: true}},
			Baseline:   0.5,
			BestValue:  0.9,
			BestPrompt: "a better persona",
		}, nil
	}
	body := `{"symbol":"TEST","strategy":"llm","days":200,"objective":"sortino","rounds":2,"stall":3,"cv_folds":2,"prompt":"starter"}`
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/api/tune-prompt", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Objective  string  `json:"objective"`
		Symbol     string  `json:"symbol"`
		LLMEnabled bool    `json:"llm_enabled"`
		BestPrompt string  `json:"best_prompt"`
		Rounds     []struct {
			Index    int     `json:"index"`
			Improved bool    `json:"improved"`
			Value    float64 `json:"objective_value"`
		} `json:"rounds"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Objective != "sortino" || !payload.LLMEnabled {
		t.Errorf("objective/llm = %q/%v, want sortino/true", payload.Objective, payload.LLMEnabled)
	}
	if payload.BestPrompt != "a better persona" {
		t.Errorf("best_prompt = %q, want the winning persona", payload.BestPrompt)
	}
	if len(payload.Rounds) != 2 || !payload.Rounds[1].Improved {
		t.Errorf("rounds not threaded through: %+v", payload.Rounds)
	}
}

// TestTunePromptEndpointRejectsNonLLMStrategy guards the scope: the persona
// only exists for the llm strategy, so the endpoint must refuse others.
func TestTunePromptEndpointRejectsNonLLMStrategy(t *testing.T) {
	server, _ := newTestServer(t)
	server.PromptTuneRunner = func(config.Config, model.Series, tune.PromptOptions) (tune.PromptReport, error) {
		t.Fatal("runner must not be reached for a non-llm strategy")
		return tune.PromptReport{}, nil
	}
	body := `{"symbol":"TEST","strategy":"ma_cross","days":200}`
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/api/tune-prompt", strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
}

// TestTunePromptEndpointRejectsWrongMethod guards the POST-only contract.
func TestTunePromptEndpointRejectsWrongMethod(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/tune-prompt", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}
// TestBacktestReviewUnavailableWhenNoKey verifies the degraded path: asking for
// a review with no LLM key must not fail the backtest, and must surface the
// unavailability note instead of a review.
func TestBacktestReviewUnavailableWhenNoKey(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	server, _ := newTestServer(t)

	body := `{"symbol":"TEST","strategy":"ma_cross","days":200,"review":true}`
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/api/backtest", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s (a review failure must not fail the backtest)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Review            string `json:"review"`
		ReviewUnavailable string `json:"review_unavailable"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Review != "" {
		t.Errorf("review = %q, want empty when no key is set", payload.Review)
	}
	if payload.ReviewUnavailable == "" {
		t.Error("review_unavailable is empty; want a note explaining the missing model")
	}
}

// TestBacktestReviewOmittedByDefault confirms the field is simply absent when
// the request did not ask for a review (no note, no text).
func TestBacktestReviewOmittedByDefault(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	server, _ := newTestServer(t)

	body := `{"symbol":"TEST","strategy":"ma_cross","days":200}`
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/api/backtest", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	// omitempty means neither key appears in the JSON when no review was asked.
	if strings.Contains(recorder.Body.String(), "review") {
		t.Errorf("unexpected review field in response: %s", recorder.Body.String())
	}
}
