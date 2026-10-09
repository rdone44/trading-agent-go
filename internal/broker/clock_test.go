package broker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The regression this file exists for: a host clock more than a second ahead
// of Binance's made every signed request fail with
// "code -1021: Timestamp for this request was 1000ms ahead of the server's
// time", starting with the leverage call in Init, so the session could not
// start at all. Signing must follow the exchange's clock, not the host's.
func TestFuturesInitSignsWithTheExchangeClock(t *testing.T) {
	// The exchange believes it is two hours ahead of this host.
	const skew = 2 * time.Hour
	serverTime := time.Now().Add(skew).UnixMilli()
	var stamped int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/time":
			fmt.Fprintf(w, `{"serverTime":%d}`, serverTime)
		case "/fapi/v1/leverage":
			stamped, _ = strconv.ParseInt(r.URL.Query().Get("timestamp"), 10, 64)
			// Reject anything outside the exchange's 1s window, exactly as
			// Binance does, so a host-clock signature fails this test.
			if delta := stamped - serverTime; delta > 1000 || delta < -1000 {
				w.WriteHeader(400)
				fmt.Fprintf(w, `{"code":-1021,"msg":"Timestamp for this request was 1000ms ahead of the server's time."}`)
				return
			}
			fmt.Fprint(w, `{}`)
		case "/fapi/v1/marginType":
			w.WriteHeader(400)
			fmt.Fprint(w, `{"code":-4046,"msg":"No need to change margin type."}`)
		case "/fapi/v1/exchangeInfo":
			fmt.Fprint(w, `{"symbols":[{"symbol":"BTCUSDT","quantityPrecision":3,"filters":[{"filterType":"LOT_SIZE","stepSize":"0.001"}]}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	b := NewFutures(FuturesConfig{BaseURL: server.URL, Symbol: "BTCUSDT", Leverage: 5})
	if err := b.Init(); err != nil {
		t.Fatalf("Init with a skewed host clock: %v", err)
	}
	if stamped == 0 {
		t.Fatal("leverage request was never signed")
	}
	if delta := stamped - serverTime; delta > 1000 || delta < -1000 {
		t.Fatalf("timestamp %d is %dms from the exchange's clock, want within 1s", stamped, delta)
	}
}

// A clock that drifts after Init must recover on its own: the exchange answers
// -1021, the broker re-syncs and retries once, and the retry succeeds.
func TestFuturesRetriesOnceAfterTimestampRejection(t *testing.T) {
	var timeCalls, orderCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/time":
			timeCalls++
			fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
		case "/fapi/v2/balance":
			orderCalls++
			if orderCalls == 1 {
				// The first attempt is refused on its timestamp.
				w.WriteHeader(400)
				fmt.Fprint(w, `{"code":-1021,"msg":"Timestamp for this request was 1000ms ahead of the server's time."}`)
				return
			}
			fmt.Fprint(w, `{"totalWalletBalance":"100"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	b := NewFutures(FuturesConfig{BaseURL: server.URL, Symbol: "BTCUSDT"})
	// Seed a wrong offset, as a host clock drifting after Init would.
	b.clock.offsetMs.Store(-60_000)
	if _, err := b.getSigned("/fapi/v2/balance", url.Values{}); err != nil {
		t.Fatalf("a -1021 rejection must be answered with a re-sync and one retry: %v", err)
	}
	if orderCalls != 2 {
		t.Fatalf("balance requested %d times, want exactly 2 (one refused, one retried)", orderCalls)
	}
	if timeCalls != 1 {
		t.Fatalf("clock synced %d times, want exactly 1", timeCalls)
	}
}

// The retry is specific to the timestamp error. Retrying anything else would
// risk a second order, so other failures must be returned untouched.
func TestFuturesDoesNotRetryOtherErrors(t *testing.T) {
	var orderCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/time":
			fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
		default:
			orderCalls++
			w.WriteHeader(400)
			fmt.Fprint(w, `{"code":-2019,"msg":"Margin is insufficient."}`)
		}
	}))
	defer server.Close()

	b := NewFutures(FuturesConfig{BaseURL: server.URL, Symbol: "BTCUSDT"})
	_, err := b.getSigned("/fapi/v2/balance", url.Values{})
	if err == nil {
		t.Fatal("a -2019 rejection must surface as an error")
	}
	if orderCalls != 1 {
		t.Fatalf("balance requested %d times; a non-timestamp error must not be retried", orderCalls)
	}
}

// The offset must be measured from the middle of the round trip. Comparing the
// server's stamp against the moment the response *arrived* would read ordinary
// network latency as clock skew, and on a slow link that error alone can push
// every timestamp outside the exchange's 1s window.
func TestClockOffsetDiscountsRoundTripLatency(t *testing.T) {
	// A symmetric 400ms round trip: the request takes half of it to arrive and
	// the response takes the other half to come back, so the server's stamp
	// lands on the midpoint the client measures.
	const leg = 200 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(leg)
		stamp := time.Now().UnixMilli()
		time.Sleep(leg)
		fmt.Fprintf(w, `{"serverTime":%d}`, stamp)
	}))
	defer server.Close()

	var clock exchangeClock
	if err := clock.measure(server.Client(), server.URL); err != nil {
		t.Fatalf("measure: %v", err)
	}
	// The clocks genuinely agree, so the estimate must be ~0. Charging the
	// whole round trip to skew would report about -400ms here.
	if offset := clock.offset(); offset > 120 || offset < -120 {
		t.Fatalf("offset = %dms, want ~0 (the round trip must not be read as skew)", offset)
	}
}

