package reasoningeffort

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

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
	_ caddy.CleanerUpper          = (*ReasoningEffort)(nil)
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

// defaultModelsPath is the request path for the llama.cpp model listing.
// When a response arrives here, meta.n_ctx is synthesized for any model
// that lacks it, taken from the server's --ctx-size argument.
const defaultModelsPath = "/v1/models"

// defaultHookTimeout is the per-hook client timeout used when a hook does
// not specify its own timeout.
const defaultHookTimeout = 5 * time.Second

// defaultHookContentType is the Content-Type sent for string (non-JSON)
// hook bodies when the hook does not specify one.
const defaultHookContentType = "text/plain; charset=utf-8"

// ReasoningEffort is an HTTP handler that rewrites the request body of
// chat completion requests, mapping the top-level `reasoning_effort`
// string field to a `thinking_budget_tokens` integer field using a
// caller-defined mapping.
type ReasoningEffort struct {
	// Path is the request path on which the transformation is applied.
	// Defaults to "/v1/chat/completions".
	Path string `json:"path,omitempty"`

	// ModelsPath is the request path for the llama.cpp /v1/models listing.
	// meta.n_ctx is synthesized for models that lack it, taken from the
	// server's --ctx-size argument. Defaults to "/v1/models".
	ModelsPath string `json:"models_path,omitempty"`

	// SlotSavePath is where llama-server's slot save files live. It is
	// either a local directory (llama-server's --slot-save-path) or an
	// smb:// URL of the form
	// smb://[[[domain;]username[:password]@]server[:port]/share[/path]]
	// pointing at the same directory on an SMB share. Setting it enables
	// the slot file LRU (for save/restore tracking and eviction) and the
	// action=delete handler.
	SlotSavePath string `json:"slot_save_path,omitempty"`

	// SlotLRUMax is the maximum number of tracked slot save files.
	// Zero (the default) tracks files without evicting any.
	SlotLRUMax int `json:"slot_lru_max,omitempty"`

	ModelConfig
	ModelConfigs map[string]ModelConfig `json:"model_configs,omitzero"`

	// hookClient is the shared HTTP client used to fire hooks. It is
	// created in Provision and has no global timeout; each hook request
	// gets its own context timeout so different hooks can have different
	// timeouts without racing on the shared client.
	hookClient *http.Client

	// slotLRU is the slot save file LRU, created in Provision when
	// SlotSavePath is set. Nil when the slot features are disabled.
	slotLRU *slotLRU

	// slotSMB is the SMB-backed SlotFS, set in Provision when SlotSavePath
	// is an smb:// URL. Closed in Cleanup.
	slotSMB *smbFS

	log *zap.Logger
}

type ModelConfig struct {
	// Map maps a reasoning_effort value (e.g. "medium") to the
	// corresponding thinking_budget_tokens value. There is no built-in
	// default mapping; values absent from the map are left unchanged.
	Map               map[string]int64 `json:"map,omitempty"`
	ToChatTemplateKey string           `json:"to_chat_template_key,omitempty"`

	// Hooks is the ordered list of hooks fired for requests targeting the
	// model this config applies to. When a request's model matches a
	// key in ReasoningEffort.ModelConfigs, that per-model Hooks array
	// fully overrides the top-level Hooks (no inheritance).
	Hooks []Hook `json:"hooks,omitzero"`
}

// Hook is a single hook configuration. The Type field is a discriminator
// that selects the hook implementation; only "http" is supported today,
// with "command" reserved for future use.
type Hook struct {
	// Type selects the hook implementation. Currently only "http" is
	// supported; "command" is reserved for future use.
	Type string `json:"type"`

	// Command is reserved for a future "command" hook type.
	Command string `json:"command,omitempty"`

	// HTTP hook configuration (used when Type == "http").
	URL         string         `json:"url,omitempty"`
	Method      string         `json:"method,omitempty"`
	BodyJSON    jsontext.Value `json:"body_json,omitzero"`
	Body        string         `json:"body,omitempty"`
	ContentType string         `json:"content_type,omitempty"`
	Timeout     time.Duration  `json:"timeout,omitempty"`
	Blocking    bool           `json:"blocking,omitempty"`
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
	if m.ModelsPath == "" {
		m.ModelsPath = defaultModelsPath
	}
	m.log = ctx.Logger(m)

	if m.hookClient == nil {
		m.hookClient = &http.Client{}
	}

	if err := validateHooks(m.Hooks); err != nil {
		return fmt.Errorf("hooks: %w", err)
	}
	for name, mc := range m.ModelConfigs {
		if err := validateHooks(mc.Hooks); err != nil {
			return fmt.Errorf("model_configs[%q] hooks: %w", name, err)
		}
	}

	log := m.log
	if log == nil {
		log = zap.NewNop()
	}
	if err := m.provisionSlotLRU(log); err != nil {
		return err
	}
	// Log a copy with any SMB credentials in SlotSavePath redacted.
	cfg := *m
	cfg.SlotSavePath = redactSMBURL(m.SlotSavePath)
	log.Debug("config", zap.Any("config", cfg))
	return nil
}

