package reasoningeffort

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// Interface guards
var (
	_ caddy.Provisioner           = (*ReasoningEffort)(nil)
	_ caddyhttp.MiddlewareHandler = (*ReasoningEffort)(nil)
	_ caddyfile.Unmarshaler       = (*ReasoningEffort)(nil)
)

func init() {
	caddy.RegisterModule(ReasoningEffort{})
	httpcaddyfile.RegisterHandlerDirective("reasoning_effort", parseCaddyfile)
}

// defaultPath is the request path that triggers the field transformation
// when no explicit path is configured.
const defaultPath = "/v1/chat/completions"

// ReasoningEffort is an HTTP handler that rewrites the request body of
// chat completion requests, mapping the top-level `reasoning_effort`
// string field to a `thinking_budget_tokens` integer field using a
// caller-defined mapping.
type ReasoningEffort struct {
	// Path is the request path on which the transformation is applied.
	// Defaults to "/v1/chat/completions".
	Path string `json:"path,omitempty"`

	// Map maps a reasoning_effort value (e.g. "medium") to the
	// corresponding thinking_budget_tokens value. Must be explicitly
	// configured; there is no built-in default mapping.
	Map map[string]int64 `json:"map,omitempty"`

	log *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (ReasoningEffort) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.reasoning_effort",
		New: func() caddy.Module { return new(ReasoningEffort) },
	}
}

// Provision implements caddy.Provisioner.
func (m *ReasoningEffort) Provision(ctx caddy.Context) error {
	if m.Path == "" {
		m.Path = defaultPath
	}
	if len(m.Map) == 0 {
		return fmt.Errorf("reasoning_effort: a non-empty 'map' must be configured")
	}
	m.log = ctx.Logger(m)
	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (m ReasoningEffort) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	log := m.log
	if log == nil {
		log = zap.NewNop()
	}

	// Only transform requests targeting the configured path.
	if r.URL.Path != m.Path {
		return next.ServeHTTP(w, r)
	}

	// Read and preserve the body.
	bodyCopy := bytes.Buffer{}
	tee := io.TeeReader(r.Body, &bodyCopy)
	var v any
	if err := json.NewDecoder(tee).Decode(&v); err != nil {
		// Invalid JSON: skip transformation, forward as-is.
		log.Warn("skipping transformation: body is not valid json", zap.Error(err))
		r.Body = io.NopCloser(&bodyCopy)
		return next.ServeHTTP(w, r)
	}

	// Ensure the body is a JSON object.
	obj, ok := v.(map[string]any)
	if !ok {
		log.Warn("skipping transformation: top-level json is not an object")
		r.Body = io.NopCloser(&bodyCopy)
		return next.ServeHTTP(w, r)
	}

	// Look up reasoning_effort (only when it is a string present in the map).
	if raw, exists := obj["reasoning_effort"]; exists {
		if level, isStr := raw.(string); isStr {
			if budget, found := m.Map[level]; found {
				if budget == 0 {
					kwargs := obj["chat_template_kwargs"]
					switch kwargs := kwargs.(type) {
					case nil:
						obj["chat_template_kwargs"] = map[string]any{
							"enable_thinking": false,
						}
						log.Debug("setting disable-thinking via chat_template_kwargs")
					case map[string]any:
						kwargs["enable_thinking"] = false
						obj["chat_template_kwargs"] = kwargs
						log.Debug("setting disable-thinking via chat_template_kwargs")
					default:
						log.Warn("chat_template_kwargs is not a map, skipping", zap.Any("value", kwargs))
					}
				} else {
					log.Debug("mapped reasoning_effort", zap.String("reasoning_effort", level), zap.Int64("thinking_budget_tokens", budget))
					obj["thinking_budget_tokens"] = budget
				}
			} else {
				log.Info("skipping transformation: unknown reasoning_effort value", zap.String("value", level))
			}
		}
	}

	// Re-serialize and replace the request body.
	newBody, err := json.Marshal(obj)
	if err != nil {
		log.Error("failed to re-serialize request body", zap.Error(err))
		r.Body = io.NopCloser(&bodyCopy)
		return next.ServeHTTP(w, r)
	}

	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))
	r.Header.Set("Content-Length", strconv.Itoa(len(newBody)))

	return next.ServeHTTP(w, r)
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *ReasoningEffort) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if m.Map == nil {
		m.Map = map[string]int64{}
	}

	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "path":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.Path = d.Val()
			case "map":
				if !d.NextArg() {
					return d.ArgErr()
				}
				key := d.Val()
				if !d.NextArg() {
					return d.ArgErr()
				}
				val, err := strconv.ParseInt(d.Val(), 10, 64)
				if err != nil {
					return d.Errf("invalid thinking_budget_tokens value '%s': %v", d.Val(), err)
				}
				m.Map[key] = val
			default:
				return d.Errf("unexpected token '%s'", d.Val())
			}
		}
	}
	return nil
}

// parseCaddyfile unmarshals tokens from h into a new Middleware.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m ReasoningEffort
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}
