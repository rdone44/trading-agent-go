package strategy

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/testfx"
)

// fakeAsk is an injected model that returns canned answers and counts calls,
// so every test in this package stays offline.
type fakeAsk struct {
	answer string
	err    error
	calls  int
}

func (f *fakeAsk) run(system, user string) (string, error) {
	f.calls++
	return f.answer, f.err
}

func stratOf(t *testing.T, f *fakeAsk, enabled bool) LLM {
	t.Helper()
	// Ask is stored unexported; build the struct via the named fields.
	return LLM{ask: f.run, enabled: enabled}
}

func cfgFor(params map[string]float64) config.Config {
	cfg := config.Default()
	cfg.Strategy.Name = "llm"
	for k, v := range params {
		cfg.Strategy.Params[k] = v
	}
	return cfg
}

func TestNewRegistersLLMStrategy(t *testing.T) {
	s, err := New("llm", config.Default())
	if err != nil {
		t.Fatalf("New(llm): %v", err)
	}
	if s.Name() != "llm" {
		t.Fatalf("Name = %q, want llm", s.Name())
	}
	for _, name := range Available() {
		if name == "llm" {
			return
		}
	}
	t.Fatal("Available() does not list llm")
}

func TestGenerateDegradedToFlatWithoutModel(t *testing.T) {
	// No ask injected and the default client has no key: the strategy must
	// yield a complete, all-flat signal rather than erroring, so a keyless
	// backtest still produces a coherent equity curve.
	series := testfx.Bars("BTCUSDT", 60, 1, timeNow())
	cfg := cfgFor(nil)
	s, err := New("llm", cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sig, err := s.Generate(series, cfg)
	if err != nil {
		t.Fatalf("Generate without key: %v", err)
	}
	sig = sig.Normalize(series.Len())
	for i, v := range sig.Signal {
		if v != 0 {
			t.Fatalf("degraded signal[%d] = %v, want 0 (flat)", i, v)
		}
	}
}

func TestLastDecisionDelegatesToModelOnce(t *testing.T) {
	f := &fakeAsk{answer: `{"position": 1, "stop": 90, "target": 110}`}
	series := testfx.Bars("BTCUSDT", 60, 1, timeNow())
	cfg := cfgFor(map[string]float64{"llm_window": 10, "llm_step": 1})

	s := stratOf(t, f, true)
	sig, stop, target, reason, ok := s.LastDecision(series, cfg)

	if f.calls != 1 {
		t.Fatalf("LastDecision made %d model calls, want exactly 1 (single-decision path)", f.calls)
	}
	if !ok {
		t.Fatal("LastDecision with a valid model answer must report ok=true")
	}
	if sig != 1 {
		t.Fatalf("LastDecision signal = %v, want 1", sig)
	}
	if stop != 90 || target != 110 {
		t.Fatalf("levels = (%v,%v), want (90,110)", stop, target)
	}
	if reason != "" {
		t.Fatalf("LastDecision reason = %q, want empty (no reason in the fixture)", reason)
	}
}

func TestLastDecisionDegradesFlatOnModelError(t *testing.T) {
	f := &fakeAsk{err: errors.New("model down")}
	series := testfx.Bars("BTCUSDT", 60, 1, timeNow())
	s := stratOf(t, f, true)

	sig, stop, target, reason, ok := s.LastDecision(series, cfgFor(nil))
	if ok {
		t.Fatal("a model error must report ok=false so the live loop holds the position")
	}
	if sig != 0 || !math.IsNaN(stop) || !math.IsNaN(target) {
		t.Fatalf("LastDecision on model error = (%v,%v,%v), want neutral (0,NaN,NaN)", sig, stop, target)
	}
	// The reason is what the console log shows; it must name the underlying
	// failure instead of leaving the operator with a bare "AI 不可用".
	if !strings.Contains(reason, "model down") {
		t.Fatalf("LastDecision on model error reason = %q, want the underlying error", reason)
	}
}

func TestLastDecisionDegradesFlatWhenDisabled(t *testing.T) {
	f := &fakeAsk{answer: `{"position": 1}`}
	series := testfx.Bars("BTCUSDT", 60, 1, timeNow())
	s := stratOf(t, f, false) // model present but disabled

	sig, _, _, reason, ok := s.LastDecision(series, cfgFor(nil))
	if ok {
		t.Fatal("a disabled strategy must report ok=false")
	}
	if sig != 0 {
		t.Fatalf("LastDecision disabled = %v, want 0", sig)
	}
	// The operator needs to know a key is missing, not just that something
	// failed: this string is the only clue the log gives them.
	if !strings.Contains(reason, "密钥") {
		t.Fatalf("LastDecision disabled reason = %q, want it to name the missing key", reason)
	}
	if f.calls != 0 {
		t.Fatalf("disabled strategy made %d model calls, want 0", f.calls)
	}
}

// A model that answers with prose instead of JSON is the failure mode an
// operator is least able to diagnose from the outside: the key works, the
// endpoint answers, and the strategy still refuses to trade. The reason must
// quote enough of the answer to make that visible in the run log.
func TestLastDecisionReportsUnparseableAnswer(t *testing.T) {
	f := &fakeAsk{answer: "抱歉，我无法给出交易建议。"}
	series := testfx.Bars("BTCUSDT", 60, 1, timeNow())
	s := stratOf(t, f, true)

	_, _, _, reason, ok := s.LastDecision(series, cfgFor(nil))
	if ok {
		t.Fatal("an unparseable answer must report ok=false")
	}
	if !strings.Contains(reason, "无法解析") {
		t.Fatalf("reason = %q, want it to say the answer did not parse", reason)
	}
	if !strings.Contains(reason, "无法给出交易建议") {
		t.Fatalf("reason = %q, want it to quote the model's actual answer", reason)
	}
}

func TestGenerateCapsModelCallsWithStep(t *testing.T) {
	// The cost control: with llm_step=5 on 60 bars the model is called ~12
	// times, not 60 — this is what makes backtests over long histories
	// tractable.
	const bars = 60
	f := &fakeAsk{answer: `{"position": 0}`}
	series := testfx.Bars("BTCUSDT", bars, 1, timeNow())
	cfg := cfgFor(map[string]float64{"llm_step": 5, "llm_window": 10})

	s := stratOf(t, f, true)
	if _, err := s.Generate(series, cfg); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Bars 0..29 are warm-up (slow SMA needs 30 bars); the step gate skips
	// the rest. Expect well under 60 calls, and at least one.
	if f.calls == 0 {
		t.Fatal("Generate made no model calls; the strategy should consult the model")
	}
	if f.calls >= bars {
		t.Fatalf("Generate made %d calls on %d bars; llm_step was not honoured", f.calls, bars)
	}
}

func TestParseLLMAnswerSnapsPosition(t *testing.T) {
	cases := []struct {
		in         string
		wantPos    float64
		wantStop   float64
		wantTarget float64
		wantOK     bool
	}{
		{`{"position":1,"stop":90,"target":110}`, 1, 90, 110, true},
		{`{"position":-1}`, -1, 0, 0, true},
		{`{"position":0}`, 0, 0, 0, true},
		{`{"position":0.4}`, 0, 0, 0, true},                            // rounds to flat
		{`{"position":0.5}`, 1, 0, 0, true},                            // rounds to long
		{"```json\n{\"position\":1,\"stop\":88}\n```", 1, 88, 0, true}, // fenced
		{"sure: {\"position\":-1} sure", -1, 0, 0, true},               // prose around JSON
		{"no json here", 0, 0, 0, false},                               // unparseable
		{`{"position":"NaN"}`, 0, 0, 0, false},                         // bad number
	}
	for _, c := range cases {
		pos, sp, tp, reason, _, ok := parseLLMAnswer(c.in)
		if ok != c.wantOK {
			t.Fatalf("parse(%q) ok=%v, want %v", c.in, ok, c.wantOK)
		}
		if !ok {
			continue
		}
		if reason != "" {
			t.Fatalf("parse(%q) reason = %q, want empty (no reason in the fixture)", c.in, reason)
		}
		if pos != c.wantPos {
			t.Fatalf("parse(%q) position=%v, want %v", c.in, pos, c.wantPos)
		}
		if sp != c.wantStop || tp != c.wantTarget {
			t.Fatalf("parse(%q) levels=(%v,%v), want (%v,%v)", c.in, sp, tp, c.wantStop, c.wantTarget)
		}
	}
}

func TestParseLLMAnswerTreatsNullLevelsAsZero(t *testing.T) {
	// A null stop/target must come through as 0 so the engine falls back to
	// its own protective levels rather than a NaN level that never fires.
	pos, sp, tp, _, _, ok := parseLLMAnswer(`{"position":1,"stop":null,"target":null}`)
	if !ok {
		t.Fatalf("parse null levels: ok=false")
	}
	if pos != 1 || sp != 0 || tp != 0 {
		t.Fatalf("null levels = (pos=%v,stop=%v,target=%v), want (1,0,0)", pos, sp, tp)
	}
}

func TestParseLLMAnswerKeepsOneLineReason(t *testing.T) {
	pos, _, _, reason, _, ok := parseLLMAnswer(`{"position":1,"reason":"RSI 超卖后金叉\n第二行应被丢弃"}`)
	if !ok || pos != 1 {
		t.Fatalf("parse reason fixture = (pos=%v,ok=%v), want (1,true)", pos, ok)
	}
	if reason != "RSI 超卖后金叉" {
		t.Fatalf("reason = %q, want the first line only", reason)
	}

	long := make([]rune, 100)
	for i := range long {
		long[i] = 'x'
	}
	_, _, _, longReason, _, _ := parseLLMAnswer(`{"position":0,"reason":"` + string(long) + `"}`)
	if len([]rune(longReason)) != 81 || !strings.HasSuffix(longReason, "…") {
		t.Fatalf("long reason = %d runes (suffix %q), want 80 + ellipsis", len([]rune(longReason)), longReason[len([]rune(longReason))-1:])
	}
}

func TestParseLLMAnswerReadsConfidence(t *testing.T) {
	// The model's self-assessed confidence rides in the JSON contract: it is
	// read when present, 0 when absent, and clamped to [0,1] rather than
	// trusted blindly.
	_, _, _, _, conf, ok := parseLLMAnswer(`{"position":1,"confidence":0.8}`)
	if !ok || conf != 0.8 {
		t.Fatalf("explicit confidence = (%v,%v), want (0.8,true)", conf, ok)
	}
	_, _, _, _, conf, ok = parseLLMAnswer(`{"position":1}`)
	if !ok || conf != 0 {
		t.Fatalf("absent confidence = (%v,%v), want (0,true)", conf, ok)
	}
	_, _, _, _, conf, ok = parseLLMAnswer(`{"position":1,"confidence":null}`)
	if !ok || conf != 0 {
		t.Fatalf("null confidence = (%v,%v), want (0,true)", conf, ok)
	}
	_, _, _, _, conf, ok = parseLLMAnswer(`{"position":1,"confidence":1.7}`)
	if !ok || conf != 1 {
		t.Fatalf("high confidence = (%v,%v), want clamped to (1,true)", conf, ok)
	}
	_, _, _, _, conf, ok = parseLLMAnswer(`{"position":1,"confidence":-0.4}`)
	if !ok || conf != 0 {
		t.Fatalf("negative confidence = (%v,%v), want clamped to (0,true)", conf, ok)
	}
}

func TestLastDecisionDetailedCarriesAuditFields(t *testing.T) {
	f := &fakeAsk{answer: `{"position":1,"stop":90,"target":110,"reason":"金叉确认","confidence":0.75}`}
	series := testfx.Bars("BTCUSDT", 60, 1, timeNow())
	cfg := cfgFor(map[string]float64{"llm_window": 10})

	s := stratOf(t, f, true)
	d, ok := s.LastDecisionDetailed(series, cfg)
	if !ok {
		t.Fatalf("LastDecisionDetailed ok=false: %s", d.Reason)
	}
	if d.Signal != 1 || d.Stop != 90 || d.Target != 110 {
		t.Fatalf("detailed = (%v,%v,%v), want (1,90,110)", d.Signal, d.Stop, d.Target)
	}
	if d.Reason != "金叉确认" {
		t.Fatalf("detailed reason = %q", d.Reason)
	}
	if d.Confidence != 0.75 {
		t.Fatalf("detailed confidence = %v, want 0.75 (the model's own)", d.Confidence)
	}
	if d.Model == "" {
		t.Fatal("detailed model = \"\", want the configured model name")
	}
	if d.Latency < 0 {
		t.Fatalf("detailed latency = %v, want >= 0", d.Latency)
	}
}

func TestLastDecisionDetailedClassifiesFailures(t *testing.T) {
	series := testfx.Bars("BTCUSDT", 60, 1, timeNow())

	cases := []struct {
		name    string
		ask     *fakeAsk
		enabled bool
		wantCat string
	}{
		{"disabled strategy is a no_key gap", &fakeAsk{}, false, "no_key"},
		{"network down is a network fault", &fakeAsk{err: errors.New("connection refused")}, true, "network"},
		{"a timeout is a timeout", &fakeAsk{err: errors.New("context deadline exceeded")}, true, "timeout"},
		{"an http error is an http fault", &fakeAsk{err: errors.New("http 401 unauthorized")}, true, "http"},
		{"prose is an unparseable answer", &fakeAsk{answer: "抱歉，我无法给出交易建议。"}, true, "unparseable"},
	}
	for _, c := range cases {
		s := stratOf(t, c.ask, c.enabled)
		d, ok := s.LastDecisionDetailed(series, cfgFor(nil))
		if ok {
			t.Fatalf("%s: ok=true, want false", c.name)
		}
		if !isClassifiedFailure(d.Reason) {
			t.Fatalf("%s: reason %q is not classified", c.name, d.Reason)
		}
		if got := decisionFailureCategory(d.Reason); got != c.wantCat {
			t.Fatalf("%s: category = %q, want %q (reason %q)", c.name, got, c.wantCat, d.Reason)
		}
	}
}

// testEnd is a fixed reference day so the test-fixture series is deterministic.
var testEnd = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func timeNow() time.Time { return testEnd }
