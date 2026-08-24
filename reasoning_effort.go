package reasoningeffort

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	cjson "github.com/deorth-kku/go-common/json"
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
	ModelConfig
	ModelConfigs map[string]ModelConfig `json:"model_configs,omitzero"`
	log          *zap.Logger
}

type ModelConfig struct {
	// Map maps a reasoning_effort value (e.g. "medium") to the
	// corresponding thinking_budget_tokens value. There is no built-in
	// default mapping; values absent from the map are left unchanged.
	Map               map[string]int64 `json:"map,omitempty"`
	ToChatTemplateKey string           `json:"to_chat_template_key,omitempty"`
}

// RequestBody is the JSON shape we care about for the transformation.
// Fields defined explicitly are accessed by name with their Go types.
// Fields captured via `inline` are passed through unchanged — no type
// assertions needed for the rest of the payload.
type RequestBody struct {
	Model              string         `json:"model,omitzero"`
	ReasoningEffort    string         `json:"reasoning_effort,omitzero"`
	ChatTemplateKwargs kwargs         `json:"chat_template_kwargs,omitzero"`
	ThinkingBudget     int64          `json:"thinking_budget_tokens,omitzero"`
	Inline             jsontext.Value `json:",embed"`
}

type kwargs struct {
	EnableThinking cjson.Nullable[bool] `json:"enable_thinking,omitzero"`
	Inline         map[string]any       `json:",embed"`
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

	// Decode into a typed struct. The `inline` tag captures all unknown
	// fields as a pass-through map, so we avoid type assertions for them.
	var body RequestBody
	if err := json.UnmarshalRead(tee, &body); err != nil {
		log.Warn("skipping transformation: body is not valid request", zap.Error(err))
		r.Body = io.NopCloser(&bodyCopy)
		return next.ServeHTTP(w, r)
	}

	useconfig, ok := m.ModelConfigs[body.Model]
	if !ok {
		useconfig = m.ModelConfig
	}

	if level := body.ReasoningEffort; level != "" {
		// Look up reasoning_effort (only when it is a string present in the map).
		if budget, found := useconfig.Map[level]; found {
			if budget == 0 {
				log.Debug("setting disable-thinking via chat_template_kwargs")
				body.ChatTemplateKwargs.EnableThinking = cjson.NewNullable(false)
			} else {
				log.Debug("mapped reasoning_effort", zap.String("reasoning_effort", level), zap.Int64("thinking_budget_tokens", budget))
				body.ThinkingBudget = budget
			}
		} else if len(useconfig.Map) > 0 {
			// Only log when a map is configured; otherwise the value is
			// simply not mapped and there is nothing to report.
			log.Info("skipping transformation: unknown reasoning_effort value", zap.String("value", level))
		}

		if useconfig.ToChatTemplateKey != "" {
			if body.ChatTemplateKwargs.Inline == nil {
				body.ChatTemplateKwargs.Inline = make(map[string]any)
			}
			body.ChatTemplateKwargs.Inline[useconfig.ToChatTemplateKey] = level
		}
	}

	// Re-serialize and replace the request body.
	newBody, err := json.Marshal(body)
	if err != nil {
		log.Error("failed to re-serialize request body", zap.Error(err))
		return err
	}

	if log.Level().Enabled(zap.DebugLevel) {
		log.Debug("full request body", zap.String("data", string(newBody)))
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
			case "to_chat_template_key":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.ToChatTemplateKey = d.Val()
			case "model":
				if !d.NextArg() {
					return d.ArgErr()
				}
				modelName := d.Val()
				mc, err := parseModelConfigBlock(d)
				if err != nil {
					return err
				}
				if m.ModelConfigs == nil {
					m.ModelConfigs = map[string]ModelConfig{}
				}
				m.ModelConfigs[modelName] = mc
			default:
				return d.Errf("unexpected token '%s'", d.Val())
			}
		}
	}
	return nil
}

// parseModelConfigBlock parses a `model <name> { ... }` block into a
// ModelConfig. The dispenser must be positioned at the model name token,
// with the block opening brace on the same line. Only `map` and
// `to_chat_template_key` are allowed inside a model block.
func parseModelConfigBlock(d *caddyfile.Dispenser) (ModelConfig, error) {
	var mc ModelConfig
	// The dispenser is positioned on the model name token, with the
	// block-opening brace on the same line. The first NextBlock call both
	// opens the block and lands on the first content token; subsequent
	// calls advance until the matching closing brace is reached.
	level := d.Nesting()
	if !d.NextBlock(level) {
		return mc, d.Errf("model config requires a '{ ... }' block")
	}
	for {
		switch d.Val() {
		case "map":
			if !d.NextArg() {
				return mc, d.ArgErr()
			}
			key := d.Val()
			if !d.NextArg() {
				return mc, d.ArgErr()
			}
			val, err := strconv.ParseInt(d.Val(), 10, 64)
			if err != nil {
				return mc, d.Errf("invalid thinking_budget_tokens value '%s': %v", d.Val(), err)
			}
			if mc.Map == nil {
				mc.Map = map[string]int64{}
			}
			mc.Map[key] = val
		case "to_chat_template_key":
			if !d.NextArg() {
				return mc, d.ArgErr()
			}
			mc.ToChatTemplateKey = d.Val()
		default:
			return mc, d.Errf("unexpected token '%s' in model config", d.Val())
		}
		if !d.NextBlock(level) {
			return mc, nil
		}
	}
}

// parseCaddyfile unmarshals tokens from h into a new Middleware.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m ReasoningEffort
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}
