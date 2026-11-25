package reasoningeffort

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"encoding/json/v2"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	cjson "github.com/deorth-kku/go-common/json"
	"go.uber.org/zap"
)

// Default values applied when Upstreams is configured without explicit
// probe settings.
const (
	defaultProbeInterval = 10 * time.Second
	defaultProbeTimeout  = 5 * time.Second
)

// Upstream is a single llama.cpp backend. The order of entries in
// ReasoningEffort.Upstreams is the priority order used when two upstreams
// list the same model ID.
type Upstream struct {
	// Dial is the host:port of the backend's HTTP API, e.g.
	// "example.lan:8080".
	Dial string `json:"dial"`
}

// upstreamState is the runtime state of one configured upstream: the
// Caddy reverse proxy used to forward requests to it, plus the latest
// probe results.
type upstreamState struct {
	dial string
	rp   *reverseproxy.Handler

	mu      sync.RWMutex
	healthy bool
	models  []modelEntry
}

// snapshot returns a consistent view of the probe state.
func (u *upstreamState) snapshot() (healthy bool, models []modelEntry) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.healthy, u.models
}

// markHealthy records a successful probe and replaces the model list.
func (u *upstreamState) markHealthy(models []modelEntry) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.healthy = true
	u.models = models
}

// markUnhealthy records a failed probe, keeping the last known models.
func (u *upstreamState) markUnhealthy() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.healthy = false
}

// probe fetches the upstream's /v1/models listing and updates the probe
// state. It returns the fresh list and true on success; on failure it
// returns the last known list (possibly empty) and false.
func (u *upstreamState) probe(client *http.Client, timeout time.Duration, log *zap.Logger) ([]modelEntry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+u.dial+defaultModelsPath, nil)
	if err != nil {
		log.Warn("upstream probe: failed to build request",
			zap.String("dial", u.dial), zap.Error(err))
		u.markUnhealthy()
		_, models := u.snapshot()
		return models, false
	}
	res, err := client.Do(req)
	if err != nil {
		log.Warn("upstream probe failed",
			zap.String("dial", u.dial), zap.Error(err))
		u.markUnhealthy()
		_, models := u.snapshot()
		return models, false
	}
	defer res.Body.Close()
	var listing modelsResponse
	if res.StatusCode != http.StatusOK || json.UnmarshalRead(res.Body, &listing) != nil {
		log.Warn("upstream probe: bad model listing",
			zap.String("dial", u.dial), zap.Int("status", res.StatusCode))
		u.markUnhealthy()
		_, models := u.snapshot()
		return models, false
	}
	u.markHealthy(listing.Data)
	return listing.Data, true
}

// selectUpstream picks the upstream that should serve model, following
// the priority rules, in order:
//  1. the first (in priority order) healthy upstream that has the model
//     loaded;
//  2. the first healthy upstream that lists the model (loaded or not);
//  3. the first upstream that lists the model at all (the probe may be
//     stale);
//  4. (-1, false) when no upstream lists the model.
func (m ReasoningEffort) selectUpstream(model string) (int, bool) {
	type state struct {
		healthy, has, loaded bool
	}
	states := make([]state, len(m.upstreams))
	for i, u := range m.upstreams {
		healthy, models := u.snapshot()
		states[i] = state{
			healthy: healthy,
			has:     modelInList(models, model),
			loaded:  modelLoaded(models, model),
		}
	}
	for pass := 0; pass < 3; pass++ {
		for i, s := range states {
			switch pass {
			case 0:
				if s.has && s.healthy && s.loaded {
					return i, true
				}
			case 1:
				if s.has && s.healthy {
					return i, true
				}
			case 2:
				if s.has {
					return i, true
				}
			}
		}
	}
	return -1, false
}

func modelInList(models []modelEntry, id string) bool {
	for _, e := range models {
		if e.ID == id {
			return true
		}
	}
	return false
}

func modelLoaded(models []modelEntry, id string) bool {
	for _, e := range models {
		if e.ID == id {
			return e.Status.Value == "loaded"
		}
	}
	return false
}

// firstUsableUpstream returns the index of the first healthy upstream, or
// 0 when none is healthy (the probe may be stale; the request will fail
// visibly and the client can retry).
func (m ReasoningEffort) firstUsableUpstream() int {
	for i, u := range m.upstreams {
		healthy, _ := u.snapshot()
		if healthy {
			return i
		}
	}
	return 0
}

// upstreamHandler returns a handler that forwards requests to the
// upstream at index i. The reverse proxy's own next handler (only used by
// its Routes feature, which we do not configure) answers 503.
func (m ReasoningEffort) upstreamHandler(i int) caddyhttp.Handler {
	rp := m.upstreams[i].rp
	return caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		return rp.ServeHTTP(w, r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			http.Error(w, "no upstream routes", http.StatusServiceUnavailable)
			return nil
		}))
	})
}

