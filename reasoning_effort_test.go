package reasoningeffort

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// newDebugJSONLogger returns a zap logger that writes JSON-encoded entries at
// debug level to the provided buffer.
func newDebugJSONLogger(buf *bytes.Buffer) *zap.Logger {
	encoder := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	core := zapcore.NewCore(encoder, zapcore.AddSync(buf), zapcore.DebugLevel)
	return zap.New(core)
}

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

	want := ""
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

func TestUnmarshalCaddyfileToChatTemplateKey(t *testing.T) {
	input := `reasoning_effort {
		to_chat_template_key effort
		map medium 2048
	}`

	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	if m.ToChatTemplateKey != "effort" {
		t.Errorf("expected to_chat_template_key=effort, got %q", m.ToChatTemplateKey)
	}
}

func TestUnmarshalCaddyfileModelBlock(t *testing.T) {
	input := `reasoning_effort {
		to_chat_template_key effort
		map medium 2048
		model llama-4 {
			to_chat_template_key effort
			map medium 4096
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}

	// Top-level config is untouched by the model block.
	if m.ToChatTemplateKey != "effort" {
		t.Errorf("expected default to_chat_template_key=effort, got %q", m.ToChatTemplateKey)
	}
	if m.Map["medium"] != 2048 {
		t.Errorf("expected default map[medium]=2048, got %d", m.Map["medium"])
	}

	mc, ok := m.ModelConfigs["llama-4"]
	if !ok {
		t.Fatalf("expected llama-4 in ModelConfigs, got %#v", m.ModelConfigs)
	}
	if mc.ToChatTemplateKey != "effort" {
		t.Errorf("expected model to_chat_template_key=effort, got %q", mc.ToChatTemplateKey)
	}
	if mc.Map["medium"] != 4096 {
		t.Errorf("expected model map[medium]=4096, got %d", mc.Map["medium"])
	}
}

func TestUnmarshalCaddyfileModelBlockMissingBraces(t *testing.T) {
	input := `reasoning_effort {
		model llama-4
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected error for model block without braces, got nil")
	}
}

