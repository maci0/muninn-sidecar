# msc — muninn sidecar

A transparent reverse proxy that captures LLM API traffic from coding agents into [MuninnDB](https://github.com/scrypster/muninn).

```
Agent SDK  →  msc (local proxy)  →  LLM API upstream
                    ↓
                 MuninnDB
```

`msc` overrides the agent's API base URL environment variable to route traffic through a local proxy. All traffic is forwarded transparently — only LLM completion endpoints are captured and stored as memories in MuninnDB.

## Supported agents

| Agent | Env var | Default upstream |
|-------|---------|-----------------|
| `claude` | `ANTHROPIC_BASE_URL` | `api.anthropic.com` |
| `gemini` | `CODE_ASSIST_ENDPOINT` | `cloudcode-pa.googleapis.com` |
| `codex` | `OPENAI_BASE_URL` | `api.openai.com` |
| `opencode` | `OPENAI_BASE_URL` | `api.openai.com` |
| `aider` | `OPENAI_API_BASE` | `api.openai.com` |

## Install

```bash
go install github.com/maci0/muninn-sidecar/cmd/msc@latest
```

Or build from source:

```bash
git clone https://github.com/maci0/muninn-sidecar.git
cd muninn-sidecar
make build
```

## Usage

```bash
# Basic usage — launch an agent with API capture
msc claude
msc gemini
msc codex

# Pass arguments through to the agent
msc claude -p "explain this codebase"
msc aider --model gpt-4o

# Capture into a specific MuninnDB vault
msc --vault myproject claude

# Preview config without launching
msc --dry-run gemini

# Suppress msc output
msc --quiet claude

# Launch even if MuninnDB is unreachable (captures will be lost)
msc --force claude

# Check MuninnDB connectivity
msc status
msc --json status

# List supported agents
msc list
msc --json list

# Install shell completions
msc completion zsh >> ~/.zshrc
msc completion bash >> ~/.bashrc
msc completion fish > ~/.config/fish/completions/msc.fish

# Disable memory injection
msc --no-inject claude
```

Flags must come before the agent name. Everything after it passes through to the agent unmodified. Use `--` to separate if needed:

```bash
msc -- claude --weird-flag
```

## How it works

1. `msc` starts a local reverse proxy on a random port
2. It resolves the real upstream URL from the agent's environment (or uses the default)
3. It overrides the agent's API base URL env var to point at the local proxy
4. The agent launches and sends API requests through the proxy
5. All traffic is forwarded transparently (no extra headers, no modified User-Agent)
6. Requests matching the agent's `CapturePaths` (e.g. `/v1/messages`, `GenerateContent`) are captured
7. Captured exchanges are sent to MuninnDB asynchronously via MCP JSON-RPC

### Memory injection

By default, `msc` enriches outgoing LLM requests with relevant memories recalled from MuninnDB. The user's message is used as a search query, and matching memories are injected as system-level context (format-appropriate for Anthropic, OpenAI, and Gemini APIs). Injected context is stripped before storing captured exchanges to prevent recursive reinforcement. Use `--no-inject` to disable this.

### Streaming

SSE streaming responses are handled incrementally — chunks flow through to the agent in real-time. The last SSE `data:` line (which typically contains usage/stop_reason) is captured on stream completion.

### Nested invocations

`msc` sets `MSC_UPSTREAM` in the child environment so nested `msc` calls detect the real upstream and avoid infinite proxy loops.

## Configuration

### Environment variables

| Variable | Description |
|----------|-------------|
| `MUNINN_MCP_URL` | MuninnDB MCP endpoint (default: `http://127.0.0.1:8750/mcp`) |
| `MUNINN_TOKEN` | MuninnDB bearer token (default: reads `~/.muninn/mcp.token`) |
| `MSC_VAULT` | MuninnDB vault name (default: `sidecar`) |

Command-line flags take precedence over environment variables.

### Flags

```
-h, --help         Show help
-v, --version      Show version
-d, --debug        Enable debug logging (structured slog output)
-q, --quiet        Suppress msc's own output
-n, --dry-run      Show resolved config without launching
-j, --json         Machine-readable output (for list, status)
-f, --force        Launch even if MuninnDB is unreachable
    --no-inject    Disable memory injection (enabled by default)
    --vault NAME   MuninnDB vault name
    --mcp-url URL  MuninnDB MCP endpoint
    --token TOKEN  MuninnDB bearer token
```

## Prerequisites

- [MuninnDB](https://github.com/scrypster/muninn) running locally (or reachable via `MUNINN_MCP_URL`)
- The coding agent binary installed and in `PATH`

## License

MIT