// A host whose clock is already correct must not be blocked from trading just
// because the clock endpoint is unreachable: Init tolerates that, and the
// signed path only complains if the exchange actually rejects a timestamp.
func TestClockSyncFailureDoesNotBlockInit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/time":
			w.WriteHeader(503)
		case "/fapi/v1/leverage":
			fmt.Fprint(w, `{}`)
		case "/fapi/v1/marginType":
			w.WriteHeader(400)
			fmt.Fprint(w, `{"code":-4046,"msg":"No need to change margin type."}`)
		case "/fapi/v1/exchangeInfo":
			fmt.Fprint(w, `{"symbols":[{"symbol":"BTCUSDT","quantityPrecision":3,"filters":[{"filterType":"LOT_SIZE","stepSize":"0.001"}]}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	b := NewFutures(FuturesConfig{BaseURL: server.URL, Symbol: "BTCUSDT"})
	if err := b.Init(); err != nil {
		t.Fatalf("Init must tolerate an unreadable clock endpoint: %v", err)
	}
}

// When the clock really is wrong and cannot be re-read, the failure must name
// both problems rather than silently retrying forever.
func TestClockResyncFailureIsReportedWithTheOriginalError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fapi/v1/time" {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(400)
		fmt.Fprint(w, `{"code":-1021,"msg":"Timestamp for this request was 1000ms ahead of the server's time."}`)
	}))
	defer server.Close()

	b := NewFutures(FuturesConfig{BaseURL: server.URL, Symbol: "BTCUSDT"})
	_, err := b.getSigned("/fapi/v2/balance", url.Values{})
	if err == nil {
		t.Fatal("an unrecoverable timestamp failure must be reported")
	}
	if !strings.Contains(err.Error(), "-1021") || !strings.Contains(err.Error(), "重新校时失败") {
		t.Fatalf("error = %q, want both the exchange rejection and the re-sync failure", err)
	}
}

// The timestamp error is recognised by its code, not by matching the message
// text, so a rewording on the exchange's side cannot silently disable recovery.
func TestTimestampErrorDetectionUsesTheCode(t *testing.T) {
	cases := map[string]struct {
		body []byte
		want bool
	}{
		"timestamp":     {[]byte(`{"code":-1021,"msg":"whatever the wording"}`), true},
		"other code":    {[]byte(`{"code":-2019,"msg":"Margin is insufficient."}`), false},
		"not json":      {[]byte(`<html>gateway</html>`), false},
		"empty message": {[]byte(`{"code":-1021}`), true},
	}
	for name, c := range cases {
		err := httpError(400, c.body)
		if got := isTimestampError(err); got != c.want {
			t.Errorf("%s: isTimestampError = %v, want %v", name, got, c.want)
		}
	}
	if isTimestampError(nil) {
		t.Error("a nil error is not a timestamp error")
	}
}

// The signature must be the last parameter of the query string.
//
// Binance verifies the HMAC over everything preceding "signature", so the
// parameter has to come last. url.Values.Encode() sorts keys alphabetically,
// which puts "signature" before "timestamp" — the API answers
// "code -1022: Signature for this request is not
// valid", a message that looks exactly like a wrong secret key. Pinning the
// rendering here keeps a refactor from quietly reintroducing it.
func TestSignedQueryPutsTheSignatureLast(t *testing.T) {
	query := url.Values{"symbol": {"BTCUSDT"}, "side": {"BUY"}}
	signed := signedQuery("secret", "1700000000000", query)

	// Exactly one signature parameter, at the very end.
	if got := strings.Count(signed, "signature="); got != 1 {
		t.Fatalf("signed query %q has %d signature parameters, want exactly 1", signed, got)
	}
	marker := strings.LastIndex(signed, "&signature=")
	if marker < 0 {
		t.Fatalf("signed query %q has no signature parameter", signed)
	}

	// The signed payload is the part before the marker, and the signature
	// appended after it must be the HMAC of exactly that payload.
	payload := signed[:marker]
	if payload != "side=BUY&symbol=BTCUSDT&timestamp=1700000000000" {
		t.Fatalf("payload = %q, want the sorted parameters plus the timestamp", payload)
	}
	if got, want := signed[marker+len("&signature="):], sign("secret", payload); got != want {
		t.Fatalf("signature = %q, want the HMAC of %q", got, payload)
	}
}

// A nil query is the shape most read-only calls use; it must not panic.
func TestSignedQueryHandlesNilValues(t *testing.T) {
	signed := signedQuery("secret", "1700000000000", nil)
	if signed != "timestamp=1700000000000&signature="+sign("secret", "timestamp=1700000000000") {
		t.Fatalf("signed query = %q, want just the timestamp and its signature", signed)
	}
}
