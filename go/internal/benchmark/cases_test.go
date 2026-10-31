package benchmark

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSummaryVerification(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"correct", `{"orders":5,"totalCents":3847,"byRegion":{"north":2650,"south":1197,"west":0}}`, true},
		{"wrong total", `{"orders":5,"totalCents":3846,"byRegion":{"north":2650,"south":1197,"west":0}}`, false},
		{"missing region", `{"orders":5,"totalCents":3847,"byRegion":{"north":2650,"south":1197}}`, false},
		{"trailing", `{"orders":5,"totalCents":3847,"byRegion":{"north":2650,"south":1197,"west":0}} garbage`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := VerifySummary(root); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
