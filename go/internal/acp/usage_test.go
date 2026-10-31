package acp

import (
	"encoding/json"
	"testing"

	"github.com/umar052001/ai-os/go/internal/usage"
)

func TestPromptReportsTokensAndSessionCost(t *testing.T) {
	c, _ := connectFake(t, "usage", nil)
	res, err := c.Prompt(t.Context(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	u := res.Usage
	if !u.Tokens.Valid() || *u.Tokens.Total != 350 || *u.Tokens.CacheRead != 100 {
		t.Fatalf("token usage lost: %+v", u.Tokens)
	}
	if u.Context == nil || u.Context.Used != 100 || u.Cost == nil || u.Cost.Amount != 0 || u.Cost.Currency != "USD" {
		t.Fatalf("session accounting lost: %+v", u)
	}
	if u.Tokens.CacheWrite != nil {
		t.Fatal("missing category became a reported zero")
	}
}

func TestInvalidTokenTotalsRemainUnknown(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `{"totalTokens":0,"inputTokens":0}`,
		`{"totalTokens":10,"inputTokens":-1,"outputTokens":11}`,
		`{"totalTokens":9007199254740992,"inputTokens":1,"outputTokens":1}`,
	} {
		var tokens *usage.Tokens
		if err := json.Unmarshal([]byte(raw), &tokens); err != nil {
			t.Fatal(err)
		}
		if tokens.Valid() {
			t.Fatalf("accepted invalid accounting: %s", raw)
		}
	}
	var zero usage.Tokens
	if err := json.Unmarshal([]byte(`{"totalTokens":0,"inputTokens":0,"outputTokens":0}`), &zero); err != nil {
		t.Fatal(err)
	}
	if !zero.Valid() {
		t.Fatal("explicit zero token usage was rejected")
	}
}

func TestUsageMissingAndMalformedAreNotFree(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"used":-1,"size":100}`, `{"cost":{"currency":"USD"}}`,
		`{"cost":{"amount":-1,"currency":"USD"}}`, `{"cost":{"amount":1,"currency":"invalid"}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			c := &Client{coll: &collector{}}
			c.onUsageUpdate(json.RawMessage(raw))
			u := c.coll.usageSnapshot(nil)
			if u.Cost != nil || u.Tokens != nil || u.Context != nil {
				t.Fatalf("invented accounting: %+v", u)
			}
		})
	}
}

func TestContextSnapshotsCanShrinkWithoutLosingCost(t *testing.T) {
	c := &Client{coll: &collector{}}
	c.onUsageUpdate(json.RawMessage(`{"used":900,"size":1000,"cost":{"amount":0.12,"currency":"EUR"}}`))
	c.onUsageUpdate(json.RawMessage(`{"used":200,"size":1000}`))
	u := c.coll.usageSnapshot(nil)
	if u.Context.Used != 200 || u.Cost.Amount != 0.12 {
		t.Fatalf("snapshot incorrectly accumulated: %+v", u)
	}
}
