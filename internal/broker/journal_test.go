package broker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOrderIntentSurvivesAndCannotBeOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders", "pending.json")
	if err := recordIntent(path, "first", "BTCUSDT", Buy, .1); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordIntent(path, "second", "BTCUSDT", Buy, .1); err == nil {
		t.Fatal("overwrote an unconfirmed order")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("changed pending order")
	}
	if err := ClearIntent(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("intent not cleared")
	}
}
