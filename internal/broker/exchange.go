package broker

// The exchange primitives both venues need: request signing, the error type
// that carries Binance's numeric code, and the order-confirmation shape.
//
// These used to live in binance.go, which was the spot client. The spot venue
// is retired, but the futures client still depends on all of this, so the
// pieces were lifted out rather than deleted with their old home.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// sign produces the HMAC-SHA256 signature for a query string.
func sign(secret, query string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(query))
	return hex.EncodeToString(mac.Sum(nil))
}

// binanceError is a non-200 response from the exchange. The code is kept as a
// field, not just text, so the timestamp failure (-1021) can be recognised and
// answered with a clock re-sync instead of a guess at the message wording.
type binanceError struct {
	Status int
	Code   int32
	Msg    string
}

func (e *binanceError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("Binance HTTP %d (code %d): %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("Binance HTTP %d", e.Status)
}

// isTimestampError reports whether the exchange rejected the request because
// its timestamp fell outside the accepted window.
func isTimestampError(err error) bool {
	var exchangeErr *binanceError
	if errors.As(err, &exchangeErr) {
		return exchangeErr.Code == -1021
	}
	return false
}

// httpError extracts the exchange message from a failed response.
func httpError(status int, body []byte) error {
	var payload struct {
		Code int32  `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(body, &payload)
	return &binanceError{Status: status, Code: payload.Code, Msg: payload.Msg}
}

// orderDetail is the subset of a Binance order we need to confirm a fill.
type orderDetail struct {
	executed   float64
	avgPrice   float64
	commission float64
	status     string
}
