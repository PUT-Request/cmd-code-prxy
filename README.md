# CommandCode Proxy Server (Prod edition)

OpenAI-compatible proxy server for the CommandCode API. It exposes `/v1/chat/completions` and `/v1/models` endpoints so OpenAI-compatible clients can call CommandCode models through a local HTTP server.

Forked from: https://github.com/dev2k6/command-code-proxy-server

Version: `v1.0.8`

## Features

- OpenAI-compatible chat completions endpoint
- OpenAI-compatible responses endpoint (`/v1/responses`)
- Streaming and non-streaming responses
- OpenAI-compatible model list endpoint
- Short model name mapping
- YAML configuration file (`config.yaml`)
- Multiple client API keys, each with a model allowlist
- Upstream key failover: any upstream error (including 400) retries the
  next key; failed keys cool down for 20 minutes
- Multimodal image input (base64 data URLs)
- Reasoning effort control (`reasoning_effort`: minimal/low/medium/high/max)
- Token usage dashboard with key management
- Upstream key cooldown tracking
- Configurable host and port
- Checks GitHub tags for a newer proxy version and displays it next to the current version

## Requirements

- Go 1.26.2 or newer

## Run

```bash
go run main.go
```

Default server address:

```text
http://127.0.0.1:55990
```

## Dashboard

Optional management UI at `/dashboard/`. Enable it in `config.yaml`:

```yaml
dashboard:
  enabled: true
  username: "admin"
  password: "change-me"
  session_hours: 24
```

Sign in with those credentials. The dashboard shows token usage (24h / 7d / 30d)
broken down by account and by model, and manages `api_keys` in `config.yaml`
(add, edit, delete). Edits apply immediately — no restart needed.

Existing key values are never sent to the browser: the UI shows a masked
fingerprint (`****abcd`), and submitting that mask back means "unchanged". Only
newly typed keys are stored, so secrets cannot be read back through the UI.

Usage is kept in memory and resets on restart. `CC_MAX_TOOL_IMAGE_MB` (see
Image input) applies here too.

The dashboard listens on the same host and port as the API by default. Two
optional settings:

```yaml
dashboard:
  # Serve only on this port, leaving the API port untouched.
  port: "57399"
```

Set `CC_DASHBOARD_SECURE_COOKIE=1` when the panel is served over HTTPS. Leave it
unset for plain HTTP: a `Secure` cookie is silently dropped by browsers over
`http://`, which makes login appear to succeed but bounce straight back to the
login page.

## Configuration

The server reads a `config.yaml` file from the current directory
(override with the first positional argument or the `CONFIG` env var).
Example:

```yaml
# Host and port the proxy listens on.
host: 127.0.0.1
port: 55990

# Client API keys. Each key maps to a pool of upstream CommandCode
# keys; requests fail over to the next one on any upstream error, and
# failed keys cool down for 20 minutes.
api_keys:
  - key: "client-key-1"
    models: []                    # empty = all models, otherwise an allowlist
    command_code_keys:
      - "upstream-cc-key-1"
      - "upstream-cc-key-2"
    rate_limit: 0
  - key: "client-key-2"
    models: ["deepseek-v4-pro"]
    command_code_keys:
      - "upstream-cc-key-3"

# Optional: upstream endpoint (defaults to https://api.commandcode.ai).
# base_url: "https://api.commandcode.ai"

# Optional: management UI at /dashboard/ (see Dashboard).
# dashboard:
#   enabled: true
#   username: "admin"
#   password: "change-me"
#   session_hours: 24

# Enable verbose debug logging of request/response bodies.
debug: false
```

At least one `api_keys` entry with at least one `command_code_keys`
entry is required, or the server refuses to start. A legacy
`api_key` / `server_api_key` config is still accepted and auto-migrated
to a single `api_keys` entry.

## CLI options

```bash
# Run with the default ./config.yaml
go run main.go

# Use a specific config file
go run main.go /etc/command-code-proxy/config.yaml
# or: CONFIG=/etc/command-code-proxy/config.yaml go run main.go
```

The binary is invoked the same way after `go build`.

## Build

Build for the current platform:

```bash
go build -o command-code-proxy-server .
```

Cross-compile for Windows and Linux:

```bash
GOOS=windows GOARCH=amd64 go build -o command-code-proxy-server.exe .
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o command-code-proxy-server-linux-amd64 .
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o command-code-proxy-server-linux-arm64 .
```

## API key behavior

Two kinds of key are in play:

1. **Client API key** (`api_keys[].key`) protects the exposed API. Every request to the proxy endpoints (except `/health`) must include it as `Authorization: Bearer <client-key>`, otherwise the request is rejected with `401 Unauthorized`. The key also decides which models the client may use: an empty `models` list allows everything, otherwise the requested model must be on the allowlist.
2. **CommandCode API keys** (`api_keys[].command_code_keys`) are used for upstream requests. The proxy tries them in order; on any upstream error it fails over to the next, and a failed key cools down for 20 minutes before it is tried again.

Example header:

```http
Authorization: Bearer your-client-key
```

## Endpoints

### Health check

```http
GET /health
```

Response:

```json
{"status":"ok"}
```

### List models

```http
GET /v1/models
```

Returns an OpenAI-compatible model list.

### Chat completions

```http
POST /v1/chat/completions
```

Example non-streaming request:

```bash
curl http://127.0.0.1:55990/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-client-key" \
  -d '{
    "model": "deepseek-v4-pro",
    "messages": [
      {"role": "system", "content": "You are helpful."},
      {"role": "user", "content": "Hello"}
    ],
    "stream": false
  }'
```

Example streaming request:

