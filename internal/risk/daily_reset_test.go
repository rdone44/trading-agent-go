package risk

import (
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
)

func TestDailyLossResetAcrossMidnightAndRestart(t *testing.T) {
	// The configured timestamp's calendar day, not elapsed 24 hours, controls reset.
	zone := time.FixedZone("UTC+8", 8*60*60)
	before := time.Date(2024, 1, 2, 23, 59, 0, 0, zone)
	settings := config.Risk{MaxDailyLossPct: ptr(0.05), MaxOpenPositions: 1}
	m := New(settings)
	m.Update(before, 1000, 1000)
	m.Update(before.Add(30*time.Second), 940, 1000)
	if m.CanEnter(0, false).Allowed || len(m.Events) != 1 {
		t.Fatalf("loss must pause entries once: state=%+v events=%+v", m.Snapshot(), m.Events)
	}
	m.Update(before.Add(40*time.Second), 930, 1000)
	if len(m.Events) != 1 {
		t.Fatal("repeated breach duplicated daily-loss event")
	}

	restored := New(settings)
	restored.Restore(m.Snapshot())
	restored.Update(before.Add(50*time.Second), 940, 1000)
	if restored.CanEnter(0, false).Allowed {
		t.Fatal("restart cleared the same-day pause")
	}
	after := before.Add(time.Minute)
	restored.Update(after, 940, 1000)
	if !restored.CanEnter(0, false).Allowed || restored.Snapshot().DayStartEquity != 940 {
		t.Fatalf("midnight must reset pause and baseline: %+v", restored.Snapshot())
	}
	// 890 is below the new day's 5% limit; 900 is not, even though both
	// remain more than 5% below the previous day's starting equity.
	restored.Update(after.Add(time.Minute), 900, 1000)
	if !restored.CanEnter(0, false).Allowed {
		t.Fatal("daily loss was measured against yesterday's equity")
	}
	restored.Update(after.Add(2*time.Minute), 890, 1000)
	if restored.CanEnter(0, false).Allowed || len(restored.Events) != 1 {
		t.Fatalf("new day's loss must pause entries: %+v", restored.Snapshot())
	}
}

func TestDailyResetDoesNotClearPersistentHalt(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "drawdown", true: "uncertain_order"}[uncertain], func(t *testing.T) {
			m := New(config.Risk{MaxDailyLossPct: ptr(0.05), MaxDrawdownPct: ptr(0.1)})
			ts := time.Date(2024, 1, 2, 23, 59, 0, 0, time.UTC)
			m.Update(ts, 1000, 1000)
			m.Update(ts, 850, 1000)
			if uncertain {
				m.RequireReconciliation("pending order")
			}
			state := m.Snapshot()
			restored := New(m.Settings)
			restored.Restore(state)
			restored.Update(ts.Add(time.Minute), 1000, 1000)
			got := restored.Snapshot()
			if got.EntriesBlockedDay || !got.Halted || got.HaltReason != state.HaltReason || got.OrderUncertain != uncertain || restored.CanEnter(0, false).Allowed {
				t.Fatalf("midnight cleared persistent safety state: %+v", got)
			}
		})
	}
}
