package reasoningeffort

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json/v2"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"
)

// mkModel builds a synthetic model entry. marker lands in Status.Args so
// tests can tell which upstream's entry won a merge.
func mkModel(id, marker string, loaded bool) modelEntry {
	value := "loading"
	if loaded {
		value = "loaded"
	}
	return modelEntry{ID: id, Status: modelStatus{Value: value, Args: []string{marker}}}
}

func TestMergeModelLists(t *testing.T) {
	t.Run("loaded wins over priority order", func(t *testing.T) {
		merged := mergeModelLists([][]modelEntry{
			{mkModel("x", "a", false)},
			{mkModel("x", "b", true)},
		})
		if len(merged) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(merged))
		}
		if merged[0].ID != "x" || merged[0].Status.Args[0] != "b" {
			t.Fatalf("expected upstream b's entry to win, got %+v", merged[0])
		}
	})

	t.Run("priority order wins when neither is loaded", func(t *testing.T) {
		merged := mergeModelLists([][]modelEntry{
			{mkModel("x", "a", false)},
			{mkModel("x", "b", false)},
		})
		if len(merged) != 1 || merged[0].Status.Args[0] != "a" {
			t.Fatalf("expected upstream a's entry to win, got %+v", merged)
		}
	})

	t.Run("priority order wins when both are loaded", func(t *testing.T) {
		merged := mergeModelLists([][]modelEntry{
			{mkModel("x", "a", true)},
			{mkModel("x", "b", true)},
		})
		if len(merged) != 1 || merged[0].Status.Args[0] != "a" {
			t.Fatalf("expected upstream a's entry to win, got %+v", merged)
		}
	})

	t.Run("list order and dedup", func(t *testing.T) {
		merged := mergeModelLists([][]modelEntry{
			{mkModel("x", "a", false), mkModel("y", "a", false)},
			{mkModel("y", "b", false), mkModel("z", "b", false)},
		})
		if len(merged) != 3 {
			t.Fatalf("expected 3 entries, got %d", len(merged))
		}
		// Order: a's list order first (x, y), then b-only IDs (z).
		if merged[0].ID != "x" || merged[1].ID != "y" || merged[2].ID != "z" {
			t.Fatalf("unexpected order: %v", idsOf(merged))
		}
		// y is listed by both; a's entry wins (neither loaded).
		if merged[1].Status.Args[0] != "a" {
			t.Fatalf("expected upstream a's entry for y, got %+v", merged[1])
		}
	})

	t.Run("empty ids are skipped", func(t *testing.T) {
		merged := mergeModelLists([][]modelEntry{
			{mkModel("", "a", false), mkModel("x", "a", false)},
		})
		if len(merged) != 1 || merged[0].ID != "x" {
			t.Fatalf("expected only x, got %+v", merged)
		}
	})

	t.Run("all empty", func(t *testing.T) {
		if merged := mergeModelLists([][]modelEntry{{}, {}}); len(merged) != 0 {
			t.Fatalf("expected empty result, got %+v", merged)
		}
	})
}

func idsOf(entries []modelEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func TestSelectUpstream(t *testing.T) {
	mkUpstream := func(healthy bool, models ...modelEntry) *upstreamState {
		return &upstreamState{healthy: healthy, models: models}
	}

	tests := []struct {
		name      string
		upstreams []*upstreamState
		model     string
		wantIdx   int
		wantOK    bool
	}{
		{
			name:      "loaded on preferred wins",
			upstreams: []*upstreamState{mkUpstream(true, mkModel("a", "a", true)), mkUpstream(true, mkModel("a", "b", true))},
			model:     "a",
			wantIdx:   0,
			wantOK:    true,
		},
		{
			name:      "loaded on secondary beats unloaded preferred",
			upstreams: []*upstreamState{mkUpstream(true, mkModel("a", "a", false)), mkUpstream(true, mkModel("a", "b", true))},
			model:     "a",
			wantIdx:   1,
			wantOK:    true,
		},
		{
			name:      "healthy preferred wins when nothing is loaded",
			upstreams: []*upstreamState{mkUpstream(true, mkModel("a", "a", false)), mkUpstream(true, mkModel("a", "b", false))},
			model:     "a",
			wantIdx:   0,
			wantOK:    true,
		},
		{
			name:      "unhealthy preferred falls back to healthy secondary even when unloaded",
			upstreams: []*upstreamState{mkUpstream(false, mkModel("a", "a", false)), mkUpstream(true, mkModel("a", "b", false))},
			model:     "a",
			wantIdx:   1,
			wantOK:    true,
		},
		{
			name:      "last resort: preferred still listed when both are unhealthy",
			upstreams: []*upstreamState{mkUpstream(false, mkModel("a", "a", false)), mkUpstream(false, mkModel("a", "b", false))},
			model:     "a",
			wantIdx:   0,
			wantOK:    true,
		},
		{
			name:      "model only on secondary",
			upstreams: []*upstreamState{mkUpstream(true), mkUpstream(true, mkModel("a", "b", false))},
			model:     "a",
			wantIdx:   1,
			wantOK:    true,
		},
		{
			name:      "model nowhere",
			upstreams: []*upstreamState{mkUpstream(true, mkModel("b", "a", false)), mkUpstream(true, mkModel("c", "b", false))},
			model:     "a",
			wantIdx:   -1,
			wantOK:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := ReasoningEffort{upstreams: tc.upstreams}
			gotIdx, gotOK := m.selectUpstream(tc.model)
			if gotIdx != tc.wantIdx || gotOK != tc.wantOK {
				t.Fatalf("selectUpstream(%q) = (%d, %v), want (%d, %v)", tc.model, gotIdx, gotOK, tc.wantIdx, tc.wantOK)
			}
		})
	}
}

