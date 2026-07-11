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
            map {
                minimal 128
                low 512
                medium 2048
                high 8192
                xhigh 32768
                max -1
            }
        }
    }
}
```

Or use json config

```json
{
    "handle": [{
        "handler": "reasoning_effort",
        "map": {
            "none": 0,
            "low": 1024,
            "medium": 2048,
            "high": 4096,
            "xhigh": 8192,
            "max": -1
        }
    }]
}
```

## Behavior

1. The plugin only processes requests that target the configured `path`.
2. It reads the request body, decodes it as JSON, and looks for a top-level field named `reasoning_effort`.
3. If found, it replaces the field's value with the corresponding integer from the `map`.
4. If the JSON is invalid or the top-level object doesn't contain `reasoning_effort`, the request is forwarded unchanged.
5. The rewritten body is written back to the response, preserving the original encoding.

## License

MIT
