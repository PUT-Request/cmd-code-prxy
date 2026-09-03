# CommandCode Proxy Server

OpenAI-compatible proxy server for the CommandCode API. It exposes `/v1/chat/completions` and `/v1/models` endpoints so OpenAI-compatible clients can call CommandCode models through a local HTTP server.

Repository: https://github.com/dev2k6/command-code-proxy-server

Version: `v1.0.8`

## Features

- OpenAI-compatible chat completions endpoint
- Streaming and non-streaming responses
- OpenAI-compatible model list endpoint
- Short model name mapping
- YAML configuration file (`config.yaml`)
- Exposed API protected by a required API key
- Default CommandCode API key from config
- Per-request CommandCode API key via `x-command-code-api-key` header
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

## Configuration

The server reads a `config.yaml` file from the current directory (override with `-config`). Example:

```yaml
# Host and port the proxy listens on.
host: 127.0.0.1
port: 55990

# Default CommandCode API key used for upstream requests.
# Clients can override it per-request with the `x-command-code-api-key` header.
api_key: ""

# API key required to access the exposed API endpoints.
# Clients must send it as: Authorization: Bearer <server_api_key>
# This is mandatory - the server will refuse to start without it.
server_api_key: "change-me"

# Enable verbose debug logging of request/response bodies.
debug: false
```

If the config file is missing, defaults are used (host `127.0.0.1`, port `55990`). `server_api_key` is always required, either in the config file or via `-server-api-key`.

## CLI options

```bash
go run main.go [options]
```

| Option | Default | Description |
| --- | --- | --- |
| `-config` | `config.yaml` | Path to the YAML config file |
| `-host` | `127.0.0.1` | Host to bind the server to (overrides config) |
| `-port` | `55990` | Port to run the server on (overrides config) |
| `-api-key` | empty | Default CommandCode API key (overrides config) |
| `-server-api-key` | empty | API key required to access the exposed API (overrides config) |
| `-version` | `false` | Print version and exit |

Examples:

```bash
# Run with the default ./config.yaml
go run main.go

# Use a specific config file
go run main.go -config /etc/command-code-proxy/config.yaml

# Run on a custom port
go run main.go -port 8080

# Expose on all interfaces
go run main.go -host 0.0.0.0

# Override keys from the command line
go run main.go -server-api-key my-gateway-key -api-key my-commandcode-key

# Print version
go run main.go -version
```

## Build

Build for the current platform:

```bash
go build -o bin/command-code-proxy
```

Cross-compile for Windows and Linux:

```bash
GOOS=windows GOARCH=amd64 go build -o bin/command-code-proxy.exe
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/command-code-proxy-linux-amd64
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/command-code-proxy-linux-arm64
```

## API key behavior

Two separate keys are in play:

1. **Server API key** (from `server_api_key`) protects the exposed API. Every request to the proxy endpoints (except `/health`) must include it as an `Authorization: Bearer <server_api_key>` header, otherwise the request is rejected with `401 Unauthorized`.
2. **CommandCode API key** (from `api_key`) is used for upstream requests to CommandCode. Clients can override it per-request with the `x-command-code-api-key` header. If neither exists, the request returns `401 Unauthorized`.

Example headers:

```http
Authorization: Bearer your-server-api-key
x-command-code-api-key: your-commandcode-api-key
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
  -H "Authorization: Bearer your-server-api-key" \
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
  -H "Authorization: Bearer your-server-api-key" \
  -d '{
    "model": "deepseek-v4-pro",
    "messages": [
      {"role": "user", "content": "Write a short poem."}
    ],
    "stream": true
  }'
```

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

On startup and when running `-version`, the proxy calls:

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