func TestFirstUsableUpstream(t *testing.T) {
	mkUpstream := func(healthy bool) *upstreamState {
		return &upstreamState{healthy: healthy}
	}

	tests := []struct {
		name      string
		upstreams []*upstreamState
		want      int
	}{
		{name: "first healthy", upstreams: []*upstreamState{mkUpstream(true), mkUpstream(true)}, want: 0},
		{name: "second healthy", upstreams: []*upstreamState{mkUpstream(false), mkUpstream(true)}, want: 1},
		{name: "none healthy falls back to first", upstreams: []*upstreamState{mkUpstream(false), mkUpstream(false)}, want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := ReasoningEffort{upstreams: tc.upstreams}
			if got := m.firstUsableUpstream(); got != tc.want {
				t.Fatalf("firstUsableUpstream() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWriteModelNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := writeModelNotFound(rec, "model-x"); err != nil {
		t.Fatalf("writeModelNotFound: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json content type, got %q", ct)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Error.Type != "model_not_found" {
		t.Fatalf("expected error type model_not_found, got %q", body.Error.Type)
	}
	if !strings.Contains(body.Error.Message, "model-x") {
		t.Fatalf("expected message to name the model, got %q", body.Error.Message)
	}
}

func TestModelFromRequest(t *testing.T) {
	body := `{"model":"model-b"}`
	tests := []struct {
		method string
		url    string
		body   string
		want   string
	}{
		{method: http.MethodGet, url: "/models/load?model=model-a", want: "model-a"},
		{method: http.MethodHead, url: "/models/unload?model=model-a", want: "model-a"},
		{method: http.MethodDelete, url: "/models/unload?model=model-a", want: "model-a"},
		{method: http.MethodPost, url: "/models/load", body: body, want: "model-b"},
		{method: http.MethodPut, url: "/models/load", body: body, want: "model-b"},
		{method: http.MethodPatch, url: "/models/load", body: body, want: "model-b"},
		// Unknown methods are treated as body-less.
		{method: "SEARCH", url: "/models/load?model=model-a", want: "model-a"},
	}
	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			var r *http.Request
			if tc.body != "" {
				r = httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body))
			} else {
				r = httptest.NewRequest(tc.method, tc.url, nil)
			}
			if got := modelFromRequest(r); got != tc.want {
				t.Fatalf("modelFromRequest = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestModelFromBody(t *testing.T) {
	t.Run("model in JSON body", func(t *testing.T) {
		body := `{"model":"model-b"}`
		r := httptest.NewRequest(http.MethodPost, "/models/load", strings.NewReader(body))
		if got := modelFromBody(r); got != "model-b" {
			t.Fatalf("modelFromBody = %q, want %q", got, "model-b")
		}
		// The body must be restored for the downstream handler.
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading restored body: %v", err)
		}
		if string(got) != body {
			t.Fatalf("restored body = %q, want %q", got, body)
		}
	})

	t.Run("JSON without model field", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/models/load", strings.NewReader(`{"foo":1}`))
		if got := modelFromBody(r); got != "" {
			t.Fatalf("modelFromBody = %q, want empty", got)
		}
	})

	t.Run("invalid JSON keeps body", func(t *testing.T) {
		body := `not-json`
		r := httptest.NewRequest(http.MethodPost, "/models/load", strings.NewReader(body))
		if got := modelFromBody(r); got != "" {
			t.Fatalf("modelFromBody = %q, want empty", got)
		}
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading restored body: %v", err)
		}
		if string(got) != body {
			t.Fatalf("restored body = %q, want %q", got, body)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/models/load", nil)
		if got := modelFromBody(r); got != "" {
			t.Fatalf("modelFromBody = %q, want empty", got)
		}
	})
}

func TestUpstreamProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected probe path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","status":{"value":"loaded"}}]}`))
	}))
	defer srv.Close()

	u := &upstreamState{dial: strings.TrimPrefix(srv.URL, "http://")}
	client := &http.Client{}
	log := zap.NewNop()

	models, ok := u.probe(client, 2*time.Second, log)
	if !ok {
		t.Fatal("expected probe to succeed")
	}
	if len(models) != 1 || models[0].ID != "model-a" {
		t.Fatalf("unexpected probe result: %+v", models)
	}
	healthy, models := u.snapshot()
	if !healthy || len(models) != 1 {
		t.Fatalf("expected healthy state with 1 model, got healthy=%v models=%v", healthy, models)
	}

	// A dead port must mark the upstream unhealthy and keep old models.
	u2 := &upstreamState{dial: "127.0.0.1:1", models: []modelEntry{mkModel("old", "x", false)}}
	if _, ok := u2.probe(client, time.Second, log); ok {
		t.Fatal("expected probe to fail")
	}
	healthy2, models2 := u2.snapshot()
	if healthy2 || len(models2) != 1 || models2[0].ID != "old" {
		t.Fatalf("expected unhealthy state with last known models, got healthy=%v models=%v", healthy2, models2)
	}
}

