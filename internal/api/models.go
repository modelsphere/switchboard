// Package api serves the OpenAI-compatible model listing.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/modelsphere/switchboard/internal/gateway"
)

// ConsumerHeader is set by Higress key-auth to the authenticated consumer
// name; key-auth removes any client-sent copy first.
const ConsumerHeader = "X-Mse-Consumer"

// Source provides the current gateway catalog.
type Source interface {
	Catalog() *gateway.Catalog
	Ready() bool
}

// Server handles /v1/models.
type Server struct {
	Source  Source
	OwnedBy string
}

type model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type list struct {
	Object string  `json:"object"`
	Data   []model `json:"data"`
}

type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.list)
	mux.HandleFunc("GET /v1/models/{id...}", s.get)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.Source.Ready() {
			http.Error(w, "gateway config not synced", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) ([]gateway.Model, bool) {
	consumer := r.Header.Get(ConsumerHeader)
	if consumer == "" {
		writeError(w, http.StatusUnauthorized, apiError{
			Message: "Request is not authenticated: no consumer identity from the gateway.",
			Type:    "invalid_request_error",
			Code:    "invalid_api_key",
		})
		return nil, false
	}
	if !s.Source.Ready() {
		writeError(w, http.StatusServiceUnavailable, apiError{
			Message: "Model list is not available yet.",
			Type:    "server_error",
			Code:    "service_unavailable",
		})
		return nil, false
	}
	return s.Source.Catalog().Models(consumer, r.Host), true
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	ms, ok := s.models(w, r)
	if !ok {
		return
	}
	out := list{Object: "list", Data: make([]model, 0, len(ms))}
	for _, m := range ms {
		out.Data = append(out.Data, s.toModel(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	ms, ok := s.models(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	for _, m := range ms {
		if m.ID == id {
			writeJSON(w, http.StatusOK, s.toModel(m))
			return
		}
	}
	// Same answer for "not served" and "not allowed": no existence probing.
	param := "model"
	writeError(w, http.StatusNotFound, apiError{
		Message: "The model '" + id + "' does not exist or you do not have access to it.",
		Type:    "invalid_request_error",
		Param:   &param,
		Code:    "model_not_found",
	})
}

func (s *Server) toModel(m gateway.Model) model {
	var created int64
	if !m.Created.IsZero() {
		created = m.Created.Unix()
	}
	return model{ID: m.ID, Object: "model", Created: created, OwnedBy: s.OwnedBy}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, e apiError) {
	writeJSON(w, code, map[string]apiError{"error": e})
}
