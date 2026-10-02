package api

import "testing"

// Publishing is addressed by hostname, so the host has to be parsed the way a
// browser parses it. These are the cases that decide whether a link resolves to
// a run or to nothing at all.

func TestRunFromHostReadsTheSubdomain(t *testing.T) {
	cases := []struct {
		host string
		want string
		ok   bool
	}{
		{"11c18b9c683b.localhost:8090", "11c18b9c683b", true},
		{"11c18b9c683b.localhost", "11c18b9c683b", true},
		{"11c18b9c683b.ai-os.test", "11c18b9c683b", true},
		// The control plane's own names must never be read as a run.
		{"localhost:8090", "", false},
		{"127.0.0.1:8090", "", false},
		{"", "", false},
		{"localhost", "", false},
		{"short.localhost", "", false},
		{"nothex12.localhost", "", false},
		{"11C18B9C683B.localhost", "", false},
	}
	for _, c := range cases {
		got, ok := runFromHost(c.host)
		if got != c.want || ok != c.ok {
			t.Errorf("runFromHost(%q) = (%q, %v), want (%q, %v)", c.host, got, ok, c.want, c.ok)
		}
	}
}

func TestSiteURLKeepsThePort(t *testing.T) {
	cases := map[string]string{
		"localhost:8090": "http://11c18b9c683b.localhost:8090",
		// A link built from a loopback IP would not resolve in any browser, so
		// it is normalised to the name that does.
		"127.0.0.1:8090": "http://11c18b9c683b.localhost:8090",
		"localhost":      "http://11c18b9c683b.localhost",
		"box.lan:8090":   "http://11c18b9c683b.box.lan:8090",
		// Already addressed by a run id: that host is already the answer.
		"11c18b9c683b.localhost:9": "http://11c18b9c683b.localhost:9",
		"":                         "http://11c18b9c683b.localhost",
	}
	for host, want := range cases {
		if got := siteURL(host, "11c18b9c683b"); got != want {
			t.Errorf("siteURL(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestDirOfHandlesSiteRootsAtEveryDepth(t *testing.T) {
	cases := map[string]string{
		"index.html":                ".",
		"portfolio/dist/index.html": "portfolio/dist",
		"a/b/c/index.html":          "a/b/c",
	}
	for rel, want := range cases {
		if got := dirOf(rel); got != want {
			t.Errorf("dirOf(%q) = %q, want %q", rel, got, want)
		}
	}
}
