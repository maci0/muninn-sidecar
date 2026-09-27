# Security Policy

## Reporting a vulnerability

Please report security issues **privately** — do not open a public issue.

Use GitHub's private vulnerability reporting: the repository's **Security** tab →
**Report a vulnerability**. Include a description, affected version/commit, and a
reproduction if possible. You'll get an acknowledgement and a fix or mitigation
plan; please allow reasonable time before any public disclosure.

## Supported versions

Fixes target the latest released version and `main`. Pin a released tag for
stability and update when security fixes land.

## Security model & sensitive surfaces

`msc` is a local proxy between a coding agent and its LLM API. A few areas
warrant care:

- **TLS-MITM (`--mitm`, opt-in, off by default).** When enabled, msc generates a
  local certificate authority (key stored `0600` under the user config dir, never
  leaving the machine) and mints per-host leaf certs to decrypt the agent's HTTPS.
  Trust is scoped to the launched child via env vars (`NODE_EXTRA_CA_CERTS` /
  `SSL_CERT_FILE` / …) — msc never installs the CA into the system trust store.
  By default `--mitm` TLS-terminates every CONNECT host the agent opens (the
  agents that need MITM often talk to a backend that isn't their nominal API
  host); use `--mitm-host` to scope interception to the upstream plus listed
  hosts, blind-tunneling everything else untouched. `msc ca` prints the CA
  certificate (never the key) and instructions for trusting it in a browser or
  system store — importing it there is your own decision and widens the CA's
  reach well past the child msc launches.
- **Captured content & secrets.** Exchanges are stored in MuninnDB. msc redacts
  well-known credential formats and personal data (emails, payment-card numbers,
  SSNs) before storage and before injecting recalled
  content, but redaction is **best-effort** (conservative patterns) — not a
  guarantee. Treat the vault as sensitive. `--no-redact` disables write-side
  redaction for trusted local environments.
- **Tokens & transport.** The MuninnDB bearer token is read from
  `~/.muninn/mcp.token` (msc warns on overly-permissive perms or plaintext HTTP to
  a non-loopback endpoint). The proxy listens on loopback (`127.0.0.1`).
- **The loopback listener is unauthenticated.** Any process running as the same
  user can connect to the proxy port; msc does not check client identity. That
  includes `GET /__msc/health`, which answers any caller with the agent name,
  upstream, uptime, and capture counters. Under `--mitm` that client can also
  open a CONNECT tunnel to any host and have the decrypted traffic captured into
  your vault. Only run msc on a machine you trust, and scope MITM with
  `--mitm-host` where you can.
- **The grounding judge is a third party you choose.** With `--ground-url`,
  msc sends the recall query and the candidate memory passages of each turn to
  that endpoint, with `OPENAI_API_KEY` when set; with `--ground-cmd` it pipes
  the same text to a local CLI judge on stdin. Both are off by default, and msc
  warns when the endpoint is plaintext HTTP to a non-loopback host, or is not
  `api.openai.com`. Redaction runs on the prompt, but the passages are still
  your content leaving the machine.
- **The eval CLIs reach whatever host you name.** `msc-eval`, `msc-bench`, and
  `msc-qa` read a local dataset and, when `-model-url`, `-ground-url`, or
  `-rewrite-url` is set, send questions and recalled passages to that endpoint
  with a bearer key (`OPENAI_API_KEY` by default). They redact secrets from the
  prompt first, but the endpoint itself is trusted on name alone. `-token`,
  `-model-key`, `-ground-key`, and `-rewrite-key` pass secrets on the command
  line, where `ps` and shell history expose them; msc warns and proceeds.
  Treat a copied command line as a disclosure decision.
- **Vault content reaches the agent's system prompt.** Memories recalled from
  MuninnDB are injected as system-level context, so anything written into a
  vault by any client or session can steer a later agent turn. Recall is gated
  on relevance, not provenance. Use a distinct `--vault` per project if you
  share a machine with untrusted code.

The build has no third-party dependencies (standard library only); `make vuln`
(govulncheck) and CI scan the reachable code against the Go vulnerability DB.

See [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) for the risk-ranked surface,
trust boundaries, and the threats with no mitigation today, plus
[ARCHITECTURE.md](ARCHITECTURE.md) and the README for the full design.
