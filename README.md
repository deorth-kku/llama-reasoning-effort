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
| `model_configs` | model name → config | Per-model overrides. Each entry supports the same `map` and `to_chat_template_key` options as the top level. |

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

## License

MIT
