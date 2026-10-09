// Package localcreds stores the desktop edition's connection settings in a
// single per-user JSON file (credentials.json under the user's config
// directory). It exists because the desktop build is single-user by design:
// there is no administrator to configure a service environment, so the person
// double-clicking the .exe must be able to save their Binance keys and their
// model endpoint from the settings panel.
//
// Secrets live in that one file and are never echoed back: readers get
// presence flags plus the LLM base URL / model name, exactly like the
// multi-user vault in internal/auth. The file is written atomically (temp file
// + rename) with mode 0600; on Windows Go maps the mode to the read-only
// attribute and confidentiality comes from the per-user ACL of %APPDATA%.
//
// Stored values win over the YAML config file. The environment-variable
// fallback stays enabled, so an operator who already exports BINANCE_API_KEY /
// LLM_API_KEY keeps working without touching this file.
package localcreds

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// Creds is the on-disk shape. Every field is omitempty so a partially filled
// file stays readable by hand.
type Creds struct {
	BinanceAPIKey    string `json:"binance_api_key,omitempty"`
	BinanceSecretKey string `json:"binance_secret_key,omitempty"`
	LLMBaseURL       string `json:"llm_base_url,omitempty"`
	LLMModel         string `json:"llm_model,omitempty"`
	LLMPrompt        string `json:"llm_prompt,omitempty"`
	LLMAPIKey        string `json:"llm_api_key,omitempty"`
	// AllowLive is the desktop equivalent of TA_ALLOW_LIVE=1: the second
	// opt-in the live gate requires before any real order can be placed. It is
	// a setting, not a secret, and it is still combined with the confirmation
	// phrase at session start.
	AllowLive bool `json:"allow_live,omitempty"`
}

// Patch is a partial save. An empty string means "leave the stored value
// alone", so submitting one field never wipes the others.
type Patch struct {
	BinanceAPIKey    string
	BinanceSecretKey string
	LLMBaseURL       string
	LLMModel         string
	LLMPrompt        string
	LLMAPIKey        string
}

// Status reports which credentials are set, without exposing their values.
type Status struct {
	BinAPIKey    bool
	BinSecretKey bool
	LLMAPIKey    bool
	LLMBaseURL   string
	LLMModel     string
	LLMPrompt    string
	AllowLive    bool
}

// Store is the credential file plus an in-memory copy. It is safe for
// concurrent use: the webui saves from HTTP handlers while session starts read
// the values.
type Store struct {
	mu    sync.RWMutex
	path  string
	creds Creds
}

// Load reads the file at path. A missing file is not an error: it is the
// first-run state and simply yields an empty store.
//
// A file that exists but cannot be parsed returns an error, because silently
// ignoring stored keys would look like "my API key stopped working". The
// returned store is still usable and empty in that case, so the caller can
// report the problem and let the next save repair the file instead of
// refusing to start.
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s.creds); err != nil {
		s.creds = Creds{}
		return s, fmt.Errorf("凭据文件 %s 不是合法的 JSON（在设置里重新保存即可修复）: %w", path, err)
	}
	return s, nil
}

// Path is where the file lives or will be created.
func (s *Store) Path() string { return s.path }

// Snapshot returns a copy of the stored values.
func (s *Store) Snapshot() Creds {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.creds
}

// Status returns the presence flags the UI shows.
func (s *Store) Status() Status {
	c := s.Snapshot()
	return Status{
		BinAPIKey:    c.BinanceAPIKey != "",
		BinSecretKey: c.BinanceSecretKey != "",
		LLMAPIKey:    c.LLMAPIKey != "",
		LLMBaseURL:   c.LLMBaseURL,
		LLMModel:     c.LLMModel,
		LLMPrompt:    c.LLMPrompt,
		AllowLive:    c.AllowLive,
	}
}

// Set applies a partial save and persists it.
func (s *Store) Set(p Patch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	apply := func(dst *string, v string) {
		if v = strings.TrimSpace(v); v != "" {
			*dst = v
		}
	}
	apply(&s.creds.BinanceAPIKey, p.BinanceAPIKey)
	apply(&s.creds.BinanceSecretKey, p.BinanceSecretKey)
	apply(&s.creds.LLMBaseURL, p.LLMBaseURL)
	apply(&s.creds.LLMModel, p.LLMModel)
	apply(&s.creds.LLMPrompt, p.LLMPrompt)
	apply(&s.creds.LLMAPIKey, p.LLMAPIKey)
	return s.saveLocked()
}

// SetAllowLive arms or disarms the desktop live gate.
func (s *Store) SetAllowLive(allow bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds.AllowLive = allow
	return s.saveLocked()
}

// Apply overlays the stored values onto cfg. Fields that were never saved are
// left untouched, so the environment-variable fallback still applies.
func (s *Store) Apply(cfg *config.Config) {
	c := s.Snapshot()
	if c.BinanceAPIKey != "" {
		cfg.Live.ExchangeAPIKey = c.BinanceAPIKey
	}
	if c.BinanceSecretKey != "" {
		cfg.Live.ExchangeSecretKey = c.BinanceSecretKey
	}
	if c.LLMAPIKey != "" {
		cfg.LLM.APIKey = c.LLMAPIKey
	}
	if c.LLMBaseURL != "" {
		cfg.LLM.BaseURL = c.LLMBaseURL
	}
	if c.LLMModel != "" {
		cfg.LLM.Model = c.LLMModel
	}
	if c.LLMPrompt != "" {
		cfg.LLM.Prompt = c.LLMPrompt
	}
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.creds, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	// WriteFile only applies the mode when it creates the file, so a stale
	// temp file from an earlier crash could keep looser permissions.
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
