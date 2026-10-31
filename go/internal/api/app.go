package api

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/umar052001/ai-os/go/internal/environment"
)

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.store == nil {
		http.Error(w, "workspace storage unavailable", 503)
		return
	}
	dir, err := s.store.Dir(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	manifest, manifestErr := environment.LoadApp(dir)
	if r.Method != http.MethodGet {
		if !s.beginJob() {
			http.Error(w, "server is shutting down", 503)
			return
		}
		defer s.jobs.Done()
		if !s.active.beginMaintenance() {
			http.Error(w, "wait for active jobs or service operations", 409)
			return
		}
		defer s.active.endMaintenance()
		if r.Method == http.MethodDelete {
			s.apps.Stop(id)
		} else {
			if s.apps.Status("").Active {
				writeJSON(w, 409, map[string]any{"error": "stop the running app before building another app"})
				return
			}
			if manifestErr != nil {
				writeJSON(w, 400, map[string]any{"error": manifestErr.Error()})
				return
			}
			project := s.store.metadata(id).ProjectID
			if project == "" {
				project = id
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
			defer cancel()
			if manifest.Database == "postgres" {
				if _, err := s.environments.Start(ctx, project); err != nil {
					writeJSON(w, 503, map[string]any{"error": "database startup: " + err.Error()})
					return
				}
			}
			connection, err := s.environments.Connection(ctx, project)
			if err != nil {
				writeJSON(w, 503, map[string]any{"error": err.Error()})
				return
			}
			if _, err = s.apps.Start(ctx, s.jobContext, s.sandboxBin, id, dir, connection); err != nil {
				writeJSON(w, 503, map[string]any{"error": err.Error()})
				return
			}
			pub, err := s.publish(id)
			if err == nil {
				err = s.publishIndex.put(pub)
			}
			if err != nil {
				s.apps.Stop(id)
				writeJSON(w, 500, map[string]any{"error": err.Error()})
				return
			}
		}
	}
	writeJSON(w, 200, struct {
		environment.AppStatus
		Supported bool   `json:"supported"`
		URL       string `json:"url"`
	}{s.apps.Status(id), manifestErr == nil, siteURL(r.Host, id)})
}

func (s *Server) proxyApp(w http.ResponseWriter, r *http.Request, id string) {
	address := s.apps.Address(id, r.URL.Path)
	if address == "" {
		http.Error(w, "App server is stopped. Open the dashboard and choose Start app.", 503)
		return
	}
	// Generated apps have their own origin; never proxy them under dashboard /api.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "cross-origin app mutation refused", 403)
			return
		}
	}
	target, _ := url.Parse(address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{ResponseHeaderTimeout: 10 * time.Second, DisableKeepAlives: true}
	proxy.ModifyResponse = func(res *http.Response) error {
		res.Header.Set("Content-Security-Policy", "frame-ancestors "+dashboardAncestors(r.Host, id))
		res.Header.Set("X-Content-Type-Options", "nosniff")
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "App server is unavailable. Stop and restart it from the dashboard.", 502)
	}
	proxy.ServeHTTP(w, r)
}
