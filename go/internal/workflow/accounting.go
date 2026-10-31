package workflow

import "github.com/umar052001/ai-os/go/internal/usage"

type Tokens struct {
	Input     *int64 `json:"input"`
	Output    *int64 `json:"output"`
	CacheRead *int64 `json:"cacheRead"`
	Reasoning *int64 `json:"reasoning"`
}
type Accounting struct {
	Tokens            *Tokens   `json:"tokens"`
	Measurement       string    `json:"measurement"`
	Source            string    `json:"source"`
	EstimatedUSD      *float64  `json:"estimatedUSD"`
	EstimatedUSDRange []float64 `json:"estimatedUSDRange,omitempty"`
	Billing           string    `json:"billing"`
}

func FromACP(report usage.Report) *Tokens {
	if !report.Tokens.Valid() {
		return nil
	}
	t := report.Tokens
	return &Tokens{Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, Reasoning: t.Thought}
}
func Account(m Model, t *Tokens, source string) Accounting {
	a := Accounting{Tokens: t, Measurement: "unknown", Source: source, Billing: m.Price.Billing}
	if t == nil || t.Input == nil || t.Output == nil || *t.Input < 0 || *t.Output < 0 {
		return a
	}
	a.Measurement = "provider-reported"
	if m.Price.Input == nil || m.Price.Output == nil {
		return a
	}
	input, output := float64(*t.Input), float64(*t.Output)
	base := input**m.Price.Input + output**m.Price.Output
	if source == "ollama-native" && t.CacheRead != nil && *t.CacheRead >= 0 && *t.CacheRead <= *t.Input && m.Price.CacheRead != nil {
		base = (input-float64(*t.CacheRead))**m.Price.Input + float64(*t.CacheRead)**m.Price.CacheRead + output**m.Price.Output
		value := base / 1e6
		a.EstimatedUSD = &value
	} else {
		extra := 0.0
		if t.CacheRead != nil && *t.CacheRead > 0 {
			extra += float64(*t.CacheRead) * *m.Price.Input
		}
		if t.Reasoning != nil && *t.Reasoning > 0 {
			extra += float64(*t.Reasoning) * *m.Price.Output
		}
		a.EstimatedUSDRange = []float64{base / 1e6, (base + extra) / 1e6}
	}
	return a
}
