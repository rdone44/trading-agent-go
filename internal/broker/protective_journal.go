package broker

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProtectionState is the exchange-side truth about the protective legs for a
// position. It is intentionally more precise than a boolean: one surviving
// leg is not a covered position.
type ProtectionState string

const (
	ProtectionNotRequired ProtectionState = "not_required"
	ProtectionVerified    ProtectionState = "verified"
	ProtectionMissing     ProtectionState = "missing"
	ProtectionPartial     ProtectionState = "partial"
	ProtectionConflict    ProtectionState = "conflict"
	ProtectionUnknown     ProtectionState = "unknown"
)

// ProtectionLeg is the small, non-sensitive identity of one exchange-side
// conditional order. It is safe to expose in the dashboard and audit log.
type ProtectionLeg struct {
	Present      bool    `json:"present"`
	AlgoID       int64   `json:"algo_id,omitempty"`
	ClientAlgoID string  `json:"client_algo_id,omitempty"`
	OrderType    string  `json:"order_type,omitempty"`
	TriggerPrice float64 `json:"trigger_price,omitempty"`
	Status       string  `json:"status,omitempty"`
}

// ProtectionSnapshot is the result of one exchange inspection. A successful
// response with an incomplete or conflicting set of legs is not an error: the
// state explains why the caller must stop or repair before trading.
type ProtectionSnapshot struct {
	State     ProtectionState `json:"state"`
	Stop      ProtectionLeg   `json:"stop"`
	Target    ProtectionLeg   `json:"target"`
	CheckedAt time.Time       `json:"checked_at"`
	Reason    string          `json:"reason,omitempty"`
}

type protectiveIntent struct {
	Symbol    string                `json:"symbol"`
	Side      Side                  `json:"side"`
	Legs      []protectiveIntentLeg `json:"legs"`
	CreatedAt time.Time             `json:"created_at"`
}

type protectiveIntentLeg struct {
	OrderType    string  `json:"order_type"`
	ClientAlgoID string  `json:"client_algo_id"`
	TriggerPrice float64 `json:"trigger_price"`
}

// ValidateProtectiveIntent checks a pending file without exposing its secret
// contents. Runner.Init uses it before reconciliation so a corrupt intent
// cannot silently become a fresh pair of orders.
func ValidateProtectiveIntent(path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var intent protectiveIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return fmt.Errorf("解析保护单意图失败: %w", err)
	}
	if strings.TrimSpace(intent.Symbol) == "" || (intent.Side != Buy && intent.Side != Sell) {
		return fmt.Errorf("保护单意图缺少有效 symbol 或方向")
	}
	if len(intent.Legs) == 0 {
		return fmt.Errorf("保护单意图没有保护腿")
	}
	seen := map[string]bool{}
	for _, leg := range intent.Legs {
		if leg.OrderType != "STOP_MARKET" && leg.OrderType != "TAKE_PROFIT_MARKET" {
			return fmt.Errorf("保护单意图包含未知类型 %q", leg.OrderType)
		}
		if seen[leg.OrderType] || strings.TrimSpace(leg.ClientAlgoID) == "" ||
			!strings.HasPrefix(leg.ClientAlgoID, "tap-") || !validProtectionLevel(leg.TriggerPrice) {
			return fmt.Errorf("保护单意图的 %s 腿无效或重复", leg.OrderType)
		}
		seen[leg.OrderType] = true
	}
	return nil
}

func validProtectionLevel(level float64) bool {
	return level > 0 && !math.IsNaN(level) && !math.IsInf(level, 0)
}

func loadProtectiveIntent(path string) (protectiveIntent, bool, error) {
	if path == "" {
		return protectiveIntent{}, false, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return protectiveIntent{}, false, nil
	}
	if err != nil {
		return protectiveIntent{}, false, err
	}
	var intent protectiveIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return protectiveIntent{}, false, fmt.Errorf("解析保护单意图失败: %w", err)
	}
	if err := ValidateProtectiveIntent(path); err != nil {
		return protectiveIntent{}, false, err
	}
	return intent, true, nil
}

func createProtectiveIntent(path, symbol string, side Side, legs []protectiveIntentLeg) (protectiveIntent, error) {
	intent := protectiveIntent{
		Symbol: strings.ToUpper(symbol), Side: side, Legs: legs, CreatedAt: time.Now().UTC(),
	}
	if path == "" {
		return intent, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return protectiveIntent{}, err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return protectiveIntent{}, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return protectiveIntent{}, err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return protectiveIntent{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return protectiveIntent{}, err
	}
	if err := file.Close(); err != nil {
		return protectiveIntent{}, err
	}
	return intent, nil
}

// ClearProtectiveIntent is called by the live runner only after the ledger
// containing the position has been durably saved and the exchange legs have
// been inspected successfully.
func ClearProtectiveIntent(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
