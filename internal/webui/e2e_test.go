package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestServerEditionE2ESmoke is the P2-2 smoke: it boots the server-edition
// HTTP stack on a real TCP listener (via httptest.NewServer) with the
// offline SeriesLoader, then drives it with a real http.Client through
// /healthz -> /api/config -> a backtest. Every step must answer 200, proving
// the server edition serves end to end with zero real-network access (stub
// data from testfx, no LLM key, paper mode with TA_ALLOW_LIVE unset).
func TestServerEditionE2ESmoke(t *testing.T) {
	server, _ := newTestServer(t)
	// TA_ALLOW_LIVE is unset in the test process, so the session gate stays
	// closed; a paper backtest is unaffected by that gate.

	// A real listener behind the full handler chain. The loader is fxLoader,
	// so no call reaches Binance.
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := httpServer.Client()

	// 1) health probe
	if code, err := httpGetStatus(client, httpServer.URL+"/healthz"); code != http.StatusOK || err != nil {
		t.Fatalf("GET /healthz = %d (%v), want 200", code, err)
	}

	// 2) config: the server edition describes its base config + strategies
	var cfg struct {
		Symbol     string `json:"symbol"`
		Desktop    bool   `json:"desktop"`
		Strategies []any  `json:"strategies"`
	}
	if code, err := httpGetJSON(client, httpServer.URL+"/api/config", &cfg); code != http.StatusOK || err != nil {
		t.Fatalf("GET /api/config = %d (%v), want 200", code, err)
	}
	if cfg.Symbol != "TEST" {
		t.Errorf("config symbol = %q, want TEST", cfg.Symbol)
	}
	if len(cfg.Strategies) == 0 {
		t.Error("config lists no strategies")
	}

	// 3) run a backtest end to end and assert 200 + a concrete result
	result, err := postBacktest(client, httpServer.URL, `{"symbol":"TEST","strategy":"ma_cross","days":200,"warmup_bars":30}`)
	if err != nil {
		t.Fatalf("POST /api/backtest: %v", err)
	}
	if result.Symbol != "TEST" {
		t.Errorf("backtest symbol = %q, want TEST", result.Symbol)
	}
	if result.Bars != 200 {
		t.Errorf("backtest bars = %d, want 200", result.Bars)
	}
}

// resultView is the slice of a /api/backtest response the smoke asserts on.
type resultView struct {
	Symbol string `json:"symbol"`
	Bars   int    `json:"bars"`
}

func postBacktest(client *http.Client, base, body string) (resultView, error) {
	var out resultView
	request, err := http.NewRequest(http.MethodPost, base+"/api/backtest", strings.NewReader(body))
	if err != nil {
		return out, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return out, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return out, errfStatus(response.StatusCode)
	}
	err = json.NewDecoder(response.Body).Decode(&out)
	return out, err
}

func httpGetStatus(client *http.Client, url string) (int, error) {
	response, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func httpGetJSON(client *http.Client, url string, dst any) (int, error) {
	response, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, errfStatus(response.StatusCode)
	}
	err = json.NewDecoder(response.Body).Decode(dst)
	return response.StatusCode, err
}

// errfStatus is a tiny error type so a non-200 is distinguishable in logs.
type errfStatus int

func (e errfStatus) Error() string { return http.StatusText(int(e)) }
