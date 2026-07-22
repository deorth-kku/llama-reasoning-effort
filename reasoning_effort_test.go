package reasoningeffort

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// captureHandler records the request body it receives for assertions.
type captureHandler struct {
	body    []byte
	path    string
	headers http.Header
}

func (c *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	c.body, _ = io.ReadAll(r.Body)
	c.path = r.URL.Path
	c.headers = r.Header.Clone()
	w.WriteHeader(http.StatusOK)
	return nil
}

func newTestMap() map[string]int64 {
	return map[string]int64{
		"minimal": 128,
		"low":     512,
		"medium":  2048,
		"high":    8192,
		"xhigh":   32768,
		"max":     -1,
	}
}

// runHandler builds a request with the given body and path, runs the
// middleware, and returns the captured downstream request.
func runHandler(t *testing.T, m ReasoningEffort, body string, path string) *captureHandler {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	rec := httptest.NewRecorder()

	cap := &captureHandler{}
	next := caddyhttp.HandlerFunc(cap.ServeHTTP)
	if err := m.ServeHTTP(rec, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	return cap
}

func TestMapHitWritesBudgetAndKeepsSource(t *testing.T) {
	m := ReasoningEffort{Path: defaultPath, Map: newTestMap()}
	cap := runHandler(t, m, `{"model":"x","reasoning_effort":"medium"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if got["thinking_budget_tokens"] != float64(2048) {
		t.Errorf("expected thinking_budget_tokens=2048, got %v", got["thinking_budget_tokens"])
	}
	if got["reasoning_effort"] != "medium" {
		t.Errorf("expected reasoning_effort preserved, got %v", got["reasoning_effort"])
	}
}

func TestUnknownValueSkips(t *testing.T) {
	m := ReasoningEffort{Path: defaultPath, Map: newTestMap()}
	cap := runHandler(t, m, `{"reasoning_effort":"foo"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if _, ok := got["thinking_budget_tokens"]; ok {
		t.Errorf("expected thinking_budget_tokens to be absent for unknown value, got %v", got["thinking_budget_tokens"])
	}
	if got["reasoning_effort"] != "foo" {
		t.Errorf("expected reasoning_effort preserved, got %v", got["reasoning_effort"])
	}
}

func TestNonStringValueSkips(t *testing.T) {
	m := ReasoningEffort{Path: defaultPath, Map: newTestMap()}
	cap := runHandler(t, m, `{"reasoning_effort":123}`, defaultPath)

	var got map[string]any
	_ = json.Unmarshal(cap.body, &got)
	if _, ok := got["thinking_budget_tokens"]; ok {
		t.Errorf("expected thinking_budget_tokens absent for non-string value, got %v", got["thinking_budget_tokens"])
	}
}

func TestInvalidJSONSkips(t *testing.T) {
	m := ReasoningEffort{Path: defaultPath, Map: newTestMap()}
	cap := runHandler(t, m, `{not valid json`, defaultPath)

	// Body should be forwarded unchanged.
	if string(cap.body) != `{not valid json` {
		t.Errorf("expected original body forwarded, got %q", string(cap.body))
	}
	if _, ok := cap.headers["Content-Length"]; ok {
		// Content-Length may be absent after invalid json path; just ensure no crash.
	}
}

func TestPathMismatchSkips(t *testing.T) {
	m := ReasoningEffort{Path: defaultPath, Map: newTestMap()}
	cap := runHandler(t, m, `{"reasoning_effort":"high"}`, "/other/path")

	var got map[string]any
	_ = json.Unmarshal(cap.body, &got)
	if _, ok := got["thinking_budget_tokens"]; ok {
		t.Errorf("expected no transformation on path mismatch, got %v", got["thinking_budget_tokens"])
	}
}

func TestNegativeBudgetValue(t *testing.T) {
	m := ReasoningEffort{Path: defaultPath, Map: newTestMap()}
	cap := runHandler(t, m, `{"reasoning_effort":"max"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if got["thinking_budget_tokens"] != float64(-1) {
		t.Errorf("expected thinking_budget_tokens=-1, got %v", got["thinking_budget_tokens"])
	}
}

func TestContentLengthUpdated(t *testing.T) {
	m := ReasoningEffort{Path: defaultPath, Map: newTestMap()}
	cap := runHandler(t, m, `{"reasoning_effort":"high"}`, defaultPath)

	want := strconv.Itoa(len(cap.body))
	if got := cap.headers.Get("Content-Length"); got != want {
		t.Errorf("expected Content-Length %q, got %q", want, got)
	}
}

func TestUnmarshalCaddyfile(t *testing.T) {
	input := `reasoning_effort {
		path /v1/chat/completions
		map minimal 128
		map low 512
		map medium 2048
		map high 8192
		map xhigh 32768
		map max -1
	}`

	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}

	if m.Path != "/v1/chat/completions" {
		t.Errorf("expected path /v1/chat/completions, got %q", m.Path)
	}
	want := map[string]int64{
		"minimal": 128,
		"low":     512,
		"medium":  2048,
		"high":    8192,
		"xhigh":   32768,
		"max":     -1,
	}
	for k, v := range want {
		if m.Map[k] != v {
			t.Errorf("map[%q] = %d, want %d", k, m.Map[k], v)
		}
	}
}

func TestUnmarshalCaddyfileInvalidValue(t *testing.T) {
	input := `reasoning_effort {
		map minimal notanumber
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected error for non-integer budget value, got nil")
	}
}

// TestEnableThinkingSetWhenBudgetZero verifies that when the mapped
// thinking_budget_tokens is 0, the middleware sets
// chat_template_kwargs.enable_thinking to false.
func TestEnableThinkingSetWhenBudgetZero(t *testing.T) {
	// Add a mapping where budget is 0 so EnableThinking gets set.
	m := ReasoningEffort{Path: defaultPath, Map: newTestMap()}
	m.Map["zero"] = 0

	cap := runHandler(t, m, `{"reasoning_effort":"zero"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}

	// With omitzero, thinking_budget_tokens=0 is omitted.
	if _, exists := got["thinking_budget_tokens"]; exists {
		t.Errorf("expected thinking_budget_tokens to be omitted (omitzero), got %v", got["thinking_budget_tokens"])
	}

	// Verify chat_template_kwargs.enable_thinking is false.
	kwargs, ok := got["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("expected chat_template_kwargs to be an object, got %v (type %T)", got["chat_template_kwargs"], got["chat_template_kwargs"])
	}
	et, ok := kwargs["enable_thinking"].(bool)
	if !ok {
		t.Fatalf("expected enable_thinking to be bool, got %v (type %T)", kwargs["enable_thinking"], kwargs["enable_thinking"])
	}
	if et != false {
		t.Errorf("expected enable_thinking=false, got %v", et)
	}
}

// TestEnableThinkingOmittedWhenBudgetNonZero verifies that when the
// mapped thinking_budget_tokens is non-zero, chat_template_kwargs is
// omitted (enable_thinking is null/omitted).
func TestEnableThinkingOmittedWhenBudgetNonZero(t *testing.T) {
	m := ReasoningEffort{Path: defaultPath, Map: newTestMap()}
	cap := runHandler(t, m, `{"reasoning_effort":"high"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}

	// With omitzero, empty chat_template_kwargs is omitted.
	if _, exists := got["chat_template_kwargs"]; exists {
		t.Errorf("expected chat_template_kwargs to be omitted when budget non-zero, got %v", got["chat_template_kwargs"])
	}

	// Verify thinking_budget_tokens is set.
	if got["thinking_budget_tokens"] != float64(8192) {
		t.Errorf("expected thinking_budget_tokens=8192, got %v", got["thinking_budget_tokens"])
	}
}

// TestExtraFieldsPassthrough verifies that fields not explicitly defined
// in RequestBody (e.g. model, messages, temperature) are preserved
// unchanged in the downstream request body.
func TestExtraFieldsPassthrough(t *testing.T) {
	m := ReasoningEffort{Path: defaultPath, Map: newTestMap()}
	body := `{"model":"llama-3","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"reasoning_effort":"low","extra_nested":{"a":{"b":1}}}`
	cap := runHandler(t, m, body, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}

	// Explicitly handled fields.
	if got["reasoning_effort"] != "low" {
		t.Errorf("expected reasoning_effort=low, got %v", got["reasoning_effort"])
	}
	if got["thinking_budget_tokens"] != float64(512) {
		t.Errorf("expected thinking_budget_tokens=512, got %v", got["thinking_budget_tokens"])
	}

	// Extra fields that should be passed through unchanged.
	if got["model"] != "llama-3" {
		t.Errorf("expected model=llama-3, got %v", got["model"])
	}
	if got["temperature"] != float64(0.7) {
		t.Errorf("expected temperature=0.7, got %v", got["temperature"])
	}

	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("expected messages to be a single-element array, got %v", got["messages"])
	}

	// Nested extra field.
	enc, ok := got["extra_nested"].(map[string]any)
	if !ok {
		t.Fatalf("expected extra_nested to be an object, got %v", got["extra_nested"])
	}
	ab, ok := enc["a"].(map[string]any)
	if !ok {
		t.Fatalf("expected extra_nested.a to be an object, got %v", enc["a"])
	}
	if ab["b"] != float64(1) {
		t.Errorf("expected extra_nested.a.b=1, got %v", ab["b"])
	}
}
