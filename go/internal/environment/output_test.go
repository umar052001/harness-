package environment

import (
	"strings"
	"testing"
)

func TestDiagnosticsAreBoundedAndRedactDatabaseSecrets(t *testing.T) {
	var out commandOutput
	payload := strings.Repeat("x", 20000) + "end"
	if n, err := out.Write([]byte(payload)); err != nil || n != len(payload) {
		t.Fatalf("write: %d %v", n, err)
	}
	if text := out.String(); len(text) != 8192 || !strings.HasSuffix(text, "end") {
		t.Fatal("diagnostic tail lost or unbounded")
	}
	connection := "postgresql://app:secret@127.0.0.1:5432/app"
	if safe := redactConnection(connection+" password=secret", connection); strings.Contains(safe, "secret") || strings.Contains(safe, connection) {
		t.Fatal("database credential leaked")
	}
}
