package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDisableTokenLayer(t *testing.T) {
	cases := []struct {
		accounts, explicit, want bool
	}{
		{false, false, false}, // no accounts: token rules unchanged
		{true, false, true},   // accounts on, no explicit token: token layer off
		{true, true, false},   // accounts on AND explicit token: double auth
		{false, true, false},  // no accounts, explicit token: token layer on
	}
	for _, c := range cases {
		if got := disableTokenLayer(c.accounts, c.explicit); got != c.want {
			t.Errorf("disableTokenLayer(accounts=%v, explicit=%v) = %v, want %v", c.accounts, c.explicit, got, c.want)
		}
	}
}

func TestResolveTokenPrecedence(t *testing.T) {
	// Explicit token wins over everything.
	if got, err := resolveToken("explicit", "", false); err != nil || got != "explicit" {
		t.Errorf("explicit token = %q, %v", got, err)
	}

	// Anonymous is an explicit opt-in.
	if got, err := resolveToken("", "", true); err != nil || got != "" {
		t.Errorf("anonymous = %q, %v; want \"\", nil", got, err)
	}

	// Without an opt-in and without a token file, startup must fail.
	if _, err := resolveToken("", "", false); err == nil {
		t.Error("expected an error with no token and no anonymous opt-in")
	}

	// A token file is read when present and generated (persisted) when not.
	dir := t.TempDir()
	file := filepath.Join(dir, "token.txt")
	if err := os.WriteFile(file, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveToken("", file, false); err != nil || got != "file-token" {
		t.Errorf("token file = %q, %v; want file-token", got, err)
	}
	generated := filepath.Join(dir, "new-token.txt")
	generatedValue, err := resolveToken("", generated, false)
	if err != nil || len(generatedValue) != 32 {
		t.Errorf("generated token = %q, %v; want 32 hex chars", generatedValue, err)
		return
	}
	if raw, err := os.ReadFile(generated); err != nil || string(raw) != generatedValue+"\n" {
		t.Errorf("generated token not persisted verbatim: %q", raw)
	}
}
