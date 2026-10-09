package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The cycle log used to live only in memory and was cleared by every Start,
// so a restart erased the entire run history — including the rows that would
// explain why the previous run stopped. The journal is the on-disk half: one
// JSON object per cycle, appended next to the ledger.
//
// It is deliberately separate from the ledger file. The ledger is state the
// engine must be able to trust and rewrite atomically; the journal is an
// append-only record that is only ever read back as a display tail, so a
// half-written last line (a crash mid-append) can simply be skipped instead of
// failing the session.
const (
	// journalSuffix is appended to the ledger name, so every ledger keeps its
	// own history and accounts mode inherits the per-account directory.
	journalSuffix = ".log.jsonl"
)

// journalPathFor derives the journal path from the ledger path. An empty
// ledger path means "do not persist", so it yields an empty journal path too.
func journalPathFor(statePath string) string {
	if statePath == "" {
		return ""
	}
	return strings.TrimSuffix(statePath, filepath.Ext(statePath)) + journalSuffix
}

// latestJournal returns the most recently written journal in a sessions
// directory, newest first by modification time. It answers the question the
// console faces on a cold start — "what did the last run do?" — without the
// operator having to restart a session just to read its history.
func latestJournal(dir string) string {
	if dir == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	newest, newestAt := "", time.Time{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), journalSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestAt) {
			newest, newestAt = filepath.Join(dir, entry.Name()), info.ModTime()
		}
	}
	return newest
}

// LatestLog returns the newest persisted run log in a sessions directory along
// with its path. It lets a freshly opened console show the previous run's
// history before any session has been started, instead of an empty table that
// looks like the log was lost.
func LatestLog(dir string) (string, []CycleRecord) {
	path := latestJournal(dir)
	if path == "" {
		return "", nil
	}
	return path, loadJournal(path, cycleLogLimit)
}

// loadJournal reads the tail of a journal, newest last. A missing file is not
// an error: it just means this ledger has no history yet. Unparseable lines
// are skipped rather than fatal — the only line that can be corrupt is the one
// being written when the process died, and refusing to start because of it
// would defeat the purpose of keeping a log.
func loadJournal(path string, limit int) []CycleRecord {
	if path == "" || limit <= 0 {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	// Keep a rolling window of the last limit records instead of holding the
	// whole file: a long-running journal is read on every start.
	ring := make([]CycleRecord, 0, limit)
	scanner := bufio.NewScanner(file)
	// A record is a few hundred bytes; the default 64 KiB token limit is
	// plenty, but a model answer quoted into a reason can be long, so allow
	// some headroom before giving up on the line.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record CycleRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		if len(ring) == limit {
			copy(ring, ring[1:])
			ring[len(ring)-1] = record
			continue
		}
		ring = append(ring, record)
	}
	return ring
}

// appendJournal adds one record. Errors are returned so the caller can decide
// what to do, but a journal failure must never stop trading: the log is an
// observer of the session, not a participant in it.
func appendJournal(path string, record CycleRecord) error {
	if path == "" {
		return nil
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(encoded)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// rewriteJournal replaces the journal with exactly the given tail. It is used
// once the append path has grown past journalMaxBytes, so the file cannot grow
// without bound on a session left running for months.
func rewriteJournal(path string, records []CycleRecord) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			continue
		}
		if _, err := writer.Write(append(encoded, '\n')); err != nil {
			_ = file.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
