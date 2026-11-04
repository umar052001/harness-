package environment

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// AI responses may take minutes before sending headers. Keep connection setup
// bounded; request cancellation still closes an in-flight upstream request.
var appTransport = &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ResponseHeaderTimeout: 2 * time.Minute,
	DisableKeepAlives:     true,
}

// NewAppProxy shares the runtime's transport policy between previews and checks.
// address is a host-allocated loopback service URL, never a user-supplied target.
func NewAppProxy(address string) *httputil.ReverseProxy {
	target, _ := url.Parse(address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = appTransport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "app service unavailable or response timed out", http.StatusBadGateway)
	}
	return proxy
}
