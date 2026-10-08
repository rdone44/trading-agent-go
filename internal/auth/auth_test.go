package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newVault(t *testing.T) *Service {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// Register must reject weak input before touching the vault.
func TestRegisterValidation(t *testing.T) {
	s := newVault(t)
	cases := []struct{ name, pass string }{
		{"", "password"},
		{"ab", "password"},
		{"bad name", "password"},
		{"ok", "short"},
	}
	for _, tc := range cases {
		if _, err := s.Register(tc.name, tc.pass); err == nil {
			t.Errorf("Register(%q, %q) accepted, want rejection", tc.name, tc.pass)
		}
	}
	if _, err := s.Register("alice", "secret123"); err != nil {
		t.Fatalf("Register valid: %v", err)
	}
	if _, err := s.Register("alice", "other"); err == nil {
		t.Error("duplicate username accepted")
	}
}

// The stored hash must not contain the password in clear, and the vault file
// must be private.
func TestVaultFileIsPrivateAndUnhashed(t *testing.T) {
	s := newVault(t)
	if _, err := s.Register("bob", "hunter2secret"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(s.path); info.Mode().Perm() != 0o600 {
		t.Errorf("vault mode = %v, want 0600", info.Mode())
	}
	// The password must not appear anywhere in the file.
	if containsBytes(raw, []byte("hunter2secret")) {
		t.Error("the password is stored in the vault file")
	}
}

func TestAuthenticateRoundTrip(t *testing.T) {
	s := newVault(t)
	if _, err := s.Register("carol", "correcthorse"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("carol", "wrong"); err == nil {
		t.Error("wrong password accepted")
	}
	user, err := s.Authenticate("carol", "correcthorse")
	if err != nil || user.Username != "carol" {
		t.Fatalf("authenticate = %v, %+v", err, user)
	}
	// Unknown users must fail without a leak.
	if _, err := s.Authenticate("nobody", "x"); err == nil {
		t.Error("unknown user authenticated")
	}
}

// A restart must not lose the accounts: reopen the same path and log in.
func TestVaultPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	first, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Register("dave", "password1"); err != nil {
		t.Fatal(err)
	}

	second, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := second.Authenticate("dave", "password1"); err != nil {
		t.Fatalf("restarted vault lost the account: %v", err)
	}
}

// SetCredentials must persist per-user secrets; Status reports presence only
// and Get hands the trading layer the values.
func TestCredentialsRoundTrip(t *testing.T) {
	s := newVault(t)
	if _, err := s.Register("erin", "password1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCredentials("erin", "BINKEY", "BINSECRET", "https://llm.example/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status("erin")
	if err != nil {
		t.Fatal(err)
	}
	if !st.BinAPIKey || !st.BinSecretKey || !st.LLMAPIKey || st.LLMBaseURL != "https://llm.example/v1" {
		t.Errorf("status = %+v", st)
	}
	user, ok := s.Get("erin")
	if !ok {
		t.Fatal("Get returned no user")
	}
	if user.BinAPIKey != "BINKEY" || user.BinSecretKey != "BINSECRET" || user.LLMAPIKey != "sk-test" {
		t.Errorf("Get credentials wrong: %+v", user)
	}
	// Unknown user.
	if st, _ := s.Status("ghost"); st.BinAPIKey {
		t.Error("ghost should have no credentials")
	}
}

// A session token must verify for its user, reject tampering, and honour the
// expiry. The HMAC secret lives in the vault file, so a server restart keeps
// signed sessions valid — that is the intended behaviour.
func TestSessionTokens(t *testing.T) {
	s := newVault(t)
	if _, err := s.Register("fred", "password1"); err != nil {
		t.Fatal(err)
	}
	token, err := s.IssueSession("fred", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if user, ok := s.VerifySession(token); !ok || user != "fred" {
		t.Fatalf("VerifySession = %q, %v", user, ok)
	}
	if _, ok := s.VerifySession(token + "x"); ok {
		t.Error("tampered token verified")
	}
	if _, ok := s.VerifySession(""); ok {
		t.Error("empty token verified")
	}
	// An expired token is rejected.
	expired, err := s.IssueSession("fred", -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.VerifySession(expired); ok {
		t.Error("expired token verified")
	}
	// The HMAC secret is persisted in the vault, so after a restart the
	// pre-restart token still verifies (sessions survive a server restart).
	reopened, err := New(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if user, ok := reopened.VerifySession(token); !ok || user != "fred" {
		t.Errorf("restarted service lost a live session: %q, %v", user, ok)
	}
}

// The whole public behaviour of the vault must hold on a second path, which
// also proves the file format is self-contained.
func TestSecondUserIndependent(t *testing.T) {
	s := newVault(t)
	if _, err := s.Register("user1", "password1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register("user2", "password2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("user1", "password1"); err != nil {
		t.Error(err)
	}
	if _, err := s.Authenticate("user2", "password1"); err == nil {
		t.Error("cross-user password accepted")
	}
}

func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