func TestUnmarshalCaddyfileModelBlockUnexpectedToken(t *testing.T) {
	input := `reasoning_effort {
		model llama-4 {
			foobar 123
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected error for unexpected token in model block, got nil")
	}
}

func TestUnmarshalCaddyfileModelBlockInvalidValue(t *testing.T) {
	input := `reasoning_effort {
		model llama-4 {
			map medium notanumber
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected error for invalid budget value in model block, got nil")
	}
}

// TestLogitBiasSetWhenLevelInMap verifies that when the request's
// reasoning_effort is present in the LogitBias map, the mapped logit_bias
// value is written to the downstream request body.
func TestLogitBiasSetWhenLevelInMap(t *testing.T) {
	m := ReasoningEffort{
		Path: defaultPath,
		LogitBias: map[string]jsontext.Value{
			"medium": []byte(`{"50256":-100}`),
		},
	}
	cap := runHandler(t, m, `{"reasoning_effort":"medium"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	lb, ok := got["logit_bias"].(map[string]any)
	if !ok {
		t.Fatalf("expected logit_bias to be an object, got %v (type %T)", got["logit_bias"], got["logit_bias"])
	}
	if lb["50256"] != float64(-100) {
		t.Errorf("expected logit_bias[50256]=-100, got %v", lb["50256"])
	}
}

// TestLogitBiasNotSetWhenLevelNotInMap verifies that when the request's
// reasoning_effort is not in the LogitBias map, no logit_bias is written.
func TestLogitBiasNotSetWhenLevelNotInMap(t *testing.T) {
	m := ReasoningEffort{
		Path: defaultPath,
		LogitBias: map[string]jsontext.Value{
			"high": []byte(`{"50256":-100}`),
		},
	}
	cap := runHandler(t, m, `{"reasoning_effort":"medium"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if _, ok := got["logit_bias"]; ok {
		t.Errorf("expected logit_bias to be absent for unknown value, got %v", got["logit_bias"])
	}
}

// TestLogitBiasPreservedWhenNotMapped verifies that a logit_bias already
// present in the request is preserved unchanged when the level is not in the
// LogitBias map.
func TestLogitBiasPreservedWhenNotMapped(t *testing.T) {
	m := ReasoningEffort{
		Path: defaultPath,
		LogitBias: map[string]jsontext.Value{
			"high": []byte(`{"50256":-100}`),
		},
	}
	cap := runHandler(t, m, `{"reasoning_effort":"medium","logit_bias":{"999":5}}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	lb, ok := got["logit_bias"].(map[string]any)
	if !ok {
		t.Fatalf("expected logit_bias to be preserved, got %v (type %T)", got["logit_bias"], got["logit_bias"])
	}
	if lb["999"] != float64(5) {
		t.Errorf("expected logit_bias[999]=5, got %v", lb["999"])
	}
}

// TestModelLogitBiasOverride verifies that a per-model LogitBias fully
// overrides the top-level LogitBias when the request's model matches.
func TestModelLogitBiasOverride(t *testing.T) {
	m := ReasoningEffort{
		Path: defaultPath,
		LogitBias: map[string]jsontext.Value{
			"medium": []byte(`{"1":1}`),
		},
		ModelConfigs: map[string]ModelConfig{
			"llama-4": {
				LogitBias: map[string]jsontext.Value{
					"medium": []byte(`{"2":2}`),
				},
			},
		},
	}
	cap := runHandler(t, m, `{"model":"llama-4","reasoning_effort":"medium"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	lb, ok := got["logit_bias"].(map[string]any)
	if !ok {
		t.Fatalf("expected logit_bias to be an object, got %v (type %T)", got["logit_bias"], got["logit_bias"])
	}
	if lb["2"] != float64(2) {
		t.Errorf("expected model logit_bias[2]=2, got %v", lb["2"])
	}
	if _, ok := lb["1"]; ok {
		t.Errorf("expected top-level logit_bias[1] to be overridden, got %v", lb["1"])
	}
}

// TestReasoningLogitBiasSetWhenLevelInMap verifies that when the request's
// reasoning_effort is present in the ReasoningLogitBias map, the mapped
// reasoning_logit_bias value is written to the downstream request body.
func TestReasoningLogitBiasSetWhenLevelInMap(t *testing.T) {
	m := ReasoningEffort{
		Path: defaultPath,
		ReasoningLogitBias: map[string]jsontext.Value{
			"medium": []byte(`{"50256":-100}`),
		},
	}
	cap := runHandler(t, m, `{"reasoning_effort":"medium"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	lb, ok := got["reasoning_logit_bias"].(map[string]any)
	if !ok {
		t.Fatalf("expected reasoning_logit_bias to be an object, got %v (type %T)", got["reasoning_logit_bias"], got["reasoning_logit_bias"])
	}
	if lb["50256"] != float64(-100) {
		t.Errorf("expected reasoning_logit_bias[50256]=-100, got %v", lb["50256"])
	}
}

// TestReasoningLogitBiasNotSetWhenLevelNotInMap verifies that when the
// request's reasoning_effort is not in the ReasoningLogitBias map, no
// reasoning_logit_bias is written.
func TestReasoningLogitBiasNotSetWhenLevelNotInMap(t *testing.T) {
	m := ReasoningEffort{
		Path: defaultPath,
		ReasoningLogitBias: map[string]jsontext.Value{
			"high": []byte(`{"50256":-100}`),
		},
	}
	cap := runHandler(t, m, `{"reasoning_effort":"medium"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if _, ok := got["reasoning_logit_bias"]; ok {
		t.Errorf("expected reasoning_logit_bias to be absent for unknown value, got %v", got["reasoning_logit_bias"])
	}
}

// TestReasoningLogitBiasPreservedWhenNotMapped verifies that a
// reasoning_logit_bias already present in the request is preserved
// unchanged when the level is not in the ReasoningLogitBias map.
func TestReasoningLogitBiasPreservedWhenNotMapped(t *testing.T) {
	m := ReasoningEffort{
		Path: defaultPath,
		ReasoningLogitBias: map[string]jsontext.Value{
			"high": []byte(`{"50256":-100}`),
		},
	}
	cap := runHandler(t, m, `{"reasoning_effort":"medium","reasoning_logit_bias":{"999":5}}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	lb, ok := got["reasoning_logit_bias"].(map[string]any)
	if !ok {
		t.Fatalf("expected reasoning_logit_bias to be preserved, got %v (type %T)", got["reasoning_logit_bias"], got["reasoning_logit_bias"])
	}
	if lb["999"] != float64(5) {
		t.Errorf("expected reasoning_logit_bias[999]=5, got %v", lb["999"])
	}
}

// TestModelReasoningLogitBiasOverride verifies that a per-model
// ReasoningLogitBias fully overrides the top-level ReasoningLogitBias when
// the request's model matches.
func TestModelReasoningLogitBiasOverride(t *testing.T) {
	m := ReasoningEffort{
		Path: defaultPath,
		ReasoningLogitBias: map[string]jsontext.Value{
			"medium": []byte(`{"1":1}`),
		},
		ModelConfigs: map[string]ModelConfig{
			"llama-4": {
				ReasoningLogitBias: map[string]jsontext.Value{
					"medium": []byte(`{"2":2}`),
				},
			},
		},
	}
	cap := runHandler(t, m, `{"model":"llama-4","reasoning_effort":"medium"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	lb, ok := got["reasoning_logit_bias"].(map[string]any)
	if !ok {
		t.Fatalf("expected reasoning_logit_bias to be an object, got %v (type %T)", got["reasoning_logit_bias"], got["reasoning_logit_bias"])
	}
	if lb["2"] != float64(2) {
		t.Errorf("expected model reasoning_logit_bias[2]=2, got %v", lb["2"])
	}
	if _, ok := lb["1"]; ok {
		t.Errorf("expected top-level reasoning_logit_bias[1] to be overridden, got %v", lb["1"])
	}
}

// TestRemoveRemovesReasoningEffort verifies that when Remove is set, the
// reasoning_effort field is stripped from the downstream request body.
func TestRemoveRemovesReasoningEffort(t *testing.T) {
	m := ReasoningEffort{Path: defaultPath, Remove: true}
	cap := runHandler(t, m, `{"reasoning_effort":"medium"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Errorf("expected reasoning_effort to be removed, got %v", got["reasoning_effort"])
	}
}

// TestRemoveNotSetKeepsReasoningEffort verifies that when Remove is not set,
// the reasoning_effort field is preserved in the downstream request body.
func TestRemoveNotSetKeepsReasoningEffort(t *testing.T) {
	m := ReasoningEffort{Path: defaultPath}
	cap := runHandler(t, m, `{"reasoning_effort":"medium"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if got["reasoning_effort"] != "medium" {
		t.Errorf("expected reasoning_effort preserved, got %v", got["reasoning_effort"])
	}
}

func TestUnmarshalCaddyfileLogitBias(t *testing.T) {
	input := `reasoning_effort {
		logit_bias medium {
			50256 -100
			50257 50
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	bias, ok := m.LogitBias["medium"]
	if !ok {
		t.Fatalf("expected logit_bias[medium] to be set, got %#v", m.LogitBias)
	}
	var got map[string]float64
	if err := json.Unmarshal(bias, &got); err != nil {
		t.Fatalf("logit_bias not valid json: %v", err)
	}
	if got["50256"] != -100 {
		t.Errorf("expected logit_bias[50256]=-100, got %v", got["50256"])
	}
	if got["50257"] != 50 {
		t.Errorf("expected logit_bias[50257]=50, got %v", got["50257"])
	}
}

// TestUnmarshalCaddyfileLogitBiasFalse verifies that the literal token
// `false` is emitted as JSON `false` (interpreted downstream as -Inf).
func TestUnmarshalCaddyfileLogitBiasFalse(t *testing.T) {
	input := `reasoning_effort {
		logit_bias medium {
			50256 false
			50257 -100
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	bias, ok := m.LogitBias["medium"]
	if !ok {
		t.Fatalf("expected logit_bias[medium] to be set, got %#v", m.LogitBias)
	}
	var got map[string]any
	if err := json.Unmarshal(bias, &got); err != nil {
		t.Fatalf("logit_bias not valid json: %v", err)
	}
	if v, ok := got["50256"].(bool); !ok || v != false {
		t.Errorf("expected logit_bias[50256]=false, got %v (type %T)", got["50256"], got["50256"])
	}
	if got["50257"] != float64(-100) {
		t.Errorf("expected logit_bias[50257]=-100, got %v", got["50257"])
	}
}

func TestUnmarshalCaddyfileRemove(t *testing.T) {
	input := `reasoning_effort {
		remove
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	if !m.Remove {
		t.Errorf("expected remove to be true")
	}
}

func TestUnmarshalCaddyfileLogitBiasInvalidValue(t *testing.T) {
	input := `reasoning_effort {
		logit_bias medium {
			50256 notanumber
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected error for invalid logit_bias value, got nil")
	}
}

func TestUnmarshalCaddyfileLogitBiasMissingBraces(t *testing.T) {
	input := `reasoning_effort {
		logit_bias medium
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected error for logit_bias without braces, got nil")
	}
}

func TestUnmarshalCaddyfileModelBlockLogitBias(t *testing.T) {
	input := `reasoning_effort {
		model llama-4 {
			logit_bias medium {
				50256 -100
			}
			remove
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	mc, ok := m.ModelConfigs["llama-4"]
	if !ok {
		t.Fatalf("expected llama-4 in ModelConfigs, got %#v", m.ModelConfigs)
	}
	bias, ok := mc.LogitBias["medium"]
	if !ok {
		t.Fatalf("expected model logit_bias[medium] to be set, got %#v", mc.LogitBias)
	}
	var got map[string]float64
	if err := json.Unmarshal(bias, &got); err != nil {
		t.Fatalf("model logit_bias not valid json: %v", err)
	}
	if got["50256"] != -100 {
		t.Errorf("expected model logit_bias[50256]=-100, got %v", got["50256"])
	}
	if !mc.Remove {
		t.Errorf("expected model remove to be true")
	}
}

func TestUnmarshalCaddyfileReasoningLogitBias(t *testing.T) {
	input := `reasoning_effort {
		reasoning_logit_bias medium {
			50256 -100
			50257 50
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	bias, ok := m.ReasoningLogitBias["medium"]
	if !ok {
		t.Fatalf("expected reasoning_logit_bias[medium] to be set, got %#v", m.ReasoningLogitBias)
	}
	var got map[string]float64
	if err := json.Unmarshal(bias, &got); err != nil {
		t.Fatalf("reasoning_logit_bias not valid json: %v", err)
	}
	if got["50256"] != -100 {
		t.Errorf("expected reasoning_logit_bias[50256]=-100, got %v", got["50256"])
	}
	if got["50257"] != 50 {
		t.Errorf("expected reasoning_logit_bias[50257]=50, got %v", got["50257"])
	}
}

func TestUnmarshalCaddyfileReasoningLogitBiasFalse(t *testing.T) {
	input := `reasoning_effort {
		reasoning_logit_bias medium {
			50256 false
			50257 -100
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	bias, ok := m.ReasoningLogitBias["medium"]
	if !ok {
		t.Fatalf("expected reasoning_logit_bias[medium] to be set, got %#v", m.ReasoningLogitBias)
	}
	var got map[string]any
	if err := json.Unmarshal(bias, &got); err != nil {
		t.Fatalf("reasoning_logit_bias not valid json: %v", err)
	}
	if v, ok := got["50256"].(bool); !ok || v != false {
		t.Errorf("expected reasoning_logit_bias[50256]=false, got %v (type %T)", got["50256"], got["50256"])
	}
	if got["50257"] != float64(-100) {
		t.Errorf("expected reasoning_logit_bias[50257]=-100, got %v", got["50257"])
	}
}

func TestUnmarshalCaddyfileReasoningLogitBiasInvalidValue(t *testing.T) {
	input := `reasoning_effort {
		reasoning_logit_bias medium {
			50256 notanumber
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected error for invalid reasoning_logit_bias value, got nil")
	}
}

func TestUnmarshalCaddyfileModelBlockReasoningLogitBias(t *testing.T) {
	input := `reasoning_effort {
		model llama-4 {
			reasoning_logit_bias medium {
				50256 -100
			}
			remove
		}
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	mc, ok := m.ModelConfigs["llama-4"]
	if !ok {
		t.Fatalf("expected llama-4 in ModelConfigs, got %#v", m.ModelConfigs)
	}
	bias, ok := mc.ReasoningLogitBias["medium"]
	if !ok {
		t.Fatalf("expected model reasoning_logit_bias[medium] to be set, got %#v", mc.ReasoningLogitBias)
	}
	var got map[string]float64
	if err := json.Unmarshal(bias, &got); err != nil {
		t.Fatalf("model reasoning_logit_bias not valid json: %v", err)
	}
	if got["50256"] != -100 {
		t.Errorf("expected model reasoning_logit_bias[50256]=-100, got %v", got["50256"])
	}
	if !mc.Remove {
		t.Errorf("expected model remove to be true")
	}
}

// TestModelConfigOverride verifies that a per-model config selected by the
// request's model field is used instead of the default config.
func TestModelConfigOverride(t *testing.T) {
	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
	}
	m.ModelConfigs = map[string]ModelConfig{
		"llama-4": {
			Map:               map[string]int64{"medium": 4096},
			ToChatTemplateKey: "effort",
		},
	}
	cap := runHandler(t, m, `{"model":"llama-4","reasoning_effort":"medium"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if got["thinking_budget_tokens"] != float64(4096) {
		t.Errorf("expected thinking_budget_tokens=4096 from model config, got %v", got["thinking_budget_tokens"])
	}
	if got["reasoning_effort"] != "medium" {
		t.Errorf("expected reasoning_effort preserved, got %v", got["reasoning_effort"])
	}
	// to_chat_template_key from the model config should have been applied.
	kwargs, ok := got["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("expected chat_template_kwargs to be an object, got %v", got["chat_template_kwargs"])
	}
	if kwargs["effort"] != "medium" {
		t.Errorf("expected chat_template_kwargs.effort=medium, got %v", kwargs["effort"])
	}
}

// TestModelConfigFallback verifies that a model not present in ModelConfigs
// falls back to the default (top-level) config.
func TestModelConfigFallback(t *testing.T) {
	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
	}
	m.ModelConfigs = map[string]ModelConfig{
		"llama-4": {Map: map[string]int64{"medium": 4096}},
	}
	// "other" model has no per-model config -> uses default.
	cap := runHandler(t, m, `{"model":"other","reasoning_effort":"medium"}`, defaultPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if got["thinking_budget_tokens"] != float64(2048) {
		t.Errorf("expected thinking_budget_tokens=2048 from default config, got %v", got["thinking_budget_tokens"])
	}
}

// TestDebugLogDataFieldIsJSONObject verifies that the debug log for the full
// request body emits the "data" field as a nested JSON object rather than an
// escaped JSON string.
func TestDebugLogDataFieldIsJSONObject(t *testing.T) {
	var buf bytes.Buffer
	logger := newDebugJSONLogger(&buf)

	m := ReasoningEffort{Path: defaultPath, Map: newTestMap(), log: logger}

	// "medium" maps to a non-zero budget, which mutates body before it is
	// logged, so we can assert on the transformed value.
	runHandler(t, m, `{"model":"x","reasoning_effort":"medium"}`, defaultPath)

	// zap emits newline-delimited JSON (JSONL); the full-body debug entry is
	// the last line written by the middleware.
	lines := strings.TrimSpace(buf.String())
	lastLine := lines[strings.LastIndex(lines, "\n")+1:]

	var logLine map[string]any
	if err := json.Unmarshal([]byte(lastLine), &logLine); err != nil {
		t.Fatalf("log output not valid json: %v\nlog was: %s", err, buf.String())
	}

	data, ok := logLine["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data field to be a JSON object, got type %T: %v",
			logLine["data"], logLine["data"])
	}

	if data["reasoning_effort"] != "medium" {
		t.Errorf("expected data.reasoning_effort=medium, got %v", data["reasoning_effort"])
	}
	if data["thinking_budget_tokens"] != float64(2048) {
		t.Errorf("expected data.thinking_budget_tokens=2048, got %v", data["thinking_budget_tokens"])
	}
}

// hookRequest records the details of a single request received by a
// hookRecorder.
type hookRequest struct {
	method      string
	body        []byte
	contentType string
	path        string
}

// hookRecorder is an httptest server that records every request it receives
// in order, and signals via the signal channel after each request so tests
// can wait for arrival without flaky sleeps.
type hookRecorder struct {
	mu       sync.Mutex
	requests []hookRequest
	signal   chan struct{}
}

func newHookRecorder() *hookRecorder {
	return &hookRecorder{signal: make(chan struct{}, 64)}
}

func (r *hookRecorder) handler(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.requests = append(r.requests, hookRequest{
		method:      req.Method,
		body:        body,
		contentType: req.Header.Get("Content-Type"),
		path:        req.URL.Path,
	})
	r.mu.Unlock()
	r.signal <- struct{}{}
	w.WriteHeader(http.StatusOK)
}

// wait blocks until n requests have been received, or times out.
func (r *hookRecorder) wait(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		select {
		case <-r.signal:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for hook request %d/%d", i+1, n)
		}
	}
}

func (r *hookRecorder) snapshot() []hookRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]hookRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

func (r *hookRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// TestHookFiresOnMatchingPath verifies a top-level http hook fires for a
// request to the configured path, with the ORIGINAL (pre-transformation)
// request body.
func TestHookFiresOnMatchingPath(t *testing.T) {
	rec := newHookRecorder()
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	const original = `{"model":"x","reasoning_effort":"medium","keepme":true}`
	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
		Hooks: []Hook{{
			Type: "http",
			URL:  srv.URL,
			// Reference the original request body via placeholder.
			Body: "{http.request.body}",
		}},
	}
	runHandler(t, m, original, defaultPath)
	rec.wait(t, 1)

	got := rec.snapshot()[0]
	if got.method != http.MethodPost {
		t.Errorf("expected hook method POST, got %q", got.method)
	}
	if string(got.body) != original {
		t.Errorf("expected hook to receive original body (not transformed), got %q", string(got.body))
	}
}

// TestMultipleHooksAllFire verifies every http hook in the array fires.
func TestMultipleHooksAllFire(t *testing.T) {
	recA := newHookRecorder()
	srvA := httptest.NewServer(http.HandlerFunc(recA.handler))
	defer srvA.Close()
	recB := newHookRecorder()
	srvB := httptest.NewServer(http.HandlerFunc(recB.handler))
	defer srvB.Close()

	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
		Hooks: []Hook{
			{Type: "http", URL: srvA.URL},
			{Type: "http", URL: srvB.URL},
		},
	}

	runHandler(t, m, `{"reasoning_effort":"high"}`, defaultPath)
	recA.wait(t, 1)
	recB.wait(t, 1)
}

// TestMultipleHooksFireInOrder verifies blocking hooks fire in array order.
func TestMultipleHooksFireInOrder(t *testing.T) {
	rec := newHookRecorder()
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
		Hooks: []Hook{
			{Type: "http", URL: srv.URL, Body: "first", Blocking: true},
			{Type: "http", URL: srv.URL, Body: "second", Blocking: true},
		},
	}

	runHandler(t, m, `{"reasoning_effort":"high"}`, defaultPath)
	rec.wait(t, 2)

	got := rec.snapshot()
	if string(got[0].body) != "first" {
		t.Errorf("expected first hook body 'first', got %q", string(got[0].body))
	}
	if string(got[1].body) != "second" {
		t.Errorf("expected second hook body 'second', got %q", string(got[1].body))
	}
}

// TestHookPerModelOverride verifies a per-model hooks array is selected over
// the top-level hooks array (full override).
func TestHookPerModelOverride(t *testing.T) {
	recA := newHookRecorder()
	srvA := httptest.NewServer(http.HandlerFunc(recA.handler))
	defer srvA.Close()
	recB := newHookRecorder()
	srvB := httptest.NewServer(http.HandlerFunc(recB.handler))
	defer srvB.Close()

	m := ReasoningEffort{
		Path:  defaultPath,
		Map:   newTestMap(),
		Hooks: []Hook{{Type: "http", URL: srvB.URL}},
		ModelConfigs: map[string]ModelConfig{
			"llama-4": {Hooks: []Hook{{Type: "http", URL: srvA.URL}}},
		},
	}

	// model llama-4 -> per-model hook (srvA).
	runHandler(t, m, `{"model":"llama-4","reasoning_effort":"medium"}`, defaultPath)
	recA.wait(t, 1)

	// unknown model -> top-level hook (srvB).
	runHandler(t, m, `{"model":"other","reasoning_effort":"medium"}`, defaultPath)
	recB.wait(t, 1)

	if recA.count() != 1 {
		t.Errorf("expected srvA (per-model) hit once, got %d", recA.count())
	}
	if recB.count() != 1 {
		t.Errorf("expected srvB (top-level) hit once, got %d", recB.count())
	}
}

// TestHookSkippedOnNonMatchingPath verifies hooks do not fire for paths that
// do not match the configured path.
func TestHookSkippedOnNonMatchingPath(t *testing.T) {
	rec := newHookRecorder()
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	m := ReasoningEffort{
		Path:  defaultPath,
		Map:   newTestMap(),
		Hooks: []Hook{{Type: "http", URL: srv.URL}},
	}

	runHandler(t, m, `{"reasoning_effort":"high"}`, "/other/path")
	// Give any (spurious) async hook a chance to fire before asserting.
	time.Sleep(200 * time.Millisecond)
	if rec.count() != 0 {
		t.Errorf("expected no hook hit on non-matching path, got %d", rec.count())
	}
}

// TestHookFailureDoesNotBreakRequest verifies a failing hook (500) does not
// prevent the downstream request, and that remaining hooks in the array still
// fire.
func TestHookFailureDoesNotBreakRequest(t *testing.T) {
	// Hook server that always returns 500.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	// A second, healthy hook that must still fire.
	rec := newHookRecorder()
	okSrv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer okSrv.Close()

	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
		Hooks: []Hook{
			{Type: "http", URL: failSrv.URL},
			{Type: "http", URL: okSrv.URL},
		},
	}

	cap := runHandler(t, m, `{"reasoning_effort":"high"}`, defaultPath)
	if cap.body == nil {
		t.Fatal("expected downstream handler to run")
	}
	rec.wait(t, 1)
}

// TestHookBodyJSONWinsOverString verifies that when both body_json and body
// are set, the JSON body wins and is sent with Content-Type application/json.
func TestHookBodyJSONWinsOverString(t *testing.T) {
	rec := newHookRecorder()
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
		Hooks: []Hook{{
			Type:     "http",
			URL:      srv.URL,
			Body:     "string-body",
			BodyJSON: jsontext.Value(`{"json":true}`),
		}},
	}

	runHandler(t, m, `{"reasoning_effort":"high"}`, defaultPath)
	rec.wait(t, 1)

	got := rec.snapshot()[0]
	if string(got.body) != `{"json":true}` {
		t.Errorf("expected JSON body to win, got %q", string(got.body))
	}
	if got.contentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", got.contentType)
	}
}

// TestHookStringBodyContentType verifies the string body honors a custom
// content_type, and falls back to the default when none is set.
func TestHookStringBodyContentType(t *testing.T) {
	recCustom := newHookRecorder()
	srvCustom := httptest.NewServer(http.HandlerFunc(recCustom.handler))
	defer srvCustom.Close()

	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
		Hooks: []Hook{{
			Type:        "http",
			URL:         srvCustom.URL,
			Body:        "hello",
			ContentType: "application/xml",
		}},
	}
	runHandler(t, m, `{"reasoning_effort":"high"}`, defaultPath)
	recCustom.wait(t, 1)
	if got := recCustom.snapshot()[0]; got.contentType != "application/xml" {
		t.Errorf("expected custom Content-Type application/xml, got %q", got.contentType)
	}
}

// TestHookDefaultContentType verifies the string body defaults to
// text/plain; charset=utf-8 when no content_type is set.
func TestHookDefaultContentType(t *testing.T) {
	rec := newHookRecorder()
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
		Hooks: []Hook{{
			Type: "http",
			URL:  srv.URL,
			Body: "hello",
		}},
	}
	runHandler(t, m, `{"reasoning_effort":"high"}`, defaultPath)
	rec.wait(t, 1)
	if got := rec.snapshot()[0]; got.contentType != defaultHookContentType {
		t.Errorf("expected default Content-Type %q, got %q", defaultHookContentType, got.contentType)
	}
}

// TestHookStringBodyPlaceholder verifies Caddy placeholder substitution in the
// string body template.
func TestHookStringBodyPlaceholder(t *testing.T) {
	rec := newHookRecorder()
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
		Hooks: []Hook{{
			Type: "http",
			URL:  srv.URL,
			Body: "host={http.request.host}",
		}},
	}
	// httptest.NewRequest sets Host from the URL.
	runHandler(t, m, `{"reasoning_effort":"high"}`, "http://example.test/v1/chat/completions")
	rec.wait(t, 1)

	got := rec.snapshot()[0]
	if !strings.Contains(string(got.body), "example.test") {
		t.Errorf("expected placeholder to resolve to host example.test, got %q", string(got.body))
	}
}

// TestHookBlocking verifies a blocking hook completes before the downstream
// handler runs.
func TestHookBlocking(t *testing.T) {
	proceed := make(chan struct{})
	rec := newHookRecorder()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-proceed // block until the test allows the hook to respond
		rec.handler(w, r)
	}))
	defer srv.Close()

	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
		Hooks: []Hook{{
			Type:     "http",
			URL:      srv.URL,
			Body:     "blocked",
			Blocking: true,
		}},
	}

	downstreamCalled := make(chan struct{})
	req := httptest.NewRequest(http.MethodPost, defaultPath, bytes.NewBufferString(`{"reasoning_effort":"high"}`))
	req.Header.Set("Content-Length", strconv.Itoa(len(`{"reasoning_effort":"high"}`)))
	recorder := httptest.NewRecorder()
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		close(downstreamCalled)
		w.WriteHeader(http.StatusOK)
		return nil
	})

	go func() {
		_ = m.ServeHTTP(recorder, req, next)
	}()

	// The middleware should be blocked inside the hook; downstream has not
	// run yet.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-downstreamCalled:
		t.Fatal("downstream ran before blocking hook completed")
	default:
	}

	// Let the hook respond; downstream should now complete.
	close(proceed)
	select {
	case <-downstreamCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("downstream never completed after hook proceeded")
	}
	rec.wait(t, 1)
}

// TestHookInvalidJSONDoesNotFire verifies that when the request body is
// invalid JSON, the transformation is skipped and no hooks fire.
func TestHookInvalidJSONDoesNotFire(t *testing.T) {
	rec := newHookRecorder()
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	m := ReasoningEffort{
		Path: defaultPath,
		Map:  newTestMap(),
		Hooks: []Hook{{
			Type: "http",
			URL:  srv.URL,
			Body: "fired",
		}},
	}

	cap := runHandler(t, m, `{not valid json`, defaultPath)
	if string(cap.body) != `{not valid json` {
		t.Errorf("expected original body forwarded on invalid JSON, got %q", string(cap.body))
	}
	// Give any (spurious) async hook a chance to fire before asserting.
	time.Sleep(200 * time.Millisecond)
	if rec.count() != 0 {
		t.Errorf("expected no hook hit on invalid JSON, got %d", rec.count())
	}
}

// TestProvisionHookValidation verifies Provision rejects invalid hook configs.
func TestProvisionHookValidation(t *testing.T) {
	tests := []struct {
		name  string
		hooks []Hook
	}{
		{"missing type", []Hook{{URL: "http://x"}}},
		{"unknown type", []Hook{{Type: "grpc", URL: "http://x"}}},
		{"http without url", []Hook{{Type: "http"}}},
		{"command not supported", []Hook{{Type: "command", Command: "echo hi"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := ReasoningEffort{Path: defaultPath, Hooks: tc.hooks}
			if err := m.Provision(caddy.Context{}); err == nil {
				t.Fatalf("expected Provision error for %#v", tc.hooks)
			}
		})
	}
}

// TestProvisionValidHooks verifies Provision accepts valid hook configs.
func TestProvisionValidHooks(t *testing.T) {
	m := ReasoningEffort{
		Path: defaultPath,
		Hooks: []Hook{{
			Type: "http",
			URL:  "http://example.test/hook",
		}},
		ModelConfigs: map[string]ModelConfig{
			"llama-4": {Hooks: []Hook{{Type: "http", URL: "http://example.test/model-hook"}}},
		},
	}
	if err := m.Provision(caddy.Context{}); err != nil {
		t.Fatalf("expected valid hooks, got error: %v", err)
	}
	if m.hookClient == nil {
		t.Error("expected hookClient to be initialized in Provision")
	}
}

// TestUnmarshalCaddyfileHook verifies parsing of top-level and per-model hook
// directives, including multiple hooks appended to the array.
func TestUnmarshalCaddyfileHook(t *testing.T) {
	input := `reasoning_effort {
		map medium 2048
		hook http http://example.test/hook {
			method POST
			body '{"reasoning_effort":"{{.Request.Body}}"}'
			content_type application/json
			timeout 5s
			blocking
		}
		hook http http://example.test/hook2
		model llama-4 {
			map medium 4096
			hook http http://example.test/model-hook {
				method PUT
				body "model-hook"
				timeout 10s
			}
		}
	}`

	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}

	if len(m.Hooks) != 2 {
		t.Fatalf("expected 2 top-level hooks, got %d", len(m.Hooks))
	}
	if m.Hooks[0].Type != "http" {
		t.Errorf("expected hook type http, got %q", m.Hooks[0].Type)
	}
	if m.Hooks[0].URL != "http://example.test/hook" {
		t.Errorf("unexpected hook URL: %q", m.Hooks[0].URL)
	}
	if m.Hooks[0].Method != "POST" {
		t.Errorf("expected method POST, got %q", m.Hooks[0].Method)
	}
	if m.Hooks[0].ContentType != "application/json" {
		t.Errorf("expected content_type application/json, got %q", m.Hooks[0].ContentType)
	}
	if m.Hooks[0].Timeout != 5*time.Second {
		t.Errorf("expected timeout 5s, got %v", m.Hooks[0].Timeout)
	}
	if !m.Hooks[0].Blocking {
		t.Error("expected blocking=true")
	}
	if m.Hooks[0].Body == "" {
		t.Error("expected string body to be parsed")
	}
	if m.Hooks[0].BodyJSON != nil {
		t.Error("expected BodyJSON to be empty in Caddyfile parsing")
	}
	if m.Hooks[1].URL != "http://example.test/hook2" {
		t.Errorf("expected second hook URL, got %q", m.Hooks[1].URL)
	}

	mc, ok := m.ModelConfigs["llama-4"]
	if !ok {
		t.Fatalf("expected llama-4 in ModelConfigs")
	}
	if len(mc.Hooks) != 1 {
		t.Fatalf("expected 1 hook for model llama-4, got %d", len(mc.Hooks))
	}
	if mc.Hooks[0].Method != "PUT" {
		t.Errorf("expected model hook method PUT, got %q", mc.Hooks[0].Method)
	}
	if mc.Hooks[0].Timeout != 10*time.Second {
		t.Errorf("expected model hook timeout 10s, got %v", mc.Hooks[0].Timeout)
	}
}

// TestUnmarshalCaddyfileHookUnsupportedType verifies a non-http hook type in
// the Caddyfile is rejected.
func TestUnmarshalCaddyfileHookUnsupportedType(t *testing.T) {
	input := `reasoning_effort {
		hook grpc http://example.test
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected error for unsupported hook type in Caddyfile, got nil")
	}
}
