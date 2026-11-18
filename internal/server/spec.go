package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/AndreyOsipuk/telemux/api"
)

var (
	specOnce sync.Once
	specJSON []byte
	specErr  error
)

// handleSpec отдаёт OpenAPI-спеку в JSON (страница /api-docs панели и генерация типов).
func (s *Server) handleSpec(w http.ResponseWriter, _ *http.Request) {
	specOnce.Do(func() {
		doc, err := openapi3.NewLoader().LoadFromData(api.Spec)
		if err != nil {
			specErr = fmt.Errorf("разбор openapi.yaml: %w", err)
			return
		}
		specJSON, specErr = json.Marshal(doc)
	})
	if specErr != nil {
		writeErr(w, http.StatusInternalServerError, specErr, s.deps.Log)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(specJSON)
}
