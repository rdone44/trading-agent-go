package localcreds_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/localcreds"
)

// A missing file is the first-run state: the store must work and stay empty
// instead of failing the desktop start.
func TestLoadMissingFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store, err := localcreds.Load(path)
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if st := store.Status(); st.BinAPIKey || st.LLMAPIKey || st.AllowLive {
		t.Errorf("status = %+v, want everything empty", st)
	}
}

// Secrets must never be readable through Status, and the file must be 0600 on
// the Unix editions (Windows maps the mode to the read-only attribute; its
// confidentiality comes from the per-user directory ACL).
func TestSetPersistsWithPrivateModeAndNoEcho(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store, err := localcreds.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(localcreds.Patch{
		BinanceAPIKey: "bin-key", BinanceSecretKey: "bin-secret",
		LLMBaseURL: "https://llm.example/v1", LLMModel: "gpt-5.4", LLMAPIKey: "sk-secret",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "bin-key") {
		t.Error("saved Binance key is not in the file")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode = %v, want 0600", got)
		}
	}

	st := store.Status()
	if !st.BinAPIKey || !st.BinSecretKey || !st.LLMAPIKey || st.LLMBaseURL != "https://llm.example/v1" || st.LLMModel != "gpt-5.4" {
		t.Errorf("status = %+v", st)
	}
	// Status is the only reader the UI has; it must be presence flags only.
	for _, secret := range []string{"bin-key", "bin-secret", "sk-secret"} {
		if strings.Contains(strings.Join([]string{
			st.LLMBaseURL, st.LLMModel, st.LLMPrompt,
		}, " "), secret) {
			t.Errorf("status leaked %q", secret)
		}
	}
}

// A partial save only updates what was typed; an empty field never wipes a
// stored secret.
func TestSetKeepsUntouchedFields(t *testing.T) {
	store, err := localcreds.Load(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(localcreds.Patch{BinanceAPIKey: "key", BinanceSecretKey: "secret"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(localcreds.Patch{LLMModel: "gpt-5.4"}); err != nil {
		t.Fatal(err)
	}
	st := store.Status()
	if !st.BinAPIKey || !st.BinSecretKey {
		t.Errorf("partial save dropped the Binance keys: %+v", st)
	}
	if st.LLMModel != "gpt-5.4" {
		t.Errorf("model = %q, want gpt-5.4", st.LLMModel)
	}
}

// Apply overlays stored values onto the config and leaves everything else
// alone, which is what keeps the environment fallback working.
func TestApplyOverlaysWithoutClearingFallback(t *testing.T) {
	store, err := localcreds.Load(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(localcreds.Patch{
		BinanceAPIKey: "stored-key", LLMModel: "gpt-5.4",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Live.ExchangeSecretKey = "from-yaml-or-env"
	store.Apply(&cfg)
	if cfg.Live.ExchangeAPIKey != "stored-key" {
		t.Errorf("api key = %q, want the stored one", cfg.Live.ExchangeAPIKey)
	}
	if cfg.Live.ExchangeSecretKey != "from-yaml-or-env" {
		t.Errorf("secret = %q, must keep the untouched value", cfg.Live.ExchangeSecretKey)
	}
	if cfg.LLM.Model != "gpt-5.4" {
		t.Errorf("model = %q", cfg.LLM.Model)
	}
	if cfg.LLM.APIKey != "" {
		t.Errorf("llm key = %q, want empty so the env fallback applies", cfg.LLM.APIKey)
	}
}

// The desktop live gate persists separately from the credentials.
func TestAllowLiveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store, err := localcreds.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAllowLive(true); err != nil {
		t.Fatal(err)
	}
	reloaded, err := localcreds.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Status().AllowLive {
		t.Error("allow_live did not survive a reload")
	}
	if err := reloaded.SetAllowLive(false); err != nil {
		t.Fatal(err)
	}
	if reloaded.Status().AllowLive {
		t.Error("allow_live stayed on after being switched off")
	}
}

// A corrupt file must not silently erase the user's keys, but it must also not
// stop the desktop from starting: Load returns a usable empty store plus the
// error, and the next save rewrites the file.
func TestCorruptFileRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := localcreds.Load(path)
	if err == nil {
		t.Fatal("corrupt file: want an error the shell can log")
	}
	if store == nil {
		t.Fatal("store must still be usable so the panel can repair the file")
	}
	if err := store.Set(localcreds.Patch{LLMAPIKey: "sk-new"}); err != nil {
		t.Fatalf("repair save: %v", err)
	}
	again, err := localcreds.Load(path)
	if err != nil {
		t.Fatalf("reload after repair: %v", err)
	}
	if !again.Status().LLMAPIKey {
		t.Error("repaired file lost the new key")
	}
}
