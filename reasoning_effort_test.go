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
