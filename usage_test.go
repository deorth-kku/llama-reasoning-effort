package reasoningeffort

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// sseWithUsage is a synthetic chat completions SSE stream whose final chunk
// carries a usage object, followed by the terminal [DONE] event.
const sseWithUsage = `data: {"id":"chatcmpl-test000","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hel"}}]}

data: {"id":"chatcmpl-test000","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}

data: {"id":"chatcmpl-test000","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19},"timings":{"predicted_n":7,"predicted_per_second":35.0,"prompt_n":12,"prompt_per_second":60.0}}

data: [DONE]

`

// sseWithoutUsage is a synthetic SSE stream with no usage object.
const sseWithoutUsage = `data: {"id":"chatcmpl-test000","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hi"}}]}

data: [DONE]

`

// TestSSEUsageParser verifies the incremental parser recovers usage whether
// the stream arrives in one chunk or is split across many, and yields nil when
// no usage is present.
func TestSSEUsageParser(t *testing.T) {
	t.Run("single chunk", func(t *testing.T) {
		p := &sseUsageParser{}
		u := p.feed([]byte(sseWithUsage))
		if u == nil {
			t.Fatal("expected a usage object, got nil")
		}
		if u["prompt_tokens"] != float64(12) || u["completion_tokens"] != float64(7) || u["total_tokens"] != float64(19) {
			t.Errorf("unexpected usage values: %+v", u)
		}
		if p.timings == nil || p.timings["predicted_n"] != float64(7) || p.timings["prompt_n"] != float64(12) {
			t.Errorf("unexpected timings values: %+v", p.timings)
		}
	})

	t.Run("split across chunks", func(t *testing.T) {
		// Feed one byte at a time so every event is split across many writes.
		p := &sseUsageParser{}
		stream := []byte(sseWithUsage)
		var u map[string]any
		for i := 0; i < len(stream); i++ {
			u = p.feed(stream[i : i+1])
		}
		if u == nil {
			t.Fatal("expected a usage object, got nil")
		}
		if u["total_tokens"] != float64(19) {
			t.Errorf("expected total_tokens=19, got %+v", u)
		}
	})

	t.Run("without usage", func(t *testing.T) {
		p := &sseUsageParser{}
		if u := p.feed([]byte(sseWithoutUsage)); u != nil {
			t.Errorf("expected nil usage, got %+v", u)
		}
		if u := p.finalize(); u != nil {
			t.Errorf("expected nil usage after finalize, got %+v", u)
		}
	})

	t.Run("trailing event without blank-line terminator", func(t *testing.T) {
		// The final usage event is not followed by a blank line; finalize must
		// still process it.
		stream := "data: {\"choices\":[]}\n\ndata: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}"
		p := &sseUsageParser{}
		p.feed([]byte(stream))
		if u := p.finalize(); u == nil || u["total_tokens"] != float64(3) {
			t.Errorf("expected usage total_tokens=3 from trailing event, got %+v", u)
		}
	})

	t.Run("non-json data lines ignored", func(t *testing.T) {
		stream := "data: not-json\n\ndata: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n"
		p := &sseUsageParser{}
		if u := p.feed([]byte(stream)); u == nil || u["total_tokens"] != float64(3) {
			t.Errorf("expected usage total_tokens=3, got %+v", u)
		}
	})
}

// TestServeChatCompletionsPassthrough verifies the SSE stream reaches the
// client unchanged and the usage is logged once the stream completes.
func TestServeChatCompletionsPassthrough(t *testing.T) {
	var logBuf bytes.Buffer
	log := newDebugJSONLogger(&logBuf)

	m := ReasoningEffort{
		Path:     defaultPath,
		log:      log,
		LogUsage: true,
	}

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`
	req := httptest.NewRequest(http.MethodPost, defaultPath, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseWithUsage)
		return nil
	})

	if err := m.ServeHTTP(rec, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}

	// The client must receive the SSE stream byte-for-byte unchanged.
	if got := rec.Body.String(); got != sseWithUsage {
		t.Errorf("SSE stream not passed through unchanged:\n--- got ---\n%s\n--- want ---\n%s", got, sseWithUsage)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("expected text/event-stream Content-Type preserved, got %q", ct)
	}

	// The usage and timings are logged after the stream completes.
	if !strings.Contains(logBuf.String(), `"total_tokens":19`) {
		t.Errorf("expected usage log with total_tokens=19, got:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"predicted_n":7`) {
		t.Errorf("expected timings log with predicted_n=7, got:\n%s", logBuf.String())
	}
}
