package session

import (
	"encoding/json"
	"testing"
)

// legacyBacktest / legacyRisk / legacySettings replicate, as plain structs,
// exactly what the pre-P2-1 webui types serialized to once Go's json encoder
// flattened the `json:",inline"` embedded BacktestRequest into StartSessionRequest
// (verified empirically: the embedded fields are promoted, in field order, and
// the outer struct's own fields follow). They exist only to pin the console
// wire format: Settings must serialize byte-identically to that legacy shape,
// otherwise the dashboard's settings echo would silently change.
type legacyBacktest struct {
	Symbol      string             `json:"symbol"`
	Strategy    string             `json:"strategy"`
	Params      map[string]float64 `json:"params"`
	Days        int                `json:"days"`
	InitialCash float64            `json:"initial_cash"`
	WarmupBars  int                `json:"warmup_bars"`
	Risk        *legacyRisk        `json:"risk"`
	Review      bool               `json:"review"`
}

type legacyRisk struct {
	MaxPositionPct     *float64 `json:"max_position_pct"`
	MaxRiskPerTradePct *float64 `json:"max_risk_per_trade_pct"`
	StopLossPct        *float64 `json:"stop_loss_pct"`
	TakeProfitPct      *float64 `json:"take_profit_pct"`
	MaxDrawdownPct     *float64 `json:"max_drawdown_pct"`
	MaxDailyLossPct    *float64 `json:"max_daily_loss_pct"`
	AllowShort         *bool    `json:"allow_short"`
	CommissionBps      *float64 `json:"commission_bps"`
	SlippageBps        *float64 `json:"slippage_bps"`
}

type legacySettings struct {
	legacyBacktest
	IntervalSeconds int    `json:"interval_seconds"`
	Execute         bool   `json:"execute"`
	Confirm         string `json:"confirm"`
	Leverage        int    `json:"leverage"`
	StatePath       string `json:"state_path"`
	Veto            bool   `json:"veto"`
}

// TestSettingsWireFormatIsStable guards the session's settings echo against a
// field reordering in Settings that would break the console: the flattened
// Settings JSON must equal the legacy (inline-embedded) StartSessionRequest
// JSON, byte for byte, for a fully-populated value.
func TestSettingsWireFormatIsStable(t *testing.T) {
	// A value with every field set, so a reordering or key change is caught.
	fp := func(f float64) *float64 { return &f }
	tb := func(b bool) *bool { return &b }
	// Same map content on both sides; Go's encoder sorts map keys, so both
	// serializations agree regardless of insertion order.
	params := map[string]float64{"slow": 30, "fast": 10}

	modern := Settings{
		Symbol: "BTCUSDT", Strategy: "ma_cross",
		Params: params, Days: 300, InitialCash: 10000, WarmupBars: 30,
		Risk: &SessionRiskView{
			MaxPositionPct: fp(0.95), MaxRiskPerTradePct: fp(0.02),
			StopLossPct: fp(0.05), TakeProfitPct: fp(0.10),
			MaxDrawdownPct: fp(0.15), MaxDailyLossPct: fp(0.03),
			AllowShort: tb(true), CommissionBps: fp(5), SlippageBps: fp(2),
		},
		Review:          true,
		IntervalSeconds: 60, Execute: true, Confirm: "确认实盘",
		Leverage: 3, StatePath: "trade-state.json", Veto: true,
	}

	legacy := legacySettings{
		legacyBacktest: legacyBacktest{
			Symbol: "BTCUSDT", Strategy: "ma_cross",
			Params: params, Days: 300, InitialCash: 10000, WarmupBars: 30,
			Risk: &legacyRisk{
				MaxPositionPct: fp(0.95), MaxRiskPerTradePct: fp(0.02),
				StopLossPct: fp(0.05), TakeProfitPct: fp(0.10),
				MaxDrawdownPct: fp(0.15), MaxDailyLossPct: fp(0.03),
				AllowShort: tb(true), CommissionBps: fp(5), SlippageBps: fp(2),
			},
			Review: true,
		},
		IntervalSeconds: 60, Execute: true, Confirm: "确认实盘",
		Leverage: 3, StatePath: "trade-state.json", Veto: true,
	}

	m, err := json.Marshal(modern)
	if err != nil {
		t.Fatalf("marshal Settings: %v", err)
	}
	l, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if string(m) != string(l) {
		t.Fatalf("Settings wire format drifted from the legacy StartSessionRequest shape:\nmodern: %s\nlegacy: %s", m, l)
	}
}

// A session with no risk overrides must serialize risk as null, matching the
// legacy encoder's treatment of a nil pointer, not as an empty object.
func TestSettingsNilRiskSerializesAsNull(t *testing.T) {
	s := Settings{Symbol: "BTCUSDT", Strategy: "ma_cross"}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacySettings{legacyBacktest: legacyBacktest{Symbol: "BTCUSDT", Strategy: "ma_cross"}}
	lb, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(lb) {
		t.Fatalf("nil-risk wire format drifted:\nmodern: %s\nlegacy: %s", b, lb)
	}
}
