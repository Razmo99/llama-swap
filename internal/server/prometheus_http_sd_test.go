package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// httpSDResponse decodes a Prometheus HTTP service discovery payload.
type httpSDResponse struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// TestServer_PrometheusHTTPSD_OnlyReadyLocalModels verifies the discovery
// endpoint lists exactly the ready local models and nothing else: unloaded
// local models and peer models are excluded.
func TestServer_PrometheusHTTPSD_OnlyReadyLocalModels(t *testing.T) {
	local := newStubRouter([]string{"model1", "model2"}, "")
	local.running = map[string]process.ProcessState{
		"model1": process.StateReady,
		"model2": process.StateStarting,
	}
	peer := newStubRouter([]string{"peer-model-a"}, "")

	s := newTestServerWithConfig(
		config.Config{
			Models: map[string]config.ModelConfig{
				"model1": {Name: "Model One", Proxy: "http://model1:8080"},
				"model2": {Name: "Model Two", Proxy: "http://model2:8080"},
			},
		},
		local, peer,
	)

	req := httptest.NewRequest(http.MethodGet, "/api/prometheus/http_sd", nil)
	req.Host = "llama-swap.example:8080"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q want 200", w.Code, w.Body.String())
	}

	var resp []httpSDResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%q", err, w.Body.String())
	}
	if len(resp) != 1 {
		t.Fatalf("groups=%d want 1 (only ready local model) body=%q", len(resp), w.Body.String())
	}

	got := resp[0]
	if len(got.Targets) != 1 || got.Targets[0] != "llama-swap.example:8080" {
		t.Errorf("targets=%v want [llama-swap.example:8080]", got.Targets)
	}
	wantLabels := map[string]string{
		"__scheme__":       "http",
		"__metrics_path__": "/upstream/model1/metrics",
		"job":              "llama.cpp",
		"model":            "model1",
		"state":            "ready",
		"managed_by":       "llama-swap",
	}
	for k, want := range wantLabels {
		if got.Labels[k] != want {
			t.Errorf("label %s=%q want %q", k, got.Labels[k], want)
		}
	}
	if got.Labels["model_name"] != "Model One" {
		t.Errorf("model_name=%q want %q", got.Labels["model_name"], "Model One")
	}
	if strings.Contains(w.Body.String(), "model2") {
		t.Errorf("body must not contain unloaded model2: %q", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "peer-model-a") {
		t.Errorf("body must not contain peer model: %q", w.Body.String())
	}
}

// TestServer_UpstreamMetricsDoesNotStartStoppedModel verifies a metrics scrape
// of a stopped model returns 503 and never reaches the router (which would
// load the model).
func TestServer_UpstreamMetricsDoesNotStartStoppedModel(t *testing.T) {
	local := newStubRouter([]string{"model1"}, "")
	local.running = map[string]process.ProcessState{}
	served := false
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}

	s := newTestServerWithConfig(
		config.Config{Models: map[string]config.ModelConfig{"model1": {Proxy: "http://model1:8080"}}},
		local, newStubRouter(nil, ""),
	)

	req := httptest.NewRequest(http.MethodGet, "/upstream/model1/metrics", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%q want 503", w.Code, w.Body.String())
	}
	if served {
		t.Errorf("router was served for a stopped model; scrape would trigger a swap")
	}
}

// TestServer_UpstreamMetricsDoesNotEvictRunningModel verifies scraping metrics
// for a stopped model leaves the running model's state untouched and does not
// dispatch to the router.
func TestServer_UpstreamMetricsDoesNotEvictRunningModel(t *testing.T) {
	local := newStubRouter([]string{"model1", "model2"}, "")
	local.running = map[string]process.ProcessState{"model2": process.StateReady}
	served := false
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}

	s := newTestServerWithConfig(
		config.Config{Models: map[string]config.ModelConfig{
			"model1": {Proxy: "http://model1:8080"},
			"model2": {Proxy: "http://model2:8080"},
		}},
		local, newStubRouter(nil, ""),
	)

	req := httptest.NewRequest(http.MethodGet, "/upstream/model1/metrics", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%q want 503", w.Code, w.Body.String())
	}
	if served {
		t.Errorf("router was served; scrape must not dispatch when target is stopped")
	}
	if local.running["model2"] != process.StateReady {
		t.Errorf("model2 state changed: got %v want ready", local.running["model2"])
	}
	if _, ok := local.running["model1"]; ok {
		t.Errorf("model1 became running; scrape must not start it")
	}
}

// TestServer_UpstreamMetricsReadyProxiesRewrittenPath verifies a metrics scrape
// of a ready model is proxied with the /upstream/<model> prefix stripped, so
// the upstream process receives /metrics.
func TestServer_UpstreamMetricsReadyProxiesRewrittenPath(t *testing.T) {
	var gotPath string
	local := newStubRouter([]string{"model1"}, "")
	local.running = map[string]process.ProcessState{"model1": process.StateReady}
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}

	s := newTestServerWithConfig(
		config.Config{Models: map[string]config.ModelConfig{"model1": {Proxy: "http://model1:8080"}}},
		local, newStubRouter(nil, ""),
	)

	req := httptest.NewRequest(http.MethodGet, "/upstream/model1/metrics", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q want 200", w.Code, w.Body.String())
	}
	if gotPath != "/metrics" {
		t.Errorf("upstream path=%q want /metrics", gotPath)
	}
}
