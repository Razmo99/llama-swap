package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// prometheusHTTPSDTargetGroup is one entry in a Prometheus HTTP service
// discovery response. Prometheus polls the endpoint and scrapes each target
// using the __scheme__ / __metrics_path__ labels.
type prometheusHTTPSDTargetGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// handleAPIPrometheusHTTPSD serves Prometheus HTTP service discovery for the
// ready local models. Each ready model becomes a target pointing at its own
// /upstream/<model>/metrics passthrough, so a single Prometheus scrape config
// discovers every loaded model without knowing the model names ahead of time.
//
// Only ready local models are listed: unloaded models would fail the scrape, and
// peer models are scraped through their own llama-swap instance.
func (s *Server) handleAPIPrometheusHTTPSD(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	host := strings.TrimSpace(r.Host)
	if host == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "request host required")
		return
	}

	running := s.local.RunningModels()
	groups := make([]prometheusHTTPSDTargetGroup, 0, len(running))
	for id, state := range running {
		if state != process.StateReady {
			continue
		}
		mc := s.cfg.Models[id]
		groups = append(groups, prometheusHTTPSDTargetGroup{
			Targets: []string{host},
			Labels: map[string]string{
				"__scheme__":       scheme,
				"__metrics_path__": "/upstream/" + id + "/metrics",
				"job":              "llama.cpp",
				"model":            id,
				"model_name":       mc.Name,
				"state":            string(state),
				"managed_by":       "llama-swap",
			},
		})
	}

	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Labels["model"] < groups[j].Labels["model"]
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(groups)
}
