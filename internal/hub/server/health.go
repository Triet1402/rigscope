package server

import (
	"encoding/json"
	"io"
	"net/http"
)

// healthResponse is the exact /api/health document from the browser API
// contract. Field order is stable so captured fixtures stay byte-identical.
type healthResponse struct {
	Status   string `json:"status"`
	Version  string `json:"version"`
	Mode     string `json:"mode"`
	HubRunID string `json:"hubRunId"`
}

// serveHealth answers the unauthenticated CLI/installer/proxy health check.
// A wrong method keeps the prototype's text/plain 405 with Allow: GET, HEAD;
// docs/rewrite/api-v1.md lists this as the one JSON route whose method error is
// deliberately not JSON, so the 1.0 port preserves it.
func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, "method not allowed")
		return
	}
	body, err := json.Marshal(healthResponse{
		Status:   "ok",
		Version:  s.opts.Version,
		Mode:     "go",
		HubRunID: s.runID,
	})
	if err != nil {
		http.Error(w, "health unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
