# Reasoning Effort plugin

A Caddy HTTP plugin that rewrites request bodies for chat completion APIs, mapping the top-level `reasoning_effort` string field to a `thinking_budget_tokens` integer field using a caller-defined mapping.

## Overview

When calling an LLM's chat completion endpoint with a `reasoning_effort` field (e.g., `"minimal"`, `"low"`, `"medium"`, `"high"`, `"xhigh"`, `"max"`), Caddy rewrites the request before it reaches your application. This lets you expose a simple string parameter to clients while internally using token budgets as integers.

## Usage

Add the plugin to your Caddyfile:

```caddyfile
example.com {
    handle /v1/chat/completions * {
        reasoning_effort {
            path /v1/chat/completions
            to_chat_template_key effort
            map minimal 128
            map low 512
            map medium 2048
            map high 8192
            map xhigh 32768
            map max -1
            model llama-4 {
                to_chat_template_key effort
                map medium 4096
            }
        }
    }
}
```

Or use JSON config:

```json
{
    "handle": [{
        "handler": "reasoning_effort",
        "to_chat_template_key": "effort",
        "map": {
            "none": 0,
            "low": 1024,
            "medium": 2048,
            "high": 4096,
            "xhigh": 8192,
            "max": -1
        },
        "model_configs": {
            "llama-4": {
                "to_chat_template_key": "effort",
                "map": {
                    "medium": 4096
                }
            }
        }
    }]
}
```

## Options

| Option | Type | Description |
| --- | --- | --- |
| `path` | string | The request path on which the transformation is applied. Defaults to `/v1/chat/completions`. |
| `map` | string → int | Maps a `reasoning_effort` value to the `thinking_budget_tokens` value. A value of `0` sets `chat_template_kwargs.enable_thinking` to `false` instead. |
| `to_chat_template_key` | string | If set, the `reasoning_effort` value is also written into `chat_template_kwargs` under this key. |
| `model_configs` | model name → config | Per-model overrides. Each entry supports the same `map`, `to_chat_template_key`, and `hooks` options as the top level. |
| `hooks` | hook → config | (Top level) An ordered list of hooks fired for every matching request. See [Hooks](#hooks). |

In the Caddyfile, per-model configs use a `model <name> { ... }` block:

```caddyfile
model llama-4 {
    to_chat_template_key effort
    map medium 4096
}
```

## Behavior

1. The plugin only processes requests that target the configured `path`.
2. It reads the request body, decodes it as JSON, and looks for a top-level field named `reasoning_effort`.
3. The config in use is selected by the request's `model` field: if `model` matches a key in `model_configs`, that per-model config is used; otherwise the default (top-level) config applies. A per-model config is a full override — entries missing from it are not inherited from the default config.
4. If the `reasoning_effort` value matches an entry in the config's `map`, the corresponding `thinking_budget_tokens` is set. A budget of `0` instead sets `chat_template_kwargs.enable_thinking` to `false`.
5. If `to_chat_template_key` is set, the `reasoning_effort` value is also written into `chat_template_kwargs` under that key.
6. If the JSON is invalid or the top-level object doesn't contain `reasoning_effort`, the request is forwarded unchanged.

## Hooks

In addition to rewriting the body, the plugin can fire one or more outbound HTTP **hooks** for every request that matches the configured `path`. Hooks are useful for logging, metrics, tracing, or notifying external systems when a request arrives.

### Behavior

- Hooks fire **before** the body transformation, and always receive the **original** (pre-transformation) request body.
- Hooks are selected by the request's `model` field, exactly like the mapping config: if `model` matches a key in `model_configs`, that model's `hooks` array is used; otherwise the top-level `hooks` array is used. A per-model `hooks` array is a **full override** — it is not merged with the top-level hooks.
- All hooks in the selected array fire, in declaration order.
- Hook execution is **best-effort**: any error is logged, and a failing hook never blocks or fails the original request.
- Each hook has a `type` discriminator. Only `"http"` is supported today; `"command"` is reserved for future use.

### Options (per hook)

| Option | Type | Description |
| --- | --- | --- |
| `type` | string | Hook discriminator. Currently only `"http"`. Required. |
| `url` | string | Target URL for the hook request. Required when `type` is `"http"`. |
| `method` | string | HTTP method for the hook request. Defaults to `POST`. |
| `body` | string | Request body template. Supports [Caddy placeholders](https://caddyserver.com/docs/configuration-syntax/#placeholders) such as `{http.request.body}`. Only usable in the Caddyfile (single-line). |
| `body_json` | JSON value | A raw JSON body, used verbatim (no placeholder resolution). JSON-config-file only; takes precedence over `body` when both are set. |
| `content_type` | string | `Content-Type` for the hook request. Defaults to `application/json` when `body_json` is used, otherwise `text/plain; charset=utf-8` for a string `body`. |
| `timeout` | duration | Per-hook client timeout (e.g. `5s`, `200ms`). Defaults to `5s`. |
| `blocking` | bool | When `false` (default), the hook fires asynchronously in the background and never blocks the request. When `true`, the request waits for the hook to complete. |

### Caddyfile

```caddyfile
example.com {
    handle /v1/chat/completions * {
        reasoning_effort {
            path /v1/chat/completions
            map medium 2048

            # Top-level hooks fire for every matching request.
            # `{http.request.body}` is the original request body.
            hook http https://example.com/ingest {
                method POST
                body {http.request.body}
                content_type application/json
                timeout 5s
            }

            # Per-model hooks fully override the top-level hooks for that model.
            model llama-4 {
                map medium 4096
                hook http https://example.com/llama4 {
                    method POST
                    # A static body: quote it (it contains spaces) and escape
                    # literal braces with \` so the replacer leaves them alone.
                    body '\{"event":"request","model":"llama-4"\}'
                    blocking
                }
            }
        }
    }
}
```

> [!NOTE]
> In the Caddyfile, `body` is a single-line string and only supports the string form (not `body_json`). Because Caddy resolves `{...}` placeholders in the body, a static body containing literal braces must escape them as `\{` and `\}`, and a body containing spaces must be single-quoted.

### JSON config

```json
{
    "handle": [{
        "handler": "reasoning_effort",
        "map": {"medium": 2048},
        "hooks": [
            {
                "type": "http",
                "url": "https://example.com/ingest",
                "method": "POST",
                "body_json": {"event": "request", "reasoning_effort": "medium"},
                "timeout": "5s"
            }
        ],
        "model_configs": {
            "llama-4": {
                "map": {"medium": 4096},
                "hooks": [
                    {
                        "type": "http",
                        "url": "https://example.com/llama4",
                        "body_json": {"event": "request", "model": "llama-4"},
                        "blocking": true
                    }
                ]
            }
        }
    }]
}
```

In the JSON config, `body_json` is written as a raw JSON value (e.g. `{"event": "request"}`), while `body` is a string template that supports Caddy placeholders.

## License

MIT
