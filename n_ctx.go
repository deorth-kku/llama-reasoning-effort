package reasoningeffort

import (
	"encoding/json/jsontext"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	cjson "github.com/deorth-kku/go-common/json"
	"go.uber.org/zap"
)

// modelsResponse mirrors the top-level shape of a /v1/models listing.
// Only the fields the patcher inspects are typed; every other field
// (object, pagination, ...) is captured via Inline for pass-through.
type modelsResponse struct {
	Object string         `json:"object,omitzero"`
	Data   []modelEntry   `json:"data,omitzero"`
	Inline jsontext.Value `json:",embed"`
}

// modelEntry mirrors one element of the /v1/models `data` array.
type modelEntry struct {
	ID        string          `json:"id,omitzero"`
	Status    modelStatus     `json:"status,omitzero"`
	Meta      *modelMeta      `json:"meta,omitzero"`
	Reasoning *modelReasoning `json:"reasoning,omitzero"`
	Inline    jsontext.Value  `json:",embed"`
}

// modelStatus carries the load status plus the raw server arguments. The
// context size is recovered from Args by scanning for --ctx-size.
type modelStatus struct {
	Value  string         `json:"value,omitzero"`
	Args   []string       `json:"args,omitzero"`
	Inline jsontext.Value `json:",embed"`
}

// modelMeta captures the meta block. NCtx is a pointer so a present-but
// missing n_ctx can be told apart from a present n_ctx of 0.
type modelMeta struct {
	NCtx   cjson.Nullable[int64] `json:"n_ctx,omitzero"`
	Inline jsontext.Value        `json:",embed"`
}

// modelReasoning captures the reasoning capabilities block (an
// OpenRouter-style field that llama-server does not provide).
// SupportedEffort lists the reasoning_effort values the model accepts; it
// is synthesized from the configured Map/LogitBias/ReasoningLogitBias keys
// when upstream omits it.
type modelReasoning struct {
	SupportedEffort []string       `json:"supported_effort,omitzero"`
	Inline          jsontext.Value `json:",embed"`
}

// streamingResponseWriter wraps an http.ResponseWriter and feeds body bytes
// into an io.Writer (a cjson.JsonWriter) for streaming decode. Headers and
// status are captured in a private map so the real writer is only committed
// after the upstream handler completes.
type streamingResponseWriter struct {
	http.ResponseWriter
	header      http.Header
	statusCode  int
	body        io.Writer
	wroteHeader bool
}

func newStreamingResponseWriter(w http.ResponseWriter, body io.Writer) *streamingResponseWriter {
	h := make(http.Header, len(w.Header()))
	for k, vv := range w.Header() {
		h[k] = append([]string(nil), vv...)
	}
	return &streamingResponseWriter{
		ResponseWriter: w,
		header:         h,
		statusCode:     http.StatusOK,
		body:           body,
	}
}

// Header returns the captured header map so downstream writes are recorded
// here instead of on the real writer.
func (s *streamingResponseWriter) Header() http.Header { return s.header }

func (s *streamingResponseWriter) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.statusCode = code
	s.wroteHeader = true
}

// Write feeds body bytes to the streaming decoder.
func (s *streamingResponseWriter) Write(p []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	return s.body.Write(p)
}

// commit writes the recorded status and headers to the real writer. The
// caller is responsible for writing the response body afterwards.
// Length-framing headers captured from upstream are dropped: the response
// body is re-serialized (or empty on decode failure) and has a different
// length, so net/http must re-frame it.
func (s *streamingResponseWriter) commit() {
	h := s.ResponseWriter.Header()
	for k, vv := range s.header {
		for _, v := range vv {
			h.Add(k, v)
		}
	}
	h.Del("Content-Length")
	h.Del("Transfer-Encoding")
	s.ResponseWriter.WriteHeader(s.statusCode)
}

