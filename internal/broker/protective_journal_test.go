package broker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProtectiveIntentIsDurableAndValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.protective-pending.json")
	if _, err := createProtectiveIntent(path, "btcusdt", Buy, []protectiveIntentLeg{
		{OrderType: "STOP_MARKET", ClientAlgoID: "tap-stop", TriggerPrice: 90},
		{OrderType: "TAKE_PROFIT_MARKET", ClientAlgoID: "tap-target", TriggerPrice: 110},
	}); err != nil {
		t.Fatal(err)
	}
	if mode := fileMode(t, path); runtime.GOOS != "windows" && mode.Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", mode.Perm())
	}
	if err := ValidateProtectiveIntent(path); err != nil {
		t.Fatalf("ValidateProtectiveIntent: %v", err)
	}
	if err := ClearProtectiveIntent(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("pending file still exists: %v", err)
	}
}

func TestProtectiveIntentRejectsCorruptionAndMismatch(t *testing.T) {
	tests := []string{
		`{"symbol":"BTCUSDT","side":"buy","legs":[]}`,
		`{"symbol":"BTCUSDT","side":"buy","legs":[{"order_type":"STOP_MARKET","client_algo_id":"manual","trigger_price":90}]}`,
		`{"symbol":"BTCUSDT","side":"buy","legs":[{"order_type":"UNKNOWN","client_algo_id":"tap-x","trigger_price":90}]}`,
		`not-json`,
	}
	for i, raw := range tests {
		t.Run(strings.ReplaceAll(string(rune('a'+i)), " ", "_"), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pending.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := ValidateProtectiveIntent(path); err == nil {
				t.Fatalf("accepted invalid intent %q", raw)
			}
		})
	}
}

func TestProtectiveIntentDoesNotOverwriteExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	if err := os.WriteFile(path, []byte(`{"existing":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := createProtectiveIntent(path, "BTCUSDT", Buy, []protectiveIntentLeg{
		{OrderType: "STOP_MARKET", ClientAlgoID: "tap-stop", TriggerPrice: 90},
	}); err == nil {
		t.Fatal("existing pending intent was overwritten")
	}
	var raw map[string]bool
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &raw); err != nil || !raw["existing"] {
		t.Fatalf("existing intent changed: %s", data)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}
