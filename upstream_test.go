package reasoningeffort

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json/v2"

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