// validateHooks validates a slice of Hook configurations. It ensures every
// hook has a type, that the type is known, and that required fields for the
// given type are present.
func validateHooks(hooks []Hook) error {
	for i, h := range hooks {
		if h.Type == "" {
			return fmt.Errorf("hooks[%d]: type is required", i)
		}
		switch h.Type {
		case "http":
			if h.URL == "" {
				return fmt.Errorf("hooks[%d] (type http): url is required", i)
			}
		case "command":
			return fmt.Errorf("hooks[%d]: type \"command\" is not yet supported", i)
		default:
			return fmt.Errorf("hooks[%d]: unsupported hook type %q", i, h.Type)
		}
	}
	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (m ReasoningEffort) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	log := m.log
	if log == nil {
		log = zap.NewNop()
	}
	switch r.URL.Path {
	case m.ModelsPath:
		// Synthesize meta.n_ctx for /v1/models responses of models that lack it.
		log.Debug("matched models path", zap.String("path", r.URL.Path))
		return m.serveModels(w, r, next)
	case m.Path:
		log.Debug("matched chat-completions path", zap.String("path", r.URL.Path))
	default:
		log.Debug("not matching any path", zap.String("path", r.URL.Path))
		// Try the slot file LRU / delete handler before plain pass-through.
		if handled, err := m.serveSlots(w, r, next); handled {
			return err
		}
		// Only transform requests targeting the configured path.
		return next.ServeHTTP(w, r)
	}

	// Read and preserve the body.
	bodyCopy := bytes.Buffer{}
	tee := io.TeeReader(r.Body, &bodyCopy)

	// Decode into a typed struct. The `inline` tag captures all unknown
	// fields as a pass-through map, so we avoid type assertions for them.
	var body RequestBody
	if err := json.UnmarshalRead(tee, &body); err != nil {
		io.Copy(io.Discard, tee)
		log.Warn("skipping transformation: body is not valid request", zap.Error(err))
		r.Body = io.NopCloser(&bodyCopy)
		return next.ServeHTTP(w, r)
	}

	// Resolve the model config to use, selected by the request's `model`
	// field: a matching key in ModelConfigs overrides the top-level config.
	useconfig := m.ModelConfig
	if mc, ok := m.ModelConfigs[body.Model]; ok {
		useconfig = mc
	}

	// Fire the resolved hooks before the transformation runs. Hooks receive
	// the original (pre-transformation) request body and are selected by the
	// request's model: a per-model Hooks array fully overrides the
	// top-level Hooks. Hooks never block or fail the original request.
	for i := range useconfig.Hooks {
		m.fireHook(r, useconfig.Hooks[i], bodyCopy.Bytes())
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

	if log.Level().Enabled(zap.DebugLevel) {
		log.Debug("full request body", zap.Any("data", body))
	}

	r.Body = io.NopCloser(cjson.NewJsonReader(body))
	// llama.cpp supports unknown content length, so we can save some time by not allocating the full json body as bytes
	r.ContentLength = -1
	r.Header.Del("Content-Length")

	return next.ServeHTTP(w, r)
}

// fireHook fires a single hook for the given request. It is best-effort:
// any error is logged and never propagated to the caller, so the original
// request always proceeds regardless of hook outcome.
//
// originalBody is the request body as received from the client (before any
// transformation); hooks always see this original payload.
func (m *ReasoningEffort) fireHook(r *http.Request, hook Hook, originalBody []byte) {
	log := m.log
	if log == nil {
		log = zap.NewNop()
	}

	if hook.Type == "command" {
		log.Warn("skipping hook: command type is not yet supported", zap.String("type", hook.Type))
		return
	}
	if hook.Type != "http" {
		log.Warn("skipping hook: unsupported type", zap.String("type", hook.Type))
		return
	}

	var rep *caddy.Replacer
	if v, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer); ok {
		rep = v
	} else {
		rep = caddy.NewReplacer()
		rep.Set("http.request.body", string(originalBody))
		rep.Set("http.request.method", r.Method)
		rep.Set("http.request.host", r.Host)
		rep.Set("http.request.remote.host", r.RemoteAddr)
	}

	method := hook.Method
	if method == "" {
		method = http.MethodPost
	}

	// Resolve the body and Content-Type. A raw JSON body takes precedence
	// over the string template; both may be set in config.
	var bodyBytes []byte
	var contentType string
	switch {
	case hook.BodyJSON != nil:
		bodyBytes = hook.BodyJSON
		contentType = "application/json"
	case hook.Body != "":
		bodyBytes = []byte(rep.ReplaceAll(hook.Body, ""))
		if hook.ContentType != "" {
			contentType = hook.ContentType
		} else {
			contentType = defaultHookContentType
		}
	}

	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}

	// The context timeout is canceled only after the request completes, in
	// whichever branch actually performs the send, so an early return from
	// this function does not cancel an in-flight async request.
	ctx, cancel := context.WithTimeout(r.Context(), timeout)

	req, err := http.NewRequestWithContext(ctx, method, hook.URL, bytes.NewReader(bodyBytes))
	if err != nil {
		cancel()
		log.Warn("failed to build hook request",
			zap.String("type", hook.Type),
			zap.String("url", hook.URL),
			zap.Error(err))
		return
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	// Fall back to the default client when the module was constructed
	// directly (e.g. in tests) without going through Provision.
	client := m.hookClient
	if client == nil {
		client = http.DefaultClient
	}

	if hook.Blocking {
		defer cancel()
		if _, err := client.Do(req); err != nil {
			log.Warn("hook request failed",
				zap.String("type", hook.Type),
				zap.String("url", hook.URL),
				zap.Error(err))
		}
		return
	}

	go func() {
		defer cancel()
		if _, err := client.Do(req); err != nil {
			log.Warn("hook request failed",
				zap.String("type", hook.Type),
				zap.String("url", hook.URL),
				zap.Error(err))
		}
	}()
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
			case "models_path":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.ModelsPath = d.Val()
			case "slot_save_path", "slot_lru_max":
				if err := m.parseSlotOption(d); err != nil {
					return err
				}
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
			case "hook":
				h, err := parseHook(d)
				if err != nil {
					return err
				}
				m.Hooks = append(m.Hooks, h)
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
		case "hook":
			h, err := parseHook(d)
			if err != nil {
				return mc, err
			}
			mc.Hooks = append(mc.Hooks, h)
		default:
			return mc, d.Errf("unexpected token '%s' in model config", d.Val())
		}
		if !d.NextBlock(level) {
			return mc, nil
		}
	}
}

