package tune

import (
	"fmt"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

// WindowResult uses half-open bar offsets in the original series.
type WindowResult struct {
	Index    int     `json:"index"`
	Start    int     `json:"start"`
	End      int     `json:"end"`
	Holdout  bool    `json:"holdout"`
	Baseline float64 `json:"baseline"`
	Winner   float64 `json:"winner"`
	Valid    bool    `json:"valid"`
	Passed   bool    `json:"passed"`
}

type ValidationReport struct {
	Accepted bool           `json:"accepted"`
	Windows  []WindowResult `json:"windows"`
}

// Reserve the first chunk exclusively for proposals, K chunks for validation,
// and the last for holdout. No future validation data enters the LLM prompt.
func splitCV(series model.Series, folds, warmup int) (model.Series, []WindowResult, error) {
	if folds == 0 {
		return series, nil, nil
	}
	if folds < 0 || folds > 32 {
		return model.Series{}, nil, fmt.Errorf("cv folds must be between 0 and 32")
	}
	chunks := folds + 2
	size := series.Len() / chunks
	if warmup < 0 {
		warmup = 0
	}
	if size < 2 || size <= warmup {
		return model.Series{}, nil, fmt.Errorf("insufficient bars for %d cv folds plus training/holdout and %d warmup bars", folds, warmup)
	}
	training := series
	training.Bars = series.Bars[:size]
	windows := make([]WindowResult, 0, folds+1)
	for i := 1; i < chunks; i++ {
		end := (i + 1) * size
		if i == chunks-1 {
			end = series.Len()
		}
		windows = append(windows, WindowResult{Index: i, Start: i * size, End: end, Holdout: i == chunks-1})
	}
	return training, windows, nil
}

func validateWinner(cfg config.Config, series model.Series, windows []WindowResult, baseline, winner map[string]float64, objective string) *ValidationReport {
	report := &ValidationReport{Windows: append([]WindowResult(nil), windows...)}
	evaluate := func(params map[string]float64, window model.Series) (float64, bool) {
		clone := cfg
		clone.Strategy.Params = copyParams(params)
		strat, err := strategy.New(clone.Strategy.Name, clone)
		if err != nil {
			return 0, false
		}
		result, err := engine.New(clone, strat).RunBacktest(window)
		if err != nil {
			return 0, false
		}
		value, ok := ObjectiveValue(result.Metrics, objective)
		return value, ok && model.Valid(value)
	}
	for i := range report.Windows {
		w := &report.Windows[i]
		window := series
		window.Bars = series.Bars[w.Start:w.End]
		base, baseOK := evaluate(baseline, window)
		candidate, candidateOK := evaluate(winner, window)
		w.Valid = baseOK && candidateOK
		// Undefined metrics fail closed, never count as zero-score wins.
		if baseOK {
			w.Baseline = base
		}
		if candidateOK {
			w.Winner = candidate
		}
		w.Passed = w.Valid && candidate >= base
	}
	report.Accepted = acceptsValidation(report.Windows)
	return report
}

func acceptsValidation(windows []WindowResult) bool {
	folds, passed, holdouts := 0, 0, 0
	for _, w := range windows {
		if w.Holdout {
			holdouts++
			if !w.Valid || !w.Passed {
				return false
			}
		} else {
			folds++
			if w.Valid && w.Passed {
				passed++
			}
		}
	}
	return holdouts == 1 && folds > 0 && passed > folds/2
}
