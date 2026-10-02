package api

import (
	"path"
	"regexp"
	"strings"
)

// Preview support: making a generated site actually run inside the dashboard.
//
// A build output is written for a web server that hosts it at the root of its
// own domain. Vite writes `<script src="/assets/index-abc.js">`, which means
// "the root of this site". This dashboard serves the same bytes from
// /api/workspace/<id>/raw/<path>, where the site root is not the origin root,
// so every root-absolute reference resolves against the dashboard instead: the
// script request 404s into the SPA, and the preview is a blank page.
//
// The fix is to rewrite those references on the way out, mapping the site's root
// onto the mount the file is actually served from. It has to be done per
// request, because only the server knows the run id, and it has to be done for
// CSS and JavaScript as well as HTML: a stylesheet's `url(/assets/font.woff2)`
// is written against the same site root and fails identically.

// mountFor is the URL prefix a run's workspace is served under.
func mountFor(id string) string {
	return "/api/workspace/" + id + "/raw/"
}

// The three forms a root-absolute reference takes in build output.
//
// Only attributes and functions that address a subresource are matched. This is
// a rewrite of text a program generated, not an HTML parser: the goal is to fix
// the references that break, and to leave every other byte of the file alone.
var (
	attrRef   = regexp.MustCompile(`(?i)(\s(?:src|href|poster|data-src)\s*=\s*["'])(/[^/"'][^"']*)(["'])`)
	cssRef    = regexp.MustCompile(`(?i)(url\(\s*["']?)(/[^)"']*)(["']?\s*\))`)
	importRef = regexp.MustCompile(`(?i)(\bimport\s*\(\s*["'])(/[^"']*)(["'])`)
)

// rewriteRootRelative maps a site's root-absolute URLs onto mount.
//
// siteRoot is the directory the site is rooted at, relative to the workspace,
// which is where the site's "/" lives. A reference is remapped to the
// equivalent file under the mount, keeping any query string or fragment.
func rewriteRootRelative(body, siteRoot, mount string) string {
	remap := func(m *regexp.Regexp) func(string) string {
		return func(s string) string {
			return m.ReplaceAllStringFunc(s, func(all string) string {
				g := m.FindStringSubmatch(all)
				if g == nil {
					return all
				}
				return g[1] + remapRef(g[2], siteRoot, mount) + g[3]
			})
		}
	}
	out := remap(attrRef)(body)
	out = remap(cssRef)(out)
	out = remap(importRef)(out)
	return out
}

// remapRef resolves one root-absolute reference against the site root.
//
// A query or fragment is split off first so that path.Clean cannot discard it,
// and a reference that climbs out of the site root is left alone rather than
// rewritten: it is either an error or an attempt to address something outside
// the built site, and in both cases guessing is worse than passing it through.
// The file it ends up pointing at is re-validated by the raw handler on the next
// request either way, so nothing here can widen what is readable.
func remapRef(ref, siteRoot, mount string) string {
	p, suffix := ref, ""
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p, suffix = p[:i], p[i:]
	}
	if p == "" || p == "/" {
		if siteRoot == "." {
			return mount + suffix
		}
		return mount + siteRoot + "/" + suffix
	}
	joined := path.Join(siteRoot, p)
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return ref
	}
	return mount + joined + suffix
}

// previewRootFor decides which directory a file's site root lives in.
//
// An HTML file is its own site's root: that is what index.html means. For CSS
// and JavaScript the file carries no such signal, so the workspace's preview
// choice decides it - the stylesheet in dist/assets belongs to the site rooted
// at dist, not to a site rooted at dist/assets, and getting this wrong produces
// assets that resolve one directory too deep.
func previewRootFor(rel string, isHTML bool, previewFile string) string {
	if isHTML {
		if d := path.Dir(rel); d != "" {
			return d
		}
		return "."
	}
	if previewFile != "" {
		if d := path.Dir(previewFile); d != "" {
			return d
		}
	}
	if d := path.Dir(rel); d != "" {
		return d
	}
	return "."
}

// rewritableContentType reports whether a file is text that references other
// files by URL. Anything else is served byte for byte.
func rewritableContentType(ct string) bool {
	switch {
	case strings.HasPrefix(ct, "text/html"):
		return true
	case strings.HasPrefix(ct, "text/css"):
		return true
	case strings.Contains(ct, "javascript"):
		return true
	default:
		return false
	}
}
