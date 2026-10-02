package api

import "testing"

// These tests pin down the rewrite that makes a generated site render. The
// failure they guard against is not subtle at the byte level - the rewrites are
// all simple - but at the level of "which directory is the site root", which is
// wrong silently and produces a blank preview with a 200 in the network tab.

func TestRewriteRootRelativeHandlesViteOutput(t *testing.T) {
	// Realistic vite build output: an entry module, a stylesheet with a font,
	// and a dynamic import. Every one of these is root-absolute.
	const built = `<!doctype html>
<html><head>
<link rel="stylesheet" crossorigin href="/assets/index-DaF0sT.css">
</head><body>
<div id="root"></div>
<script type="module" crossorigin src="/assets/index-BbQ1x9.js"></script>
</body></html>`

	got := rewriteRootRelative(built, "dist", mountFor("abc123def456"))
	for _, want := range []string{
		`href="/api/workspace/abc123def456/raw/dist/assets/index-DaF0sT.css"`,
		`src="/api/workspace/abc123def456/raw/dist/assets/index-BbQ1x9.js"`,
	} {
		if !contains(got, want) {
			t.Errorf("rewritten page is missing %s\n%s", want, got)
		}
	}
	// A raw absolute asset path must be gone, or the browser asks the dashboard
	// for it and gets the SPA back.
	if contains(got, `src="/assets/`) {
		t.Errorf("a root-absolute reference survived the rewrite:\n%s", got)
	}
}

func TestRewriteRootRelativeHandlesCSSAndJS(t *testing.T) {
	mount := mountFor("run1")

	css := `@font-face { src: url(/assets/inter.woff2) format("woff2"); }
.hero { background: url("/assets/bg.png") no-repeat; }`
	gotCSS := rewriteRootRelative(css, "dist", mount)
	for _, want := range []string{
		`url(/api/workspace/run1/raw/dist/assets/inter.woff2)`,
		`url("/api/workspace/run1/raw/dist/assets/bg.png")`,
	} {
		if !contains(gotCSS, want) {
			t.Errorf("css is missing %s\n%s", want, gotCSS)
		}
	}

	js := `const lazy = () => import("/assets/lazy-chunk.js");
const cfg = { api: "/api/data" };`
	gotJS := rewriteRootRelative(js, ".", mount)
	if !contains(gotJS, `import("/api/workspace/run1/raw/assets/lazy-chunk.js")`) {
		t.Errorf("dynamic import not remapped:\n%s", gotJS)
	}
	// Only subresource references are touched. A string in a script that happens
	// to look like a path is data, and rewriting data corrupts the program.
	if !contains(gotJS, `"/api/data"`) {
		t.Errorf("an ordinary string in a script was rewritten:\n%s", gotJS)
	}
}

func TestRewriteRootRelativeLeavesTheRestAlone(t *testing.T) {
	const page = `<!doctype html>
<a href="#top">top</a>
<link rel="canonical" href="https://example.com/page">
<img src="data:image/png;base64,AAAA">
<script src="relative/thing.js"></script>
<meta property="og:url" content="https://example.com">`
	got := rewriteRootRelative(page, "dist", "/mnt/")
	if got != page {
		t.Errorf("unrelated references were modified:\ngot  %s\nwant %s", got, page)
	}
}

func TestRewriteRootRelativeKeepsQueryAndFragment(t *testing.T) {
	got := rewriteRootRelative(`<img src="/assets/a.png?v=2#x">`, "dist", "/m/")
	if want := `src="/m/dist/assets/a.png?v=2#x"`; !contains(got, want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestRemapRefRefusesToClimbOut(t *testing.T) {
	// Left verbatim rather than guessed at. The follow-up request is validated
	// anyway, but silently resolving ".." on a generated page's behalf is the
	// kind of guess that turns into a read of something else later.
	got := remapRef("/../../etc/passwd", "dist", "/m/")
	if got != "/../../etc/passwd" {
		t.Errorf("got %q, want the reference unchanged", got)
	}
}

func TestRemapRefHandlesSiteRoot(t *testing.T) {
	if got, want := remapRef("/", "dist", "/m/"), "/m/dist/"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := remapRef("/", ".", "/m/"), "/m/"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPreviewRootFor(t *testing.T) {
	cases := []struct {
		name, rel string
		isHTML    bool
		preview   string
		want      string
	}{
		{"html roots its own dir", "dist/index.html", true, "", "dist"},
		{"html at the top", "index.html", true, "", "."},
		{"css follows the preview", "dist/assets/a.css", false, "dist/index.html", "dist"},
		{"css with no preview falls back", "src/a.css", false, "", "src"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := previewRootFor(tc.rel, tc.isHTML, tc.preview); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRewritableContentType(t *testing.T) {
	for _, ct := range []string{"text/html; charset=utf-8", "text/css; charset=utf-8", "text/javascript", "application/javascript"} {
		if !rewritableContentType(ct) {
			t.Errorf("%q should be rewritten", ct)
		}
	}
	for _, ct := range []string{"image/png", "application/json", "font/woff2"} {
		if rewritableContentType(ct) {
			t.Errorf("%q must be served byte for byte", ct)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
