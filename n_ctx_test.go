package reasoningeffort

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// modelsPatchBody is a /v1/models listing used to exercise n_ctx injection:
//   - "unloaded" lacks meta and has --ctx-size 131072 -> should gain n_ctx.
//   - "loaded" already has meta.n_ctx=8192 but --ctx-size 4096 -> unchanged.
//   - "nocs" lacks meta and has no --ctx-size -> untouched.
const modelsPatchBody = `{
  "object": "model.list",
  "data": [
    {"id":"unloaded","object":"model","owned_by":"llamacpp","created":1700000000,"status":{"value":"unloaded","args":["--model","/m/unloaded.gguf","--ctx-size","131072","--flash-attn","on"]}},
    {"id":"loaded","object":"model","owned_by":"llamacpp","created":1700000000,"status":{"value":"loaded","args":["--ctx-size","4096"]},"meta":{"n_ctx":8192,"n_vocab":151936,"n_params":1234,"ftype":"Q6_K"}},
    {"id":"nocs","object":"model","owned_by":"llamacpp","created":1700000000,"status":{"value":"unloaded","args":["--model","/m/nocs.gguf"]}}
  ]
}`

// runModelsResponse runs the middleware against a GET request to reqPath
// whose upstream returns upstreamBody with the given Content-Type. It
// returns the recorder so tests can inspect the patched response.
func runModelsResponse(t *testing.T, m ReasoningEffort, reqPath, upstreamBody, upstreamContentType string) *httptest.ResponseRecorder {
	t.Helper()
	m.ModelsPath = defaultModelsPath

	req := httptest.NewRequest(http.MethodGet, reqPath, nil)
	rec := httptest.NewRecorder()

	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		if upstreamContentType != "" {
			w.Header().Set("Content-Type", upstreamContentType)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstreamBody)
		return nil
	})

	if err := m.ServeHTTP(rec, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	return rec
}

// containsArg reports whether s contains the string want.
func containsArg(s []any, want string) bool {
	for _, v := range s {
		if str, ok := v.(string); ok && str == want {
			return true
		}
	}
	return false
}

// TestServeModelsInjectsNCtx verifies meta.n_ctx is synthesized for models
// missing it (from --ctx-size), existing n_ctx is preserved, and models
// without --ctx-size are left untouched.
func TestServeModelsInjectsNCtx(t *testing.T) {
	rec := runModelsResponse(t, ReasoningEffort{Map: newTestMap()}, defaultModelsPath, modelsPatchBody, "application/json; charset=utf-8")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("expected upstream Content-Type preserved, got %q", ct)
	}

	var resp struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("patched body not valid json: %v\n%s", err, rec.Body.String())
	}
	if resp.Object != "model.list" {
		t.Errorf("expected object=model.list preserved, got %q", resp.Object)
	}
	if len(resp.Data) != 3 {
		t.Fatalf("expected 3 models, got %d", len(resp.Data))
	}

	getNCtx := func(id string) (float64, bool) {
		for _, e := range resp.Data {
			if e["id"] != id {
				continue
			}
			meta, ok := e["meta"].(map[string]any)
			if !ok {
				return 0, false
			}
			v, ok := meta["n_ctx"].(float64)
			return v, ok
		}
		return 0, false
	}

	if v, ok := getNCtx("unloaded"); !ok || v != 131072 {
		t.Errorf("expected unloaded n_ctx=131072, got %v (present=%v)", v, ok)
	}
	if v, ok := getNCtx("loaded"); !ok || v != 8192 {
		t.Errorf("expected loaded n_ctx=8192 preserved (not overwritten by --ctx-size 4096), got %v (present=%v)", v, ok)
	}
	if _, ok := getNCtx("nocs"); ok {
		t.Errorf("expected nocs to have no meta.n_ctx")
	}

	// Pass-through fields must survive the re-serialization.
	entry := func(id string) map[string]any {
		for _, e := range resp.Data {
			if e["id"] == id {
				return e
			}
		}
		return nil
	}

	un := entry("unloaded")
	if un == nil || un["owned_by"] != "llamacpp" {
		t.Errorf("expected owned_by=llamacpp preserved for unloaded")
	}
	if un["status"] == nil {
		t.Errorf("expected status preserved for unloaded")
	}
	if st, ok := un["status"].(map[string]any); !ok || !containsArg(st["args"].([]any), "--ctx-size") {
		t.Errorf("expected status.args preserved for unloaded, got %v", un["status"])
	}
	if md, ok := entry("loaded")["meta"].(map[string]any); !ok || md["n_vocab"] != float64(151936) {
		t.Errorf("expected loaded meta.n_vocab=151936 preserved, got %v", md["n_vocab"])
	}
}

