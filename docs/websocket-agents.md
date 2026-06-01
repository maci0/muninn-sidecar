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
| **grok** (xAI CLI) | **HTTP** — OpenAI Responses API at `cli-chat-proxy.grok.com/v1/responses` | **Yes**, under `--mitm` | Live-verified: in its default subscription mode grok inference is plain HTTPS (`POST /v1/responses`), not WebSocket — captured, stored, and recalled via the normal path (see `openAIV1BaseCapturePaths`). The `wss://grok.com/ws/gw/` gateway and `wss://.../ws/code-agent` strings exist in the binary but were not exercised for inference here; grok's WebSockets observed in practice are ACP (editor integration), unrelated to LLM content. API-key mode uses `/v1/chat/completions`, also captured. |
| **agy** (Antigravity) | **gRPC/HTTP2 (protobuf)** to `cloudcode-pa.googleapis.com` | No (not a usable capture) | Live-probed: `--mitm` intercepts agy's HTTPS fine (auth/register/userinfo decrypt cleanly), but inference is `application/grpc` protobuf, which the JSON extractors can't read — so turns aren't captured in usable form. Headless `-p` did startup only; an inference RPC needs interactive onboarding. Full support needs protobuf decoding (separate effort). Not a WebSocket. |
| **opencode** | HTTP (OpenAI-compatible); its WebSocket is the local TUI↔server bridge | n/a | The `permessage-deflate`/`wss://` strings are its own client/server channel, not upstream LLM traffic. |
| **claude**, **aider**, **qwen** | HTTP / SSE | Yes (normal path) | No WebSocket LLM transport. qwen (Gemini-CLI fork) captures both OpenAI and Gemini formats. |

## Mapping a new WebSocket protocol

`msc` only decodes codex's Responses-API envelope out of the box. If a future
agent (or a different mode of an existing one) streams inference over a
proprietary WebSocket, you first need to learn its message envelope from live
traffic. Run the agent under `--mitm` with the probe enabled:

```sh
MSC_WS_DEBUG=1 msc --mitm --debug <agent> …
```

If no `ws message` lines appear, the agent isn't streaming inference over a
WebSocket (it's plain HTTP) — capture it by adding its endpoint to the agent's
`CapturePaths` instead. That is exactly how grok turned out: it uses
`POST /v1/responses` over HTTPS, so `/responses` was added to its capture paths
rather than writing a WebSocket handler.

With `MSC_WS_DEBUG` set, the splice logs the JSON `type` field and byte size of
every decoded WebSocket message in each direction (`dir=c->s` / `dir=s->c`) —
the message *shape*, never the content. From those `type`s you can identify the
request and the streamed-delta/completion events and add a handler alongside
codex's in `wscapture.go`. Capture always rides on a best-effort tap that
abandons under backpressure, so probing never blocks or alters the agent.
