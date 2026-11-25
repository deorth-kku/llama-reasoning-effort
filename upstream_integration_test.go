package reasoningeffort

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"encoding/json/v2"

	"github.com/caddyserver/caddy/v2"
	"golang.org/x/exp/collection/set"
)

// fakeBackend emulates a llama.cpp server: it serves a fixed /v1/models
// listing and records the chat completion requests it receives. All
// identifiers are synthetic placeholders.
type fakeBackend struct {
	modelsJSON string

	mu       sync.Mutex
	chatReqs int
	loadReqs []string
}

func (b *fakeBackend) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(b.modelsJSON))
		case r.URL.Path == "/models/load" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			b.mu.Lock()
			b.loadReqs = append(b.loadReqs, string(body))
			b.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
			_, _ = io.ReadAll(r.Body)
			b.mu.Lock()
			b.chatReqs++
			b.mu.Unlock()

			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "data: {\"choices\":[]}\n\n")
			fl.Flush()
			time.Sleep(200 * time.Millisecond) // simulate token latency
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
		default:
			http.NotFound(w, r)
		}
	})
}

func (b *fakeBackend) chatReqsCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.chatReqs
}

func (b *fakeBackend) loadReqsCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.loadReqs)
}

func dialOf(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://")
}

// freePort returns a TCP port that is (probably) free.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// postChat posts a chat completion request with the given model and
// returns the response.
func postChat(t *testing.T, base, model string) *http.Response {
	t.Helper()
	res, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(fmt.Sprintf(`{"model":%q}`, model)))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions (model %q): %v", model, err)
	}
	return res
}

// waitModelListed polls the merged /v1/models listing until it contains
// every wanted model ID, or fails the test on timeout.
func waitModelListed(t *testing.T, base string, wanted []string) modelsResponse {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(base + "/v1/models")
		if err == nil {
			body, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			var listing modelsResponse
			if err := json.Unmarshal(body, &listing); err == nil {
				have := set.Set[string]{}
				for _, e := range listing.Data {
					have.Insert(e.ID)
				}
				all := true
				for _, id := range wanted {
					if !have.Contains(id) {
						all = false
						break
					}
				}
				if all {
					return listing
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for models %v in merged listing", wanted)
	return modelsResponse{}
}

func TestUpstreamRoutingIntegration(t *testing.T) {
	// Upstream a hosts model-a (loaded) and model-shared (not loaded).
	// Upstream b hosts model-a (not loaded), model-b (loaded) and
	// model-shared (loaded).
	a := &fakeBackend{modelsJSON: `{"object":"list","data":[
		{"id":"model-a","status":{"value":"loaded"}},
		{"id":"model-shared","status":{"value":"loading"}}]}`}
	b := &fakeBackend{modelsJSON: `{"object":"list","data":[
		{"id":"model-a","status":{"value":"loading"}},
		{"id":"model-b","status":{"value":"loaded"}},
		{"id":"model-shared","status":{"value":"loaded"}}]}`}

	srvA := httptest.NewServer(a.handler())
	defer srvA.Close()
	srvB := httptest.NewServer(b.handler())
	defer srvB.Close()

	listenPort := freePort(t)
	cfg := fmt.Sprintf(`{
		"admin": {"disabled": true},
		"apps": {
			"http": {
				"servers": {
					"srv0": {
						"listen": ["127.0.0.1:%d"],
						"routes": [{
							"handle": [{
								"handler": "reasoning_effort",
								"upstreams": [{"dial": "%s"}, {"dial": "%s"}],
								"probe_interval": "500ms",
								"probe_timeout": "2s"
							}]
						}]
					}
				}
			}
		}
	}`, listenPort, dialOf(srvA), dialOf(srvB))

	if err := caddy.Load([]byte(cfg), true); err != nil {
		t.Fatalf("caddy.Load: %v", err)
	}

	base := fmt.Sprintf("http://127.0.0.1:%d", listenPort)

	// The on-demand /v1/models merge also refreshes probe state, so once
	// this returns, selection has current data from both upstreams.
	listing := waitModelListed(t, base, []string{"model-a", "model-b", "model-shared"})

	// The merged entry for model-shared must come from upstream b, which
	// has it loaded.
	for _, e := range listing.Data {
		if e.ID == "model-shared" && e.Status.Value != "loaded" {
			t.Fatalf("expected the loaded (upstream b) entry for model-shared, got %+v", e)
		}
	}

	// model-a is loaded on a → routes to a (loaded beats b's listing).
	res := postChat(t, base, "model-a")
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("model-a: expected 200, got %d (%s)", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("model-a: expected SSE [DONE] in response, got %q", body)
	}
	if got := a.chatReqsCount(); got != 1 {
		t.Fatalf("expected 1 chat request on upstream a, got %d", got)
	}
	if got := b.chatReqsCount(); got != 0 {
		t.Fatalf("expected 0 chat requests on upstream b, got %d", got)
	}

	// model-shared is loaded on b → routes to b despite a listing it first.
	res = postChat(t, base, "model-shared")
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if got := b.chatReqsCount(); got != 1 {
		t.Fatalf("expected model-shared to route to upstream b, got b=%d a=%d", got, a.chatReqsCount())
	}

	// model-b exists only on b → routes to b.
	res = postChat(t, base, "model-b")
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if got := b.chatReqsCount(); got != 2 {
		t.Fatalf("expected model-b to route to upstream b, got b=%d", got)
	}

	// llama.cpp's /models/load carries the model in the JSON body, not
	// in the URL query: model-b must reach upstream b.
	res, err := http.Post(base+"/models/load", "application/json",
		strings.NewReader(`{"model":"model-b"}`))
	if err != nil {
		t.Fatalf("POST /models/load: %v", err)
	}
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/models/load: expected 200, got %d", res.StatusCode)
	}
	if got := b.loadReqsCount(); got != 1 {
		t.Fatalf("expected /models/load to route to upstream b, got b=%d a=%d", b.loadReqsCount(), a.loadReqsCount())
	}
	if got := a.loadReqsCount(); got != 0 {
		t.Fatalf("expected no /models/load on upstream a, got %d", got)
	}

	// An unknown model gets a 404 and hits neither upstream.
	res = postChat(t, base, "model-x")
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("model-x: expected 404, got %d", res.StatusCode)
	}

	// Kill upstream a: once the probe marks it unhealthy, model-a must
	// fall back to b (which lists it too, unloaded).
	srvA.Close()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		res = postChat(t, base, "model-a")
		code := res.StatusCode
		_, _ = io.ReadAll(res.Body)
		_ = res.Body.Close()
		if code == http.StatusOK && b.chatReqsCount() >= 3 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if got := b.chatReqsCount(); got < 3 {
		t.Fatalf("expected model-a to fall back to upstream b after a died, got b=%d", got)
	}
}
