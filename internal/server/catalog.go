package server

import "net/http"

const (
	modelObject = "model"
	listObject  = "list"
	modelOwner  = "llama-swap"
	healthOK    = "ok"
	probeOK     = "OK"
	probeClosed = "Shutting down"
)

type model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string  `json:"object"`
	Data   []model `json:"data"`
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	models := []model{
		{ID: s.cfg.TTSModel, Object: modelObject, OwnedBy: modelOwner},
		{ID: s.cfg.ASRModel, Object: modelObject, OwnedBy: modelOwner},
	}
	s.writeJSON(w, http.StatusOK, modelList{Object: listObject, Data: models})
}

func (s *Server) handleVoices(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string][]string{"voices": s.voices})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": healthOK})
}

func (s *Server) handleLiveness(w http.ResponseWriter, _ *http.Request) {
	s.writeText(w, http.StatusOK, probeOK)
}

func (s *Server) handleReadiness(w http.ResponseWriter, _ *http.Request) {
	if s.shuttingDown.Load() {
		s.writeText(w, http.StatusServiceUnavailable, probeClosed)
		return
	}

	s.writeText(w, http.StatusOK, probeOK)
}
