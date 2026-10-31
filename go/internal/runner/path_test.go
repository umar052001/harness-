package runner

import "testing"

func TestRuntimePathDropsOnlyMiseDispatchers(t *testing.T) {
	got := withoutMiseShims("/tools/mise/installs/go/bin:/tools/mise/shims:/usr/bin:/other/shims")
	want := "/tools/mise/installs/go/bin:/usr/bin:/other/shims"
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}
