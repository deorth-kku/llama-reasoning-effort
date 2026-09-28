package reasoningeffort

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
// is synthesized from the configured Map/LogitBias keys when upstream
// omits it.
type modelReasoning struct {
	SupportedEffort []string       `json:"supported_effort,omitzero"`
	Inline          jsontext.Value `json:",embed"`
}

// bufferedResponseWriter wraps an http.ResponseWriter and buffers the body
// (and records the status code) so the middleware can inspect and rewrite
// the response before it is handed to the client. Headers are captured in a
// private map so downstream writes never mutate the real writer until
// flush runs.
type bufferedResponseWriter struct {
	http.ResponseWriter
	header      http.Header
	statusCode  int
	buf         bytes.Buffer
	wroteHeader bool
}

func newBufferedResponseWriter(w http.ResponseWriter) *bufferedResponseWriter {
	h := make(http.Header, len(w.Header()))
	for k, vv := range w.Header() {
		h[k] = append([]string(nil), vv...)
	}
	return &bufferedResponseWriter{
		ResponseWriter: w,
		header:         h,
		statusCode:     http.StatusOK,
	}
}

// Header returns the buffered header map so downstream writes are captured
// here instead of on the real writer.
func (b *bufferedResponseWriter) Header() http.Header { return b.header }

func (b *bufferedResponseWriter) WriteHeader(code int) {
	if b.wroteHeader {
		return
	}
	b.statusCode = code
	b.wroteHeader = true
}

func (b *bufferedResponseWriter) Write(p []byte) (int, error) {
	if !b.wroteHeader {
		b.WriteHeader(http.StatusOK)
	}
	return b.buf.Write(p)
}

// flush writes the recorded status, headers, and body to the real writer.
func (b *bufferedResponseWriter) flush() error {
	h := b.ResponseWriter.Header()
	for k, vv := range b.header {
		for _, v := range vv {
			h.Add(k, v)
		}
	}
	b.ResponseWriter.WriteHeader(b.statusCode)
	_, err := b.ResponseWriter.Write(b.buf.Bytes())
	return err
}

// serveModels handles the /v1/models listing. It buffers the upstream
// response and enriches every model entry: meta.n_ctx is injected from the
// --ctx-size argument in status.args when missing, and
// reasoning.supported_effort is filled from the Map/LogitBias keys of the
// config selected by the entry's id. Non-JSON responses, error responses,
// and responses without a data array are forwarded unchanged.
func (m ReasoningEffort) serveModels(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	log := m.log
	if log == nil {
		log = zap.NewNop()
	}

	bw := newBufferedResponseWriter(w)
	if err := next.ServeHTTP(bw, r); err != nil {
		return err
	}

	// Only attempt to patch JSON responses.
	if ct := bw.header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "json") {
		return bw.flush()
	}

	var resp modelsResponse
	if err := json.UnmarshalRead(bytes.NewReader(bw.buf.Bytes()), &resp); err != nil {
		log.Debug("n_ctx: /v1/models response is not a JSON object, forwarding unchanged", zap.Error(err))
		return bw.flush()
	}
	if len(resp.Data) == 0 {
		return bw.flush()
	}

	nctx, reasoning := patchModels(&resp, m.ModelConfig, m.ModelConfigs)
	if nctx > 0 {
		log.Info("injected meta.n_ctx into /v1/models response", zap.Int("models", nctx))
	}
	if reasoning > 0 {
		log.Info("injected reasoning.supported_effort into /v1/models response", zap.Int("models", reasoning))
	}

	// Re-serialize the patched response and replace the buffered body.
	patched, err := json.Marshal(resp)
	if err != nil {
		log.Warn("n_ctx: failed to re-serialize /v1/models response, forwarding original", zap.Error(err))
		return bw.flush()
	}
	log.Debug("sending models", zap.Any("data", resp))
	bw.buf = *bytes.NewBuffer(patched)
	bw.header.Set("Content-Length", strconv.Itoa(len(patched)))
	return bw.flush()
}

// patchModels enriches every model entry that is missing fields.
// meta.n_ctx is read from the entry's status.args; reasoning.supported_effort
// is the union of the Map and LogitBias keys of the config selected by the
// entry's id (a matching per-model config wins over the top-level one).
// Existing values always win over synthesized ones. It returns the number
// of entries that gained n_ctx and reasoning respectively.
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

		// reasoning.supported_effort from the config's Map/LogitBias keys.
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

// supportedEfforts returns the sorted union of the config's Map and
// LogitBias keys, or nil when neither map is configured.
func supportedEfforts(cfg ModelConfig) []string {
	if len(cfg.Map) == 0 && len(cfg.LogitBias) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(cfg.Map)+len(cfg.LogitBias))
	for k := range cfg.Map {
		set[k] = struct{}{}
	}
	for k := range cfg.LogitBias {
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
