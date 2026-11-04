package api

import (
	"net"
	"net/http"
	"strings"
)

// LocalHandler rejects foreign Host headers, including DNS rebinding to loopback.
// The executable binds only to loopback; this is not user authentication.
func LocalHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.ToLower(host)
		local := host == "localhost"
		if ip := net.ParseIP(host); ip != nil {
			local = ip.IsLoopback()
		}
		if id, ok := strings.CutSuffix(host, ".localhost"); ok {
			local = validID(id)
		}
		if !local {
			http.Error(w, "use localhost or a loopback IP to open harness++", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
