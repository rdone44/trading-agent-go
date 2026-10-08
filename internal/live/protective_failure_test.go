package live_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/state"
)

type failedPlacement struct {
	cancels   int
	cancelErr error
}

func (*failedPlacement) Open(broker.Side, float64, float64) error {
	return errors.New("placement failed")
}
func (p *failedPlacement) Cancel() error    { p.cancels++; return p.cancelErr }
func (*failedPlacement) Has() (bool, error) { return false, nil }

func TestCycleAfterProtectivePlacementFailure(t *testing.T) {
	for _, cancelFails := range []bool{false, true} {
		name := "confirmed_cancel"
		if cancelFails {
			name = "uncertain_cancel"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			runner := offlineRunner(t, path)
			protection := &failedPlacement{}
			runner.Agent().Protective = protection
			if err := runner.Init(); err != nil {
				t.Fatal(err)
			}
			res, err := runner.Cycle(time.Now().UTC())
			if err != nil || !res.Entered {
				t.Fatalf("entry: %+v err=%v", res, err)
			}
			saved, exists, err := state.Load(path)
			if err != nil || !exists {
				t.Fatalf("persisted state: exists=%v err=%v", exists, err)
			}
			_, _, _, _, _, riskState := saved.ToEngine()
			if !riskState.Halted || riskState.OrderUncertain {
				t.Fatalf("persisted risk: %+v", riskState)
			}

			// Resume the persisted halt. Neither history nor strategy may run before
			// the protective exit, even when the price is still at the entry level.
			resumed := offlineRunner(t, path)
			resumed.Agent().Protective = protection
			if err := resumed.Init(); err != nil {
				t.Fatal(err)
			}
			price := resumed.Agent().OpenTrade().EntryPrice
			priceCalls := 0
			resumed.PriceLoader = func(string) (float64, time.Time, error) { priceCalls++; return price, time.Now(), nil }
			resumed.SeriesLoader = func(string, int, time.Time) (model.Series, error) {
				t.Fatal("halted cycle fetched history")
				return model.Series{}, nil
			}
			if cancelFails {
				protection.cancelErr = errors.New("cancel timeout")
			}
			res, err = resumed.Cycle(time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if protection.cancels != 1 {
				t.Fatalf("cancel calls: %d", protection.cancels)
			}
			if cancelFails {
				if res.Exited || !resumed.Agent().Risk.OrderUncertain || !resumed.Agent().Book.Position("TEST").IsOpen() {
					t.Fatalf("unsafe cancel failure: %+v", res)
				}
				if _, err := resumed.Cycle(time.Now().UTC()); err == nil {
					t.Fatal("unknown cancellation was retried")
				}
				if priceCalls != 1 || protection.cancels != 1 || len(resumed.Agent().Broker.Fills()) != 0 {
					t.Fatal("uncertain cycle must not retry trading")
				}
			} else {
				if !res.Exited || resumed.Agent().Book.Position("TEST").IsOpen() || len(resumed.Agent().Broker.Fills()) != 1 {
					t.Fatalf("halt failed to flatten: %+v", res)
				}
				if _, err := resumed.Cycle(time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				if len(resumed.Agent().Broker.Fills()) != 1 {
					t.Fatal("halt reopened or double-closed")
				}
			}
		})
	}
}