// serveModels handles the /v1/models listing. It streams the upstream
// response into a modelsResponse struct via cjson.JsonWriter, enriches every
// model entry (meta.n_ctx from --ctx-size, reasoning.supported_effort from
// the config), then streams the patched struct back out via cjson.JsonReader.
// No large []byte is ever allocated for the full response.
func (m ReasoningEffort) serveModels(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	log := m.log
	if log == nil {
		log = zap.NewNop()
	}

	var resp modelsResponse
	jw := cjson.NewJsonWriter(&resp)
	srw := newStreamingResponseWriter(w, jw)
	if err := next.ServeHTTP(srw, r); err != nil {
		jw.Close()
		return err
	}

	// Complete the streaming decode.
	if err := jw.Close(); err != nil {
		log.Debug("n_ctx: /v1/models response is not valid JSON", zap.Error(err))
		srw.commit()
		return nil
	}

	if len(resp.Data) > 0 {
		nctx, reasoning := patchModels(&resp, m.ModelConfig, m.ModelConfigs)
		if nctx > 0 {
			log.Info("injected meta.n_ctx into /v1/models response", zap.Int("models", nctx))
		}
		if reasoning > 0 {
			log.Info("injected reasoning.supported_effort into /v1/models response", zap.Int("models", reasoning))
		}
	}

	srw.commit()
	jr := cjson.NewJsonReader(resp)
	defer jr.Close()
	_, err := io.Copy(w, jr)
	return err
}

// patchModels enriches every model entry that is missing fields.
// meta.n_ctx is read from the entry's status.args; reasoning.supported_effort
// is the union of the Map, LogitBias, and ReasoningLogitBias keys of the
// config selected by the entry's id (a matching per-model config wins over
// the top-level one). Existing values always win over synthesized ones. It
// returns the number of entries that gained n_ctx and reasoning respectively.
func patchModels(resp *modelsResponse, def ModelConfig, perModel map[string]ModelConfig) (nctx, reasoning int) {
	for i := range resp.Data {
		e := &resp.Data[i]

		// meta.n_ctx from the --ctx-size argument.
		if ctxSize := ctxSizeFromArgs(e.Status.Args); ctxSize > 0 && (e.Meta == nil || !e.Meta.NCtx.Valid) {
			if e.Meta == nil {
				e.Meta = &modelMeta{}
			}
			e.Meta.NCtx = cjson.NewNullable(ctxSize)
			nctx++
		}

		// reasoning.supported_effort from the config's Map/LogitBias/
		// ReasoningLogitBias keys.
		cfg := def
		if mc, ok := perModel[e.ID]; ok {
			cfg = mc
		}
		if efforts := supportedEfforts(cfg); efforts != nil && (e.Reasoning == nil || e.Reasoning.SupportedEffort == nil) {
			if e.Reasoning == nil {
				e.Reasoning = &modelReasoning{}
			}
			e.Reasoning.SupportedEffort = efforts
			reasoning++
		}
	}
	return nctx, reasoning
}

// supportedEfforts returns the sorted union of the config's Map,
// LogitBias and ReasoningLogitBias keys, or nil when none of them is configured.
func supportedEfforts(cfg ModelConfig) []string {
	if len(cfg.Map) == 0 && len(cfg.LogitBias) == 0 && len(cfg.ReasoningLogitBias) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(cfg.Map)+len(cfg.LogitBias)+len(cfg.ReasoningLogitBias))
	for k := range cfg.Map {
		set[k] = struct{}{}
	}
	for k := range cfg.LogitBias {
		set[k] = struct{}{}
	}
	for k := range cfg.ReasoningLogitBias {
		set[k] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ctxSizeFromArgs scans a flat --flag/argument slice for --ctx-size and
// returns the parsed integer value. It returns 0 when the flag is absent
// or unparsable.
func ctxSizeFromArgs(args []string) int64 {
	for i := range args {
		if args[i] == "--ctx-size" {
			if i+1 < len(args) {
				if v, err := strconv.ParseInt(strings.TrimSpace(args[i+1]), 10, 64); err == nil && v > 0 {
					return v
				}
			}
		}
	}
	return 0
}
