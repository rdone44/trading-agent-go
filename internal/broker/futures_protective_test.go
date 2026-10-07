package broker

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// protectiveStub records every request the futures broker makes so a test can
// assert exactly which protective orders were placed, cancelled and read. It
// keeps a request log (method + path) and the query values for each, and lets
// a test control what /fapi/v1/openOrders returns so HasProtective can be
// exercised both ways.
type protectiveStub struct {
	mu         sync.Mutex
	reqPaths   []string
	reqQuery   []url.Values
	openOrders string // body returned for GET /fapi/v1/openOrders
}

func newProtectiveStub(openOrders string) *protectiveStub {
	return &protectiveStub{openOrders: openOrders, reqQuery: []url.Values{}}
}

func (s *protectiveStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqPaths = append(s.reqPaths, r.Method+" "+r.URL.Path)
		s.reqQuery = append(s.reqQuery, r.URL.Query())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/fapi/v1/openOrders" {
			_, _ = w.Write([]byte(s.openOrders))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
}

// queriesFor returns the query values of every request to method+path, in the
// order the requests arrived.
func (s *protectiveStub) queriesFor(method, path string) []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []url.Values
	for i, p := range s.reqPaths {
		if p == method+" "+path {
			out = append(out, s.reqQuery[i])
		}
	}
	return out
}

func (s *protectiveStub) called(method, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.reqPaths {
		if p == method+" "+path {
			return true
		}
	}
	return false
}

// newLiveFuturesForStub builds a non-dry-run futures broker that talks to the
// stub. No Init is called, so nothing touches leverage/margin-type; the
// protective methods are exercised directly.
func newLiveFuturesForStub(baseURL string) *FuturesBroker {
	return NewFutures(FuturesConfig{
		Symbol: "BTCUSDT", APIKey: "key", SecretKey: "secret",
		StepSize: 0.001, BaseURL: baseURL,
	})
}

// A long with both a stop and a target places two independent closePosition
// legs: a STOP_MARKET closing via SELL and a TAKE_PROFIT_MARKET closing via
// SELL. Each carries the exact level the risk engine supplied — never a
// recomputed price.
func TestPlaceProtectiveLongPutsStopAndTarget(t *testing.T) {
	s := newProtectiveStub(`[]`)
	srv := s.server()
	defer srv.Close()
	b := newLiveFuturesForStub(srv.URL)

	if err := b.PlaceProtective(Buy, 90, 110); err != nil {
		t.Fatalf("PlaceProtective(long) = %v, want nil", err)
	}
	qs := s.queriesFor(http.MethodPost, "/fapi/v1/order")
	if len(qs) != 2 {
		t.Fatalf("want 2 protective legs, got %d", len(qs))
	}
	var sawStop, sawTarget bool
	for _, q := range qs {
		if q.Get("closePosition") != "true" {
			t.Errorf("closePosition=%q, want true", q.Get("closePosition"))
		}
		if q.Get("workingType") != "MARK_PRICE" {
			t.Errorf("workingType=%q, want MARK_PRICE", q.Get("workingType"))
		}
		if q.Get("side") != "SELL" {
			t.Errorf("a long closes via SELL, got %q", q.Get("side"))
		}
		switch q.Get("type") {
		case "STOP_MARKET":
			sawStop = true
			if q.Get("stopPrice") != "90" {
				t.Errorf("stopPrice=%q, want 90 (the risk engine's own level)", q.Get("stopPrice"))
			}
		case "TAKE_PROFIT_MARKET":
			sawTarget = true
			if q.Get("price") != "110" {
				t.Errorf("target price=%q, want 110", q.Get("price"))
			}
		}
	}
	if !sawStop || !sawTarget {
		t.Errorf("sawStop=%v sawTarget=%v, want both legs", sawStop, sawTarget)
	}
}

// A short with only a stop places a single STOP_MARKET that closes via BUY;
// the absent target must not produce a second leg.
func TestPlaceProtectiveShortStopOnly(t *testing.T) {
	s := newProtectiveStub(`[]`)
	srv := s.server()
	defer srv.Close()
	b := newLiveFuturesForStub(srv.URL)

	if err := b.PlaceProtective(Sell, 110, 0); err != nil {
		t.Fatalf("PlaceProtective(short, stop-only) = %v, want nil", err)
	}
	qs := s.queriesFor(http.MethodPost, "/fapi/v1/order")
	if len(qs) != 1 {
		t.Fatalf("want exactly 1 leg for a stop-only position, got %d", len(qs))
	}
	q := qs[0]
	if q.Get("type") != "STOP_MARKET" || q.Get("side") != "BUY" {
		t.Errorf("short stop = type %q side %q, want STOP_MARKET/BUY", q.Get("type"), q.Get("side"))
	}
	if q.Get("stopPrice") != "110" {
		t.Errorf("stopPrice=%q, want 110", q.Get("stopPrice"))
	}
}

// No protective order is ever placed for a dry-run broker: the whole method
// is a no-op and returns nil, keeping paper and backtest paths untouched.
func TestPlaceProtectiveDryRunNoOp(t *testing.T) {
	b := NewFutures(FuturesConfig{DryRun: true, Symbol: "BTCUSDT", StepSize: 0.001})
	if err := b.PlaceProtective(Buy, 90, 110); err != nil {
		t.Fatalf("dry-run PlaceProtective = %v, want nil", err)
	}
	if err := b.CancelProtective(); err != nil {
		t.Fatalf("dry-run CancelProtective = %v, want nil", err)
	}
	if has, err := b.HasProtective(); err != nil || has {
		t.Fatalf("dry-run HasProtective = %v/%v, want false/nil", has, err)
	}
}

// HasProtective inspects the symbol's open orders and reports true when a
// closePosition protective leg is present, so a restart never double-places.
func TestHasProtectiveDetectsOpenStop(t *testing.T) {
	body := `[{"symbol":"BTCUSDT","type":"STOP_MARKET","closePosition":true}]`
	s := newProtectiveStub(body)
	srv := s.server()
	defer srv.Close()
	b := newLiveFuturesForStub(srv.URL)

	has, err := b.HasProtective()
	if err != nil || !has {
		t.Fatalf("HasProtective = %v/%v, want true/nil", has, err)
	}
}

// With no protective order open (an unrelated LIMIT resting there), Has reports
// false so reconcile does not cancel something that is not a protective stop.
func TestHasProtectiveFalseWhenNoStop(t *testing.T) {
	body := `[{"symbol":"BTCUSDT","type":"LIMIT","closePosition":false}]`
	s := newProtectiveStub(body)
	srv := s.server()
	defer srv.Close()
	b := newLiveFuturesForStub(srv.URL)

	has, err := b.HasProtective()
	if err != nil || has {
		t.Fatalf("HasProtective = %v/%v, want false/nil", has, err)
	}
}

// CancelProtective removes every open order on the symbol via the exchange's
// allOpenOrders endpoint, so a local flatten is not double-closed by a leg.
func TestCancelProtectiveDeletesAllOpenOrders(t *testing.T) {
	s := newProtectiveStub(`[]`)
	srv := s.server()
	defer srv.Close()
	b := newLiveFuturesForStub(srv.URL)

	if err := b.CancelProtective(); err != nil {
		t.Fatalf("CancelProtective = %v, want nil", err)
	}
	if !s.called(http.MethodDelete, "/fapi/v1/allOpenOrders") {
		t.Fatal("CancelProtective must DELETE /fapi/v1/allOpenOrders")
	}
}
