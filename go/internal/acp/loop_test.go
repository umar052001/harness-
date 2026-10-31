package acp

import (
	"fmt"
	"testing"
)

func TestFailureGuard(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool EventTool
	}{
		{"failed tool", EventTool{Name: "bash", Kind: "execute", Status: "failed", Command: "npm test", Output: "same failure"}},
		{"browser socket", EventTool{Kind: "execute", Status: "completed", Output: "Socket path too long"}},
		{"browser gpu", EventTool{Kind: "execute", Status: "completed", Output: "GPU process isn't usable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var guard failureGuard
			for i := 0; i < 3; i++ {
				err := guard.observe(fmt.Sprint(i), tc.tool)
				if (err != nil) != (i == 2) {
					t.Fatalf("attempt %d: %v", i+1, err)
				}
				if i < 2 {
					if err := guard.observe(fmt.Sprint(i), tc.tool); err != nil {
						t.Fatal("duplicate counted:", err)
					}
					if err := guard.observe(fmt.Sprintf("read%d", i), EventTool{Kind: "read", Status: "completed", Output: tc.tool.Output}); err != nil {
						t.Fatal("log read counted:", err)
					}
				}
			}
		})
	}
}

func TestFailureGuardAllowsProgressAndPolling(t *testing.T) {
	var guard failureGuard
	failure := EventTool{Kind: "execute", Status: "failed", Command: "test", Output: "failed"}
	for i := 0; i < 10; i++ {
		if err := guard.observe(fmt.Sprintf("fail%d", i), failure); err != nil {
			t.Fatal(err)
		}
		if err := guard.observe(fmt.Sprintf("edit%d", i), EventTool{Kind: "edit", Status: "completed"}); err != nil {
			t.Fatal(err)
		}
		if err := guard.observe(fmt.Sprintf("poll%d", i), EventTool{Kind: "execute", Status: "completed", Command: "curl localhost/health", Output: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
}