// parseHook parses a `hook <type> <url> { ... }` directive (the block is
// optional). The dispenser must be positioned on the `hook` token. Only the
// `http` hook type is supported in the Caddyfile, and only the string `body`
// field (no `body_json`, which is a JSON-config-file-only field).
func parseHook(d *caddyfile.Dispenser) (Hook, error) {
	var hook Hook
	// The dispenser is positioned on the `hook` token. The two following
	// arguments are the hook type and the URL.
	if !d.NextArg() {
		return hook, d.ArgErr()
	}
	hook.Type = d.Val()
	if !d.NextArg() {
		return hook, d.ArgErr()
	}
	hook.URL = d.Val()
	if hook.Type != "http" {
		return hook, d.Errf("unsupported hook type '%s' in Caddyfile (only 'http' is supported)", hook.Type)
	}

	// The dispenser is positioned on the URL token, with the block-opening
	// brace on the same line. Capture the nesting level before entering the
	// block; the loop advances until the matching closing brace is reached.
	level := d.Nesting()
	if !d.NextBlock(level) {
		return hook, nil
	}
	for {
		switch d.Val() {
		case "method":
			if !d.NextArg() {
				return hook, d.ArgErr()
			}
			hook.Method = d.Val()
		case "body":
			if !d.NextArg() {
				return hook, d.ArgErr()
			}
			hook.Body = d.Val()
		case "content_type":
			if !d.NextArg() {
				return hook, d.ArgErr()
			}
			hook.ContentType = d.Val()
		case "timeout":
			if !d.NextArg() {
				return hook, d.ArgErr()
			}
			parsed, err := time.ParseDuration(d.Val())
			if err != nil {
				return hook, d.Errf("invalid timeout value '%s': %v", d.Val(), err)
			}
			hook.Timeout = parsed
		case "blocking":
			hook.Blocking = true
		default:
			return hook, d.Errf("unexpected token '%s' in hook", d.Val())
		}
		if !d.NextBlock(level) {
			return hook, nil
		}
	}
}

// parseCaddyfile unmarshals tokens from h into a new Middleware.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m ReasoningEffort
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}
