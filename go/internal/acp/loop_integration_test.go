package acp

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRepeatedFailuresStopPrompt(t *testing.T) {
	c, _ := connectFake(t, "failure-loop", nil)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := c.Prompt(ctx, "work")
	if err == nil || !strings.Contains(err.Error(), "3 repeated tool failures") {
		t.Fatalf("expected loop guard, got %v", err)
	}
}