```bash
curl -N http://127.0.0.1:55990/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-client-key" \
  -d '{
    "model": "deepseek-v4-pro",
    "messages": [
      {"role": "user", "content": "Write a short poem."}
    ],
    "stream": true
  }'
```

### Image input

Images are forwarded as real multimodal parts, not text. Use `image_url` content
blocks with a base64 data URL (`http` URLs cannot be fetched upstream and are
replaced with a text reference):

```bash
curl http://127.0.0.1:55990/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-client-key" \
  -d '{
    "model": "deepseek-v4-pro",
    "messages": [{
      "role": "user",
      "content": [
        {"type": "text", "text": "What is in this screenshot?"},
        {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgo..."}}
      ]
    }]
  }'
```

Images returned inside **tool results** are hoisted into a following `user`
message instead of being inlined as text. This matters: base64 tokenized as
prose costs roughly 700 tokens per MB, so a single 2.76 MB screenshot would
consume ~1.9M tokens and immediately exceed the 1M context window.

`CC_MAX_TOOL_IMAGE_MB` caps the total tool-image bytes sent per request
(default `6`, `0` disables trimming). Older images beyond the budget are
replaced with a placeholder so the model knows a screenshot was dropped.

### Reasoning effort

`reasoning_effort` selects thinking intensity. Accepted values: `minimal`,
`low`, `medium`, `high`, `max`. Unsupported values are ignored (logged, not
fatal) so the upstream default applies.

```bash
curl http://127.0.0.1:55990/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-client-key" \
  -d '{
    "model": "deepseek-v4-pro",
    "messages": [{"role": "user", "content": "Plan the refactor."}],
    "reasoning_effort": "high"
  }'
```

On `/v1/responses`, use the Responses shape instead: `"reasoning": {"effort": "high"}`.

When a client replays assistant history, `reasoning_content` must be echoed back
(it is emitted as a leading `reasoning` block). The upstream validates the
thinking round-trip and rejects requests whose reasoning is missing.

## Supported model aliases

The proxy accepts full model IDs and these short aliases:

| Alias | Maps to |
| --- | --- |
| `deepseek-v4-pro`, `deepseek-v4`, `deepseek-pro` | `deepseek/deepseek-v4-pro` |
| `deepseek-v4-flash`, `deepseek-flash` | `deepseek/deepseek-v4-flash` |
| `minimax-m2.7`, `minimax2.7` | `MiniMaxAI/MiniMax-M2.7` |
| `minimax-m2.5`, `minimax2.5`, `minimax` | `MiniMaxAI/MiniMax-M2.5` |
| `glm-5.1` | `zai-org/GLM-5.1` |
| `glm-5` | `zai-org/GLM-5` |
| `kimi-k2.6`, `kimi2.6` | `moonshotai/Kimi-K2.6` |
| `kimi-k2.5`, `kimi2.5` | `moonshotai/Kimi-K2.5` |
| `qwen-3.6-max-preview`, `qwen3.6-max` | `Qwen/Qwen3.6-Max-Preview` |
| `qwen-3.6-plus`, `qwen3.6-plus`, `qwen3.6` | `Qwen/Qwen3.6-Plus` |
| `step-3.5-flash`, `step3.5` | `stepfun/Step-3.5-Flash` |
| `gemini-3.1-flash-lite`, `gemini-flash-lite` | `google/gemini-3.1-flash-lite` |
| `minimax-m3`, `minimax3` | `MiniMaxAI/MiniMax-M3` |
| `qwen-3.7-max-free`, `qwen3.7-max-free` | `Qwen/Qwen3.7-Max-Free` |
| `qwen-3.7-max`, `qwen3.7-max` | `Qwen/Qwen3.7-Max` |
| `step-3.7-flash`, `step3.7` | `stepfun/Step-3.7-Flash` |
| `mimo-v2.5-pro`, `mimo-pro` | `xiaomi/mimo-v2.5-pro` |
| `mimo-v2.5`, `mimo` | `xiaomi/mimo-v2.5` |

Unknown model names are passed through unchanged.

## Project structure

```text
.
├── README.md
├── config.yaml
├── go.mod
├── go.sum
├── main.go
├── bin
│   ├── command-code-proxy
│   ├── command-code-proxy.exe
│   ├── command-code-proxy-linux-amd64
│   └── command-code-proxy-linux-arm64
└── internal
    ├── api
    │   ├── commandcode.go
    │   └── openai.go
    ├── config
    │   └── config.go
    ├── proxy
    │   ├── convert.go
    │   ├── model.go
    │   └── proxy.go
    ├── server
    │   └── server.go
    ├── update
    │   └── update.go
    └── version
        └── version.go
```

## How it works

1. Client sends an OpenAI-compatible request to the local proxy.
2. The proxy extracts system messages, maps the model name, and converts messages to CommandCode format.
3. The proxy sends the request to `https://api.commandcode.ai/alpha/generate`.
4. CommandCode streaming NDJSON events are converted back to OpenAI-compatible SSE chunks or collected into a single JSON response.

## Version check

On startup, the proxy calls:

```text
https://api.github.com/repos/dev2k6/command-code-proxy-server/tags
```

If the latest GitHub tag is newer than the current app version, the version line is displayed as:

```text
v1.0.8 (latest: v1.x.x)
```

## CommandCode version header

The upstream request includes:

```http
x-command-code-version: <latest npm command-code version>
```

The value is fetched from:

```text
https://registry.npmjs.org/command-code/latest
```

The fetched version is cached for 30 minutes. If the registry request fails, the proxy uses the last cached version, or `unknown` if no version has been fetched yet.
