package report_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/report"
	"github.com/rdone44/trading-agent-go/internal/strategy"
	"github.com/rdone44/trading-agent-go/internal/testfx"
)

func TestReportsAreWritten(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.HistoryDays = 200
	cfg.Backtest.WarmupBars = 30
	strat, err := strategy.New(cfg.Strategy.Name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	series := testfx.Bars(cfg.Agent.Symbol, cfg.Agent.HistoryDays, 42, time.Now().UTC())
	result, err := engine.New(cfg, strat).RunBacktest(series)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	paths, err := report.Write(result, dir, "test-run")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.HTML, paths.Markdown, paths.MetricsJSON, paths.TradesCSV, paths.OrdersCSV, paths.EquityCSV} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing artifact %s: %v", path, err)
		}
	}
	if filepath.Base(paths.Directory) != "test-run" {
		t.Fatalf("unexpected run directory %s", paths.Directory)
	}

	html, err := os.ReadFile(paths.HTML)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), "资金曲线") {
		t.Fatal("html report is missing the equity curve section")
	}
	markdown, err := os.ReadFile(paths.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(markdown), "不构成任何投资建议") {
		t.Fatal("markdown report is missing the disclaimer")
	}
}
