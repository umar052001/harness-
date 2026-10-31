package acp

import (
	"encoding/json"

	"github.com/umar052001/ai-os/go/internal/usage"
)

func (c *Client) onUsageUpdate(raw json.RawMessage) {
	var update struct {
		Used *int64 `json:"used"`
		Size *int64 `json:"size"`
		Cost *struct {
			Amount   *float64 `json:"amount"`
			Currency string   `json:"currency"`
		} `json:"cost"`
	}
	if json.Unmarshal(raw, &update) != nil {
		return
	}
	c.coll.mu.Lock()
	if update.Used != nil && update.Size != nil && *update.Used >= 0 && *update.Size > 0 && *update.Used <= 1<<53-1 && *update.Size <= 1<<53-1 {
		c.coll.usage.Context = &usage.Context{Used: *update.Used, Size: *update.Size}
	}
	if update.Cost != nil && update.Cost.Amount != nil {
		cost := &usage.Cost{Amount: *update.Cost.Amount, Currency: update.Cost.Currency}
		if cost.Valid() {
			c.coll.usage.Cost = cost
		}
	}
	snapshot := c.coll.usage
	c.coll.mu.Unlock()
	c.emit(Event{Type: EventUsage, Usage: &snapshot})
}

func (c *collector) usageSnapshot(tokens *usage.Tokens) usage.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	if tokens.Valid() {
		c.usage.Tokens = tokens
	}
	return c.usage
}
