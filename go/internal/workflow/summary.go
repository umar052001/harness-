package workflow

import "path/filepath"

type ModelTotal struct {
	Calls             int         `json:"calls"`
	Tasks             []string    `json:"tasks"`
	Input             *int64      `json:"input"`
	Output            *int64      `json:"output"`
	CacheRead         *int64      `json:"cacheRead"`
	UnknownUsageCalls int         `json:"unknownUsageCalls"`
	EstimatedUSDRange *[2]float64 `json:"knownCostEstimateUSDRange"`
	UnknownCostCalls  int         `json:"unknownCostCalls"`
}

func addCount(dest **int64, value *int64) {
	if value == nil {
		return
	}
	if *dest == nil {
		*dest = new(int64)
	}
	**dest += *value
}
func (b *Build) summary(passed bool) error {
	totals := map[string]*ModelTotal{}
	var aggregate ModelTotal
	for _, row := range b.records {
		if row.Accounting == nil {
			continue
		}
		t := totals[row.Model]
		if t == nil {
			t = &ModelTotal{}
			totals[row.Model] = t
		}
		t.Tasks = append(t.Tasks, row.Phase)
		for _, target := range []*ModelTotal{t, &aggregate} {
			a := row.Accounting
			target.Calls++
			if a.Tokens == nil {
				target.UnknownUsageCalls++
			} else {
				addCount(&target.Input, a.Tokens.Input)
				addCount(&target.Output, a.Tokens.Output)
				addCount(&target.CacheRead, a.Tokens.CacheRead)
			}
			if a.EstimatedUSD != nil {
				if target.EstimatedUSDRange == nil {
					target.EstimatedUSDRange = new([2]float64)
				}
				target.EstimatedUSDRange[0] += *a.EstimatedUSD
				target.EstimatedUSDRange[1] += *a.EstimatedUSD
			} else if len(a.EstimatedUSDRange) == 2 {
				if target.EstimatedUSDRange == nil {
					target.EstimatedUSDRange = new([2]float64)
				}
				target.EstimatedUSDRange[0] += a.EstimatedUSDRange[0]
				target.EstimatedUSDRange[1] += a.EstimatedUSDRange[1]
			} else {
				target.UnknownCostCalls++
			}
		}
	}
	outcome := "failed"
	if passed {
		outcome = "passed"
	}
	return writeJSON(filepath.Join(b.Dir, "summary.json"), map[string]any{"outcome": outcome, "models": totals, "totals": aggregate, "note": "Provider-reported counts, not estimated from text. ACP cache/reasoning categories may overlap. Costs are dated-rate equivalents, not invoices. Unknown/subscription costs are excluded, never assumed zero."})
}
