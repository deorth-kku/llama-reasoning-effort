package reasoningeffort

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
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
	ID     string         `json:"id,omitzero"`
	Status modelStatus    `json:"status,omitzero"`
	Meta   *modelMeta     `json:"meta,omitzero"`
	Inline jsontext.Value `json:",embed"`
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
// response and, for every model that lacks meta.n_ctx, injects it from the
// --ctx-size argument in status.args. Non-JSON responses, error responses,
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

	added := patchModels(&resp)
	if added > 0 {
		log.Info("injected meta.n_ctx into /v1/models response", zap.Int("models", added))
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

// patchModels injects meta.n_ctx into every model entry that lacks it,
// reading the context size from the entry's status.args. It returns the
// number of entries patched.
func patchModels(resp *modelsResponse) int {
	added := 0
	for i := range resp.Data {
		e := &resp.Data[i]

		ctxSize := ctxSizeFromArgs(e.Status.Args)
		if ctxSize <= 0 {
			// No --ctx-size to derive from; leave the entry untouched.
			continue
		}
		if e.Meta != nil && e.Meta.NCtx.Valid {
			// Already carries a context size.
			continue
		}

		if e.Meta == nil {
			e.Meta = &modelMeta{
				NCtx: cjson.NewNullable(ctxSize),
			}
		}
		added++
	}
	return added
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
