// Package usage holds agent-reported accounting. It does not infer missing data
// or treat an agent's cost estimate as a provider invoice.
package usage

import "math"

// Report is a snapshot for one fresh agent session, not an additive delta.
type Report struct {
	Tokens  *Tokens  `json:"tokens,omitempty"`
	Context *Context `json:"context,omitempty"`
	Cost    *Cost    `json:"cost,omitempty"`
}

// Tokens follows OpenCode's ACP prompt-response extension. Categories may overlap;
// Total is reported by the agent rather than calculated by adding categories.
type Tokens struct {
	Total      *int64 `json:"totalTokens,omitempty"`
	Input      *int64 `json:"inputTokens,omitempty"`
	Output     *int64 `json:"outputTokens,omitempty"`
	Thought    *int64 `json:"thoughtTokens,omitempty"`
	CacheRead  *int64 `json:"cachedReadTokens,omitempty"`
	CacheWrite *int64 `json:"cachedWriteTokens,omitempty"`
}

type Context struct {
	Used int64 `json:"used"`
	Size int64 `json:"size"`
}

type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

func (t *Tokens) Valid() bool {
	if t == nil || t.Total == nil || t.Input == nil || t.Output == nil {
		return false
	}
	for _, n := range []*int64{t.Total, t.Input, t.Output, t.Thought, t.CacheRead, t.CacheWrite} {
		if n != nil && (*n < 0 || *n > 1<<53-1) {
			return false
		}
	}
	return true
}

func (c *Cost) Valid() bool {
	if c == nil || c.Amount < 0 || math.IsNaN(c.Amount) || math.IsInf(c.Amount, 0) || len(c.Currency) != 3 {
		return false
	}
	for _, ch := range c.Currency {
		if ch < 'A' || ch > 'Z' {
			return false
		}
	}
	return true
}
