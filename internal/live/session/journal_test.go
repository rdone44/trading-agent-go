package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The regression this file exists for: the cycle log was in-memory only and
// Start cleared it, so a restart erased the run history — exactly the rows an
// operator needs to understand why the previous run behaved as it did.
func TestJournalSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "paper-futures-TEST.json")

	first := NewSession()
	first.mu.Lock()
	first.journalPath = journalPathFor(statePath)
	first.appendLocked(CycleRecord{Time: "10:00:00", Action: "ai_unavailable", Reason: "密钥无效"})
	first.appendLocked(CycleRecord{Time: "10:01:00", Action: "flat", Reason: "空仓观望"})
	first.mu.Unlock()

	if _, err := os.Stat(first.journalPath); err != nil {
		t.Fatalf("the cycle log was not written to disk: %v", err)
	}

	second := NewSession()
	second.journalPath = journalPathFor(statePath)
	recovered := loadJournal(second.journalPath, cycleLogLimit)
	if len(recovered) != 2 {
		t.Fatalf("recovered %d rows, want the 2 the previous session wrote", len(recovered))
	}
	if recovered[0].Reason != "密钥无效" || recovered[1].Action != "flat" {
		t.Fatalf("recovered rows = %+v, want the originals in order", recovered)
	}
}

// A crash mid-append leaves one truncated line. That must cost at most that
// one row, never the whole history.
func TestJournalSkipsATornLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.log.jsonl")
	good := `{"time":"10:00:00","action":"flat","position":"空仓"}` + "\n"
	torn := `{"time":"10:01:00","action":"ai_unavail`
	if err := os.WriteFile(path, []byte(good+torn), 0o600); err != nil {
		t.Fatal(err)
	}

	records := loadJournal(path, cycleLogLimit)
	if len(records) != 1 || records[0].Time != "10:00:00" {
		t.Fatalf("records = %+v, want only the intact line", records)
	}
}

// An empty ledger path means "do not persist" (the CLI's throwaway runs), and
// the journal must inherit that: no file may appear next to the process.
func TestJournalWithoutLedgerPathWritesNothing(t *testing.T) {
	dir := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	s := NewSession()
	s.journalPath = journalPathFor("")
	s.mu.Lock()
	s.appendLocked(CycleRecord{Time: "10:00:00", Action: "flat"})
	s.mu.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("an ephemeral session wrote %d file(s)", len(entries))
	}
}

// A journal that cannot be written must not take the session down with it,
// but the failure has to be visible on the status instead of silent.
func TestJournalWriteFailureIsReportedNotFatal(t *testing.T) {
	s := NewSession()
	// A path under an existing *file* cannot be created, so the append fails.
	file := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.journalPath = filepath.Join(file, "state.log.jsonl")

	s.mu.Lock()
	s.appendLocked(CycleRecord{Time: "10:00:00", Action: "flat"})
	rows, journalErr := len(s.log), s.journalErr
	s.mu.Unlock()

	if rows != 1 {
		t.Fatalf("the in-memory log has %d rows, want the row to survive a journal failure", rows)
	}
	if journalErr == "" {
		t.Fatal("a failed journal write must be reported on the status")
	}
}

// Once the append path passes the cap the journal is rewritten from the
// in-memory tail, so a long-running session cannot grow its log forever.
func TestJournalCompactsToTheInMemoryTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.log.jsonl")
	records := []CycleRecord{{Time: "10:00:00", Action: "flat"}, {Time: "10:01:00", Action: "hold"}}
	if err := rewriteJournal(path, records); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	recovered := loadJournal(path, cycleLogLimit)
	if len(recovered) != 2 || recovered[1].Action != "hold" {
		t.Fatalf("recovered = %+v, want the rewritten tail", recovered)
	}
}

// The journal path is derived from the ledger so accounts mode and the
// desktop build both get per-ledger history without extra wiring.
func TestJournalPathFollowsTheLedger(t *testing.T) {
	cases := map[string]string{
		"trade-state.json":                           "trade-state.log.jsonl",
		filepath.Join("a", "paper-futures-BTC.json"): filepath.Join("a", "paper-futures-BTC.log.jsonl"),
		"": "",
	}
	for statePath, want := range cases {
		if got := journalPathFor(statePath); got != want {
			t.Errorf("journalPathFor(%q) = %q, want %q", statePath, got, want)
		}
	}
}

// A freshly opened console must show the last run's history without the
// operator having to start a session just to read it. RecoverLog is that path;
// once a session owns a ledger it must not be overwritten by it.
func TestRecoverLogShowsThePreviousRun(t *testing.T) {
	dir := t.TempDir()
	if err := appendJournal(filepath.Join(dir, "paper-futures-BTCUSDT.log.jsonl"),
		CycleRecord{Time: "09:00:00", Action: "ai_unavailable", Reason: "网关超时"}); err != nil {
		t.Fatal(err)
	}

	s := NewSession()
	s.RecoverLog(dir)
	s.mu.Lock()
	rows, path := len(s.log), s.journalPath
	s.mu.Unlock()

	if rows != 1 || path == "" {
		t.Fatalf("recovered rows=%d path=%q, want the previous run's single row", rows, path)
	}

	// A session already bound to its own ledger keeps that ledger's log.
	bound := NewSession()
	bound.journalPath = filepath.Join(dir, "paper-futures-ETHUSDT.log.jsonl")
	bound.RecoverLog(dir)
	bound.mu.Lock()
	got := bound.journalPath
	bound.mu.Unlock()
	if got == path {
		t.Fatalf("RecoverLog replaced a bound session's journal with %q", got)
	}
}

// The newest journal wins, so the console shows the most recent run rather
// than an arbitrary older one.
func TestLatestLogPicksTheNewestJournal(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.log.jsonl")
	newest := filepath.Join(dir, "new.log.jsonl")
	if err := appendJournal(old, CycleRecord{Time: "08:00:00", Action: "flat"}); err != nil {
		t.Fatal(err)
	}
	if err := appendJournal(newest, CycleRecord{Time: "09:00:00", Action: "hold"}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	path, records := LatestLog(dir)
	if path != newest {
		t.Fatalf("LatestLog picked %q, want the newest %q", path, newest)
	}
	if len(records) != 1 || records[0].Action != "hold" {
		t.Fatalf("records = %+v, want the newest journal's row", records)
	}
}