// startProber probes every upstream's /v1/models on m.ProbeInterval until
// the Caddy context is done.
func (m *ReasoningEffort) startProber(ctx caddy.Context) {
	go func() {
		ticker := time.NewTicker(time.Duration(m.ProbeInterval))
		defer ticker.Stop()
		m.probeAll()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.probeAll()
			}
		}
	}()
}

// probeAll probes every upstream in parallel.
func (m *ReasoningEffort) probeAll() {
	log := m.log
	if log == nil {
		log = zap.NewNop()
	}
	var wg sync.WaitGroup
	for _, u := range m.upstreams {
		wg.Add(1)
		go func(u *upstreamState) {
			defer wg.Done()
			u.probe(m.probeClient, time.Duration(m.ProbeTimeout), log)
		}(u)
	}
	wg.Wait()
}

// mergeModelLists merges per-upstream model lists (in priority order)
// into a single list. For an ID present on several upstreams, the entry
// of the first upstream that has it loaded wins; when no upstream has it
// loaded, the entry of the first upstream that lists it wins. The result
// follows the first upstream's list order, then IDs that only exist on
// later upstreams, in their list order.
func mergeModelLists(lists [][]modelEntry) []modelEntry {
	pick := make(map[string]modelEntry)
	var order []string
	for _, list := range lists {
		for _, e := range list {
			if e.ID == "" {
				continue
			}
			if _, seen := pick[e.ID]; !seen {
				order = append(order, e.ID)
			}
			// First loaded wins; otherwise the earliest listing wins.
			if cur, ok := pick[e.ID]; !ok || (!isLoadedEntry(cur) && isLoadedEntry(e)) {
				pick[e.ID] = e
			}
		}
	}
	merged := make([]modelEntry, 0, len(order))
	for _, id := range order {
		merged = append(merged, pick[id])
	}
	return merged
}

func isLoadedEntry(e modelEntry) bool {
	return e.Status.Value == "loaded"
}

// serveMergedModels serves the merged /v1/models listing in terminating
// mode: it fetches every upstream's listing in parallel (refreshing the
// probe state as a side effect), merges them, applies the same
// enrichment as middleware mode, and streams the result out.
func (m ReasoningEffort) serveMergedModels(w http.ResponseWriter, r *http.Request) error {
	log := m.log
	if log == nil {
		log = zap.NewNop()
	}

	// Fall back to defaults when the module was constructed directly (e.g.
	// in tests) without going through Provision.
	client := m.probeClient
	if client == nil {
		client = http.DefaultClient
	}
	timeout := time.Duration(m.ProbeTimeout)
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}

	results := make([][]modelEntry, len(m.upstreams))
	var wg sync.WaitGroup
	for i, u := range m.upstreams {
		wg.Add(1)
		go func(i int, u *upstreamState) {
			defer wg.Done()
			list, _ := u.probe(client, timeout, log)
			results[i] = list
		}(i, u)
	}
	wg.Wait()

	// probe returns the last-known list on failure, so results already
	// contains any stale data; a single merge covers fresh and stale alike.
	merged := mergeModelLists(results)
	if len(merged) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		return nil
	}

	// Annotate each entry with the upstream that would serve it, using
	// the same selection rules as request routing. Every merged ID is
	// listed by at least one upstream, so this always resolves.
	for i := range merged {
		if idx, ok := m.selectUpstream(merged[i].ID); ok {
			merged[i].Routing = m.upstreams[idx].dial
		}
	}

	resp := modelsResponse{Object: "list", Data: merged}
	patchModels(&resp, m.ModelConfig, m.ModelConfigs)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	jr := cjson.NewJsonReader(resp)
	defer jr.Close()
	_, err := io.Copy(w, jr)
	return err
}

// modelFromRequest extracts the model identifier from a request, by
// HTTP method: methods that carry a body (POST, PUT, PATCH) have it in
// the JSON body's "model" field; methods without a body (GET, HEAD,
// DELETE) have it as a URL argument. Unknown methods are treated as
// body-less.
func modelFromRequest(r *http.Request) string {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return modelFromBody(r)
	case http.MethodGet, http.MethodHead, http.MethodDelete:
		return r.URL.Query().Get("model")
	default:
		return r.URL.Query().Get("model")
	}
}

// modelFromBody extracts the "model" field from a JSON request body,
// restoring the body for the downstream handler. It returns "" when the
// body has no model field or is not valid JSON.
func modelFromBody(r *http.Request) string {
	if r.ContentLength <= 0 {
		return ""
	}
	bodyCopy := bytes.Buffer{}
	tee := io.TeeReader(r.Body, &bodyCopy)
	var req struct {
		Model string `json:"model,omitzero"`
	}
	if json.UnmarshalRead(tee, &req) != nil {
		r.Body = io.NopCloser(&bodyCopy)
		return ""
	}
	r.Body = io.NopCloser(&bodyCopy)
	return req.Model
}

// writeModelNotFound answers with an OpenAI-style 404 for a model that
// no upstream lists.
func writeModelNotFound(w http.ResponseWriter, model string) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": fmt.Sprintf("model %q not found on any upstream", model),
			"type":    "model_not_found",
		},
	})
	_, err := w.Write(body)
	return err
}
