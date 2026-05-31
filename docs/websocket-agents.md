# WebSocket-streaming agents

Most coding agents talk to their LLM backend over plain HTTP (often SSE), which
`msc` captures directly (or under `--mitm` for OAuth-direct hosts). A few stream
turns over a **WebSocket** instead, which the normal reverse-proxy path can't
read — `msc` has to decode the frames. This page records which installed agents
do that, based on a static scan of their binaries plus live verification.

## Findings

| Agent | LLM transport | Capturable today? | Notes |
|-------|---------------|-------------------|-------|
| **codex** (ChatGPT mode) | permessage-deflate **WebSocket** (OpenAI Responses API) | **Yes**, under `--mitm` | Decoded by `wsframe.go`/`wscapture.go`; verified live. API-key mode is plain HTTP. |
| **grok** (gateway mode) | **WebSocket** — `wss://grok.com/ws/gw/`, `wss://code.grok.com/ws/code-agent` | Not yet | Proprietary gateway envelope (not Responses API). API-key/chat-proxy mode uses HTTP SSE (`cli-chat-proxy.grok.com/v1/chat/completions`) and *is* captured. grok also uses WebSockets for ACP (editor integration), unrelated to LLM content. |
| **agy** (Antigravity) | **gRPC/HTTP2** to `cloudcode-pa.googleapis.com` | Not via WS capture | Not a WebSocket protocol; needs gRPC-aware interception, a separate effort. |
| **opencode** | HTTP (OpenAI-compatible); its WebSocket is the local TUI↔server bridge | n/a | The `permessage-deflate`/`wss://` strings are its own client/server channel, not upstream LLM traffic. |
| **reasonix**, **claude**, **aider**, **qwen** | HTTP / SSE | Yes (normal path) | No WebSocket LLM transport. |

## Mapping a new WebSocket protocol

`msc` only decodes codex's Responses-API envelope out of the box. To extend
capture to another agent (e.g. grok's gateway), you first need to learn its
message envelope from live traffic:

```sh
MSC_WS_DEBUG=1 msc --mitm --debug grok …
```

With `MSC_WS_DEBUG` set, the splice logs the JSON `type` field and byte size of
every decoded WebSocket message in each direction (`dir=c->s` / `dir=s->c`) —
the message *shape*, never the content. From those `type`s you can identify the
request and the streamed-delta/completion events and add a handler alongside
codex's in `wscapture.go`. Capture always rides on a best-effort tap that
abandons under backpressure, so probing never blocks or alters the agent.
