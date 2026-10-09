package broker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

// exchangeClock tracks how far the local machine's clock is from the
// exchange's.
//
// Binance rejects a signed request whose timestamp is more than 1000 ms ahead
// of its own clock ("code -1021: Timestamp for this request was 1000ms ahead
// of the server's time"). A machine with a drifting, sleeping or manually set
// clock therefore cannot trade at all, even though the keys, permissions and
// signature are all correct — and the failure lands on the first signed call,
// which is leverage setup during Init.
//
// The offset is measured once from the exchange's own public /time endpoint
// and added to every signed timestamp, so correctness no longer depends on the
// host clock being right.
type exchangeClock struct {
	// offsetMs is the exchange's clock minus the local one, in milliseconds.
	// atomic because cycles and the UI can read it while a request signs.
	offsetMs atomic.Int64
}

// now returns the local time corrected by the measured offset.
func (c *exchangeClock) now() time.Time {
	return time.Now().Add(time.Duration(c.offsetMs.Load()) * time.Millisecond)
}

// timestamp renders the corrected time the way signed Binance requests want it.
func (c *exchangeClock) timestamp() string {
	return strconv.FormatInt(c.now().UnixMilli(), 10)
}

// offset returns the measured offset in milliseconds (exchange minus local).
func (c *exchangeClock) offset() int64 { return c.offsetMs.Load() }

// measure asks the exchange for its clock and records the offset. The round
// trip is halved before comparing, so ordinary network latency is not mistaken
// for skew — without that correction a slow link would look like a fast local
// clock and push every timestamp into the past.
func (c *exchangeClock) measure(client *http.Client, endpoint string) error {
	start := time.Now()
	response, err := client.Get(endpoint)
	if err != nil {
		return fmt.Errorf("请求交易所时间失败: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return httpError(response.StatusCode, body)
	}

	localMid := start.Add(time.Since(start) / 2)
	var payload struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("解析交易所时间失败: %w", err)
	}
	if payload.ServerTime <= 0 {
		return fmt.Errorf("交易所返回了无效时间戳 %d", payload.ServerTime)
	}
	c.offsetMs.Store(payload.ServerTime - localMid.UnixMilli())
	return nil
}

// signedQuery renders the query string for a private Binance request: the
// sorted parameters, a timestamp taken from the exchange-corrected clock, and
// the HMAC signature.
//
// The signature is appended last, deliberately, rather than set as another map
// entry. Binance verifies the HMAC over everything that precedes "signature",
// so the parameter must be sent last — but url.Values.Encode() sorts keys
// alphabetically, which puts "signature" ahead of "timestamp". The perpetual
// API rejects that ordering with
// "code -1022: Signature for this request is not valid", which is
// indistinguishable from a wrong secret key and costs an hour of debugging.
// Building the string here keeps the order correct on every endpoint.
func signedQuery(secret, timestamp string, query url.Values) string {
	if query == nil {
		query = url.Values{}
	}
	query.Set("timestamp", timestamp)
	payload := query.Encode()
	return payload + "&signature=" + sign(secret, payload)
}

// signAndRetry performs one signed attempt and, when the exchange refuses the
// timestamp, re-syncs the clock and tries exactly once more.
//
// Retrying is safe for precisely this error: -1021 is rejected on inspection,
// before the matching engine ever sees the request, so the refused attempt
// cannot have created an order. Every other failure keeps its own
// reconciliation path — for orders that is the client-order-id lookup, never a
// blind resubmit.
func signAndRetry(sync func() error, attempt func() ([]byte, error)) ([]byte, error) {
	body, err := attempt()
	if err == nil || !isTimestampError(err) {
		return body, err
	}
	if syncErr := sync(); syncErr != nil {
		return nil, fmt.Errorf("%w；重新校时失败: %v", err, syncErr)
	}
	return attempt()
}