// TestServeMergedModelsRouting verifies that the merged /v1/models
// response carries a routing field on every entry: the dial address of
// the upstream that would serve the model, per the selection rules
// (loaded beats listing, priority order breaks ties).
func TestServeMergedModelsRouting(t *testing.T) {
	a := &fakeBackend{modelsJSON: `{"object":"list","data":[
		{"id":"model-a","status":{"value":"loaded"}},
		{"id":"model-shared","status":{"value":"loading"}}]}`}
	b := &fakeBackend{modelsJSON: `{"object":"list","data":[
		{"id":"model-b","status":{"value":"loaded"}},
		{"id":"model-shared","status":{"value":"loaded"}}]}`}
	srvA := httptest.NewServer(a.handler())
	defer srvA.Close()
	srvB := httptest.NewServer(b.handler())
	defer srvB.Close()

	m := ReasoningEffort{
		upstreams: []*upstreamState{
			{dial: dialOf(srvA)},
			{dial: dialOf(srvB)},
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, defaultModelsPath, nil)
	if err := m.serveMergedModels(rec, req); err != nil {
		t.Fatalf("serveMergedModels: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp modelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("merged body not valid JSON: %v\n%s", err, rec.Body.String())
	}

	want := map[string]string{
		"model-a":      dialOf(srvA), // loaded on a
		"model-b":      dialOf(srvB), // listed only by b
		"model-shared": dialOf(srvB), // loaded on b despite a listing it first
	}
	for _, e := range resp.Data {
		dial, ok := want[e.ID]
		if !ok {
			t.Fatalf("unexpected model %q in merged listing", e.ID)
		}
		if e.Routing != dial {
			t.Errorf("model %q: routing = %q, want %q", e.ID, e.Routing, dial)
		}
	}
}
func TestUnmarshalCaddyfileUpstreamsFlat(t *testing.T) {
	input := `reasoning_effort {
		upstreams example.lan:8080 backup.lan:8080
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	if len(m.Upstreams) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(m.Upstreams))
	}
	if m.Upstreams[0].Dial != "example.lan:8080" || m.Upstreams[1].Dial != "backup.lan:8080" {
		t.Errorf("unexpected dials: %q, %q", m.Upstreams[0].Dial, m.Upstreams[1].Dial)
	}
}

func TestUnmarshalCaddyfileUpstreamsBlock(t *testing.T) {
	input := `reasoning_effort {
		upstreams {
			example.lan:8080 {
				slot_save_path /var/lib/llama/slots-a
				slot_lru_max 16
			}
			backup.lan:8080
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	if len(m.Upstreams) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(m.Upstreams))
	}
	up := m.Upstreams[0]
	if up.Dial != "example.lan:8080" {
		t.Errorf("dial: got %q", up.Dial)
	}
	if up.SlotSavePath != "/var/lib/llama/slots-a" {
		t.Errorf("slot_save_path: got %q", up.SlotSavePath)
	}
	if up.SlotLRUMax != 16 {
		t.Errorf("slot_lru_max: got %d", up.SlotLRUMax)
	}
	if m.Upstreams[1].Dial != "backup.lan:8080" || m.Upstreams[1].SlotSavePath != "" || m.Upstreams[1].SlotLRUMax != 0 {
		t.Errorf("unexpected second upstream: %+v", m.Upstreams[1])
	}
}

func TestUnmarshalCaddyfileUpstreamsBlockUnexpectedToken(t *testing.T) {
	input := `reasoning_effort {
		upstreams {
			example.lan:8080 {
				probe_interval 10s
			}
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected error for unexpected token in upstream block, got nil")
	}
}

func TestUnmarshalCaddyfileUpstreamsBlockInvalidLRUMax(t *testing.T) {
	input := `reasoning_effort {
		upstreams {
			example.lan:8080 {
				slot_lru_max abc
			}
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected error for non-integer slot_lru_max, got nil")
	}
}

func TestValidateUpstreamSlotOptions(t *testing.T) {
	if err := validateUpstreamSlotOptions(0, Upstream{SlotLRUMax: 3}); err == nil {
		t.Error("expected error for slot_lru_max without slot_save_path")
	}
	if err := validateUpstreamSlotOptions(0, Upstream{SlotLRUMax: -1}); err == nil {
		t.Error("expected error for negative slot_lru_max")
	}
	if err := validateUpstreamSlotOptions(0, Upstream{SlotSavePath: "/tmp/slots", SlotLRUMax: 0}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := validateUpstreamSlotOptions(0, Upstream{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveUpstreamSlotLRU(t *testing.T) {
	globalDir := t.TempDir()
	sharedDir := t.TempDir()
	m := ReasoningEffort{SlotSavePath: globalDir, SlotLRUMax: 4}
	if err := m.provisionSlotLRU(zap.NewNop()); err != nil {
		t.Fatalf("provisionSlotLRU error: %v", err)
	}
	lruByConfig := map[string]*slotLRU{}
	log := zap.NewNop()

	// No slot settings at all: no LRU.
	var bare ReasoningEffort
	lru, err := bare.resolveUpstreamSlotLRU(0, Upstream{Dial: "x"}, log, map[string]*slotLRU{})
	if err != nil || lru != nil {
		t.Fatalf("no settings: got lru=%v err=%v, want nil", lru, err)
	}

	// An upstream without its own settings falls back to the
	// handler-level LRU instance.
	lru, err = m.resolveUpstreamSlotLRU(0, Upstream{Dial: "a"}, log, lruByConfig)
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if lru != m.slotLRU {
		t.Error("expected the handler-level LRU instance to be reused")
	}

	// A second upstream falling back shares the same instance.
	lru2, err := m.resolveUpstreamSlotLRU(1, Upstream{Dial: "b"}, log, lruByConfig)
	if err != nil || lru2 != m.slotLRU {
		t.Fatalf("second fallback: got %v, %v; want the shared handler-level instance", lru2, err)
	}

	// An upstream with its own settings gets a dedicated instance.
	up := Upstream{Dial: "c", SlotSavePath: sharedDir, SlotLRUMax: 2}
	lru3, err := m.resolveUpstreamSlotLRU(2, up, log, lruByConfig)
	if err != nil {
		t.Fatalf("custom path: %v", err)
	}
	if lru3 == nil || lru3 == m.slotLRU {
		t.Fatal("expected a dedicated LRU instance for the upstream's own path")
	}

	// A second upstream with the same settings shares that instance.
	lru4, err := m.resolveUpstreamSlotLRU(3, up, log, lruByConfig)
	if err != nil || lru4 != lru3 {
		t.Fatalf("dedup: got %v, %v; want the same instance", lru4, err)
	}

	// The same path with a different limit gets its own instance.
	up2 := Upstream{Dial: "d", SlotSavePath: sharedDir, SlotLRUMax: 3}
	lru5, err := m.resolveUpstreamSlotLRU(4, up2, log, lruByConfig)
	if err != nil || lru5 == lru3 {
		t.Fatalf("different max: got %v, %v; want a distinct instance", lru5, err)
	}
}
