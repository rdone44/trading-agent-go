package session

import (
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/live"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/strategy"
	"github.com/rdone44/trading-agent-go/internal/testfx"
)

// slowStrategy blocks inside Generate/LastDecision for the given duration,
// standing in for a model call that takes a minute or more.
type slowStrategy struct {
	delay time.Duration
	seen  chan struct{}
}

func (slowStrategy) Name() string { return "llm" }

func (s slowStrategy) Describe() string { return "llm" }

func (s slowStrategy) Generate(series model.Series, cfg config.Config) (strategy.Signals, error) {
	if s.seen != nil {
		select {
		case s.seen <- struct{}{}:
		default:
		}
	}
	time.Sleep(s.delay)
	return strategy.Signals{Signal: make([]float64, len(series.Bars))}, nil
}

func (s slowStrategy) LastDecision(series model.Series, cfg config.Config) (float64, float64, float64, string, bool) {
	if s.seen != nil {
		select {
		case s.seen <- struct{}{}:
		default:
		}
	}
	time.Sleep(s.delay)
	return 0, 0, 0, "观望", true
}

// The regression this file exists for: one cycle used to hold the session
// mutex for its whole duration, including the model call. Every /api/session
// poll then blocked behind it, the browser timed out, and the console showed
// "无法读取会话状态：Failed to fetch" while the session was in fact healthy.
//
// Status must therefore answer immediately even while a cycle is in flight.
func TestStatusDoesNotBlockOnARunningCycle(t *testing.T) {
	s := NewSession()
	s.cfg = config.Default()
	s.cfg.Agent.Symbol = "BTCUSDT"
	s.running = true
	s.interval = time.Minute
	s.snapshot = SessionStatus{Equity: 100000, Cash: 100000, InitialCash: 100000}

	started := make(chan struct{}, 1)
	release := make(chan struct{})

	// Stand in for the loop: hold cycleMu exactly as a real cycle does, and
	// hold it until the test releases it.
	s.cycleMu.Lock()
	go func() {
		started <- struct{}{}
		<-release
		s.cycleMu.Unlock()
	}()
	<-started

	done := make(chan SessionStatus, 1)
	go func() { done <- s.Status() }()

	select {
	case status := <-done:
		if !status.Running {
			t.Fatal("status reported the session as stopped while it was running")
		}
		if status.Equity != 100000 {
			t.Fatalf("Equity = %v, want the published snapshot", status.Equity)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Status blocked on a running cycle; the console would show Failed to fetch")
	}
	close(release)
}

// The same guarantee through a real cycle: while the strategy is sleeping (a
// model call), Status keeps answering from the last published snapshot.
func TestStatusAnswersDuringASlowCycle(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Symbol = "BTCUSDT"
	cfg.Strategy.Name = "llm"

	s := NewSession()
	s.cfg = cfg
	s.interval = time.Minute
	s.snapshot = SessionStatus{Equity: 100000, Cash: 100000, InitialCash: 100000}
	s.running = true

	entered := make(chan struct{}, 1)
	strat := slowStrategy{delay: 1500 * time.Millisecond, seen: entered}
	runner, err := live.New(cfg, strat, false, "")
	if err != nil {
		t.Fatalf("build runner: %v", err)
	}
	// Serve the bars locally: this test is about lock behaviour, not Binance.
	runner.SeriesLoader = func(symbol string, days int, end time.Time) (model.Series, error) {
		return testfx.Bars(symbol, days, 42, end), nil
	}
	runner.PriceLoader = func(symbol string) (float64, time.Time, error) {
		series := testfx.Bars(symbol, 5, 42, time.Now().UTC())
		last := series.Bars[len(series.Bars)-1]
		return last.Close, last.Time, nil
	}
	s.runner = runner

	go s.cycleOnce()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the cycle never reached the strategy")
	}

	start := time.Now()
	status := s.Status()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Status took %s during a cycle; it must not wait for the model", elapsed)
	}
	if !status.Running {
		t.Fatal("status must still report the session as running")
	}
	if status.Equity != 100000 {
		t.Fatalf("Equity = %v, want the last published snapshot", status.Equity)
	}
}
