package api

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) databaseProject(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.store == nil || s.environments == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace storage unavailable"})
		return "", false
	}
	id := r.PathValue("id")
	if _, err := s.store.Dir(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "project not found"})
		return "", false
	}
	if project := s.store.metadata(id).ProjectID; project != "" {
		id = project
	}
	return id, true
}

func (s *Server) handleDatabaseStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := s.databaseProject(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	db, err := s.environments.Status(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, db)
}

func (s *Server) handleDatabaseStart(w http.ResponseWriter, r *http.Request) {
	id, ok := s.databaseProject(w, r)
	if !ok {
		return
	}
	if !s.active.beginMaintenance() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "wait for active jobs or service operations to finish"})
		return
	}
	defer s.active.endMaintenance()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	db, err := s.environments.Start(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, db)
}

func (s *Server) handleDatabaseStop(w http.ResponseWriter, r *http.Request) {
	id, ok := s.databaseProject(w, r)
	if !ok {
		return
	}
	if !s.active.beginMaintenance() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "stop active jobs before stopping a database"})
		return
	}
	defer s.active.endMaintenance()
	if s.apps.Status("").Active {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "stop the running app before stopping a database"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.environments.Stop(ctx, id); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "stopped", "dataRetained": true})
}
