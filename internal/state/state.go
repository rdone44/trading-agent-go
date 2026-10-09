// Package state persists a live session to disk so the trade loop can be
// restarted without losing its open position, equity high-water mark or
// risk-manager state. The file is JSON with NaN-safe level encoding
// (a stop or target of 0 means "none set").
package state

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/risk"
)

// State is the on-disk representation of one live session.
type State struct {
	Version    int            `json:"version"`
	Symbol     string         `json:"symbol"`
	Strategy   string         `json:"strategy"`
	SavedAt    time.Time      `json:"saved_at"`
	Initial    float64        `json:"initial_cash"`
	Cash       float64        `json:"cash"`
	Peak       float64        `json:"peak_equity"`
	Open       *Open          `json:"open,omitempty"`
	Risk       risk.RiskState `json:"risk"`
	Executed   bool           `json:"executed"` // true when real orders are used
	Venue      string         `json:"venue,omitempty"`
	Leverage   int            `json:"leverage,omitempty"`
	Accounting string         `json:"accounting,omitempty"`
	// Owner is the account that created the file. In accounts mode a session
	// refuses to resume a ledger that belongs to a different account, so a
	// copied or misdirected state file cannot move one user's position into
	// another user's book. Empty in the single-user desktop / CLI builds,
	// where the historical behaviour is unchanged.
	Owner string `json:"owner,omitempty"`
}

// Open is the persisted open position.
type Open struct {
	EntryTime  time.Time `json:"entry_time"`
	EntryPrice float64   `json:"entry_price"`
	Quantity   float64   `json:"quantity"`
	Side       string    `json:"side"`
	EntryFee   float64   `json:"entry_fee"`
	Stop       float64   `json:"stop"`        // 0 = unset
	TakeProfit float64   `json:"take_profit"` // 0 = unset
}

// Save writes the state atomically: the new content goes to a temp file in
// the same directory and is then renamed over the target, so a crash mid-
// write never leaves a truncated file behind.
func Save(path string, s State) error {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.SavedAt.IsZero() {
		s.SavedAt = time.Now()
	}
	encoded, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return fmt.Errorf("write state %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace state %s: %w", path, err)
	}
	return nil
}

// Load reads a previously saved state. A missing file is not an error: it
// means there is no session to resume, and Load returns ok=false.
func Load(path string) (State, bool, error) {
	var s State
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, false, nil
		}
		return s, false, fmt.Errorf("read state %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, false, fmt.Errorf("parse state %s: %w", path, err)
	}
	return s, true, nil
}

// FromEngine serializes live agent state into on-disk form. Stop and target
// are the open position's protective levels; a NaN/Inf level is stored as 0
// because JSON cannot represent NaN, and ToEngine restores it.
func FromEngine(symbol, strategy string, initial, cash, peak float64, open *engine.OpenTrade, stop, target float64, riskState risk.RiskState, executed bool) State {
	s := State{
		Symbol: symbol, Strategy: strategy,
		Initial: initial, Cash: cash, Peak: peak,
		Risk: riskState, Executed: executed,
	}
	if open != nil {
		s.Open = &Open{
			EntryTime: open.EntryTime, EntryPrice: open.EntryPrice,
			Quantity: open.Quantity, Side: string(open.Side),
			EntryFee: open.EntryFee, Stop: cleanLevel(stop), TakeProfit: cleanLevel(target),
		}
	}
	return s
}

// cleanLevel maps an unusable protective level to 0 for JSON storage.
func cleanLevel(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// ToEngine rebuilds the agent state a restored session needs: cash and peak,
// the open trade (with NaN levels restored from the 0 markers), and the risk
// state. A session without an open position returns a nil trade.
func (s State) ToEngine() (cash, peak float64, open *engine.OpenTrade, stop, target float64, riskState risk.RiskState) {
	cash, peak, riskState = s.Cash, s.Peak, s.Risk
	stop, target = math.NaN(), math.NaN()
	if s.Open == nil {
		return
	}
	open = &engine.OpenTrade{
		EntryTime:  s.Open.EntryTime,
		EntryPrice: s.Open.EntryPrice,
		Quantity:   s.Open.Quantity,
		Side:       broker.Side(s.Open.Side),
		EntryFee:   s.Open.EntryFee,
	}
	stop = s.Open.Stop
	target = s.Open.TakeProfit
	if stop <= 0 {
		stop = math.NaN()
	}
	if target <= 0 {
		target = math.NaN()
	}
	return
}