// TestServeModelsKeepsExistingNCtx verifies a model that already carries
// meta.n_ctx is not overwritten by its (different) --ctx-size argument.
func TestServeModelsKeepsExistingNCtx(t *testing.T) {
	body := `{"object":"model.list","data":[{"id":"m","object":"model","status":{"value":"loaded","args":["--ctx-size","4096"]},"meta":{"n_ctx":65536,"n_vocab":100}}]}`
	rec := runModelsResponse(t, ReasoningEffort{Map: newTestMap()}, defaultModelsPath, body, "application/json")

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not valid json: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 model, got %d", len(resp.Data))
	}
	meta, ok := resp.Data[0]["meta"].(map[string]any)
	if !ok {
		t.Fatalf("expected meta present, got %v", resp.Data[0]["meta"])
	}
	if meta["n_ctx"] != float64(65536) {
		t.Errorf("expected existing n_ctx=65536 preserved, got %v", meta["n_ctx"])
	}
}

// TestServeModelsNoCtxSizeUnchanged verifies a model lacking meta and
// --ctx-size is forwarded without a synthesized meta block.
func TestServeModelsNoCtxSizeUnchanged(t *testing.T) {
	body := `{"object":"model.list","data":[{"id":"m","object":"model","status":{"value":"unloaded","args":["--model","/m.gguf"]}}]}`
	rec := runModelsResponse(t, ReasoningEffort{Map: newTestMap()}, defaultModelsPath, body, "application/json")

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not valid json: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 model, got %d", len(resp.Data))
	}
	if _, ok := resp.Data[0]["meta"]; ok {
		t.Errorf("expected no meta to be added when --ctx-size is absent")
	}
}

// TestServeModelsInvalidJSONForwarded verifies a non-JSON response is
// forwarded byte-for-byte unchanged.
func TestServeModelsInvalidJSONForwarded(t *testing.T) {
	const body = `{not valid json`
	rec := runModelsResponse(t, ReasoningEffort{Map: newTestMap()}, defaultModelsPath, body, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if rec.Body.String() != body {
		t.Errorf("expected invalid JSON forwarded unchanged, got %q", rec.Body.String())
	}
}

// TestServeModelsNonJSONContentTypeForwarded verifies a non-JSON
// Content-Type response is forwarded unchanged without attempting a patch.
func TestServeModelsNonJSONContentTypeForwarded(t *testing.T) {
	const body = `<html>not json</html>`
	rec := runModelsResponse(t, ReasoningEffort{Map: newTestMap()}, defaultModelsPath, body, "text/html")
	if rec.Body.String() != body {
		t.Errorf("expected non-JSON response forwarded unchanged, got %q", rec.Body.String())
	}
}

// TestServeModelsEmptyDataForwarded verifies an empty data array is
// forwarded unchanged.
func TestServeModelsEmptyDataForwarded(t *testing.T) {
	const body = `{"object":"model.list","data":[]}`
	rec := runModelsResponse(t, ReasoningEffort{Map: newTestMap()}, defaultModelsPath, body, "application/json")
	if rec.Body.String() != body {
		t.Errorf("expected empty-data response forwarded unchanged, got %q", rec.Body.String())
	}
}

// TestServeModelsPathMismatchForwarded verifies a request to a different
// path is forwarded unchanged (no patching performed).
func TestServeModelsPathMismatchForwarded(t *testing.T) {
	rec := runModelsResponse(t, ReasoningEffort{Map: newTestMap()}, "/v1/other", modelsPatchBody, "application/json")
	if rec.Body.String() != modelsPatchBody {
		t.Errorf("expected upstream body forwarded unchanged on path mismatch, got %q", rec.Body.String())
	}
}

// TestServeModelsDefaultPath verifies Provision defaults ModelsPath to
// /v1/models when unset.
func TestServeModelsDefaultPath(t *testing.T) {
	m := ReasoningEffort{Map: newTestMap()}
	if err := m.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision error: %v", err)
	}
	if m.ModelsPath != defaultModelsPath {
		t.Errorf("expected default ModelsPath %q, got %q", defaultModelsPath, m.ModelsPath)
	}
}

// TestUnmarshalCaddyfileModelsPath verifies the models_path directive is
// parsed from the Caddyfile.
func TestUnmarshalCaddyfileModelsPath(t *testing.T) {
	input := `reasoning_effort {
		models_path /custom/models
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	if m.ModelsPath != "/custom/models" {
		t.Errorf("expected models_path /custom/models, got %q", m.ModelsPath)
	}
}

// TestCtxSizeFromArgs verifies the --ctx-size scanner across edge cases.
func TestCtxSizeFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int64
	}{
		{"present", []string{"--model", "m.gguf", "--ctx-size", "262144"}, 262144},
		{"absent", []string{"--model", "m.gguf"}, 0},
		{"no value", []string{"--ctx-size"}, 0},
		{"non-numeric", []string{"--ctx-size", "abc"}, 0},
		{"zero", []string{"--ctx-size", "0"}, 0},
		{"first wins", []string{"--ctx-size", "100", "x", "--ctx-size", "200"}, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ctxSizeFromArgs(tc.args); got != tc.want {
				t.Errorf("ctxSizeFromArgs(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}
