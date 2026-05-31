# Recall & Injection: How `msc` Decides When and What to Inject

This document describes how Muninn Sidecar decides, **transparently and in-flight**,
whether to query MuninnDB, what it asks for, and which recalled memories (if any)
to inject into the traffic between an AI agent and its model — and the empirical
work behind every one of those choices. No agent involvement, no extra round
trips: it is a reverse-proxy decision on the request body.

## The four decisions

A request flows through four core decisions in `internal/inject`:

| # | Decision | Rule | Knob |
|---|----------|------|------|
| 1 | **When to ask** MuninnDB | Recall only on new intent; reuse the session window on unchanged-query continuations | — (FNV-1a hash of the query) |
| 2 | **How to recall** | Request the `semantic` preset (pure high-precision vector search) | `RecallMode` (default `semantic`) |
| 3 | **When to inject** | Inject only if the best memory's cosine ≥ threshold; otherwise inject nothing this turn | `MinScore` (default `0.6`, auto-calibrated) |
| 4 | **What to inject** | Keep memories ≥ threshold; drop unfit/stale/contradicted/duplicate; pack within the budget | `MinScore`, `Budget` |

Decision 4 carries the correctness filters detailed below (fitness, staleness,
contradiction, near-duplicate) plus an optional answer-grounding rerank.

### 0. What to query with

The recall query is the **latest user turn alone** (system-reminders stripped).
A benchmark (`docs/experiments.md` §A1) found that concatenating prior turns
roughly halves retrieval — the embedding pools all tokens, so unrelated context
dilutes the signal. Continuity across turns comes from the session window, not a
fat query.

### 1. When to ask

Recall costs an MCP round-trip on the request hot path. A coding agent resends
the **same user message** every round of a tool-use chain (with new tool results
appended), so firing a fresh recall each time is wasted latency. `Enrich` hashes
the query; if unchanged **and** the session window still holds memories, it
**reuses the window instead of recalling**. Two refinements: a **negative cache**
skips re-querying when a repeated intent already recalled nothing, and an opt-in
**semantic trigger** (`QuerySimReuse < 1`) reuses on high query word-set overlap,
not just exact match. A continuation neither re-queries nor advances decay, so
the turn counter tracks distinct *intents*, not raw requests. First turn and an
empty window always recall.

### 2. How to recall — `semantic`

MuninnDB offers four recall presets. Benchmarked on a labeled SQuAD corpus
(`cmd/msc-bench`):

| mode | R@1 | MRR |
|------|-----|-----|
| **semantic** | **0.21** | **0.234** |
| balanced | 0.19 | 0.219 |
| deep | 0.17 | 0.201 |
| recent | 0.15 | 0.162 |

`semantic` (pure vector search) wins; `deep` (4-hop graph traversal) and `recent`
(recency bias) add noise. The injector requests `semantic`.

### 3 & 4. When and what to inject — cosine ≥ 0.6

MuninnDB returns two relevance numbers per memory:

- `score` — a composite folding in recency and graph traversal. It exceeds 1.0
  and, critically, **cannot separate relevant from irrelevant at any threshold**
  (a pure-noise query scored `score` = 1.14 on a real instance). Unusable as a gate.
- `vector_score` — raw embedding cosine. Separates cleanly: relevant ~0.6–0.85,
  unrelated-topic ~0.4–0.5.

`normalizeRelevance` rewrites each memory's working score to its cosine (falling
back to `score` only if the cosine field is absent), so the whole pipeline —
decay, ordering, threshold, the displayed relevance — operates on cosine.

`selectForInjection` then keeps every memory with effective (post-decay) cosine
≥ `MinScore`. Because this drops *all* candidates when none is confident enough,
one threshold answers both *when* (empty result → suppress the turn) and *what*
(the survivors). On top of the threshold, three correctness filters run on what
survives:

- **Fitness** — memories MuninnDB marks `archived`, `cancelled`, or `untrusted`
  are dropped regardless of cosine; surfacing retired/abandoned/unreliable content
  as current context misleads the agent (`completed` is kept — a finished
  decision is still relevant).
- **Staleness / supersession** — among same-concept duplicates, the *current*
  memory wins, not the higher-cosine one. Recall is requested with `annotate:true`,
  and MuninnDB's authoritative `stale` flag (falling back to `created_at`) decides
  which version supersedes — so "migrated to Postgres" beats the stale "uses MySQL".
- **Contradiction** — when MuninnDB flags two recalled memories as contradicting
  (`conflicts_with`), only the superseding side is kept (cross-concept), so the
  agent never receives mutually-exclusive "facts" ("deploys to AWS" + "never AWS").

Near-duplicates are also removed (identical normalized concept, or word-set
Jaccard ≥ 0.8), then `withinBudget` greedily packs the survivors. All of the above
is validated end-to-end on a live vault (`docs/experiments.md` §Staleness &
contradiction).

### Optional answer-grounding rerank

For **harm-prone vaults** (weak retrieval, many on-topic-but-wrong neighbours), an
opt-in LLM rerank adds a precision check the bi-encoder cosine can't: a single
*listwise* call judges which gated candidates actually answer the query and drops
the rest. A fast local judge (`--ground-url`, ~1s, one call/turn) is viable
in-flight; a frontier CLI (`--ground-cmd "claude -p"`, ~3.5s) is best offline. It
fails open (a slow/unavailable judge degrades to the cosine gate) and is **off by
default** — it only helps with a domain-competent grader (`docs/experiments.md`
§B3/§B4).

A cross-validated method study (`internal/inject/eval_study.go`) compared this
single-threshold rule against relative-cutoff, top-k, and combined gates on
synthetic data calibrated to the observed cosine ranges; the single absolute
threshold won on held-out F1 while being simplest and least wasteful.

## Self-improving gate (auto-calibration)

`MinScore` is **not a fixed constant in production** — the sidecar tunes it to
each deployment. Cosine magnitude is content-shaped: short-query/short-memory
vaults cluster relevant matches at ~0.6–0.85, but a short question against a long
paragraph (or a vault where MuninnDB returns `vector_score` unpopulated) clusters
much lower, where a hardcoded 0.6 would suppress *everything*. So the injector
samples effective recall scores and periodically retunes `MinScore` to the
noise/relevant valley (Otsu's method, `observeCalibration` + `CalibrateThreshold`):
first after 40 samples, then every 30 recalls to track drift. It only adopts a
valley when the clusters are clearly bimodal (mean gap ≥ 0.08), else keeps the
0.6 prior — so it never latches onto noise. On by default; `--no-auto-calibrate`
keeps the threshold fixed. This is what makes retrieval self-improve in-flight.

## Why 0.6 (the prior)

`MinScore = 0.6` is the cross-validated *starting* value (auto-calibration adjusts
it per vault), confirmed three independent ways:

- **Real MuninnDB benchmark** (`msc-bench`, labeled corpus): gating cosine at 0.6
  gave perfect inject/suppress accuracy, clean plateau over [0.575, 0.675].
- **Corpus MinScore sweep** (`msc-eval -sweep`): plateau 0.55–0.65.
- **Synthetic CV study** (`msc-eval -compare`): tuned absolute threshold ≈ 0.56.

## Does retrieval actually work?

On real SQuAD, **article/topic-level** retrieval by cosine is excellent:
**R@1 = 0.93, MRR = 0.95**. Exact-paragraph R@1 is only ~0.21, but that is
sibling-paragraph ambiguity *within the same Wikipedia article* — any paragraph
from the correct article is useful context, so it does not affect injection
quality. Topic retrieval, the level injection needs, is strong.

## Honest caveats

- When stored memories are **near-duplicates** of each other, neither retrieval
  nor gating can separate them — inherent to vector search, not a bug.
- The gate decides **topic present vs absent** well, but cannot distinguish
  "right topic, wrong specific entity" from a true hit on cosine alone.
- The synthetic study models the recall-score signal, not embedding-recall
  quality; its distributions are calibrated to real cosines but remain synthetic.
- Multilingual reach is **script-dependent**: retrieval holds for Latin-script
  languages (German R@1 0.51) but collapses for CJK (Chinese R@1 0.18). The gate
  degrades safely — it auto-calibrates higher and suppresses rather than injecting
  wrong context — but coverage is low when the embedding is out of its depth.

End-to-end answer quality **is** measured (it was the open question above):
`cmd/msc-qa` runs the recalled context through real models in none/injected/
distractor arms and scores SQuAD EM/token-F1. Across ~10 models and seven+ task
regimes, injection's value tracks retrieval accuracy × the model's in-context
ability, a wrong injection never helps, and the lift is largest where the model
*cannot* answer without memory (agent-memory 0.00→0.88, NL→code 0.03→0.81). See
[model-eval.md](model-eval.md).

## Tooling

| Command | Purpose |
|---------|---------|
| `make eval` / `go run ./cmd/msc-eval` | Offline selection-quality report on the labeled corpus (precision/recall/F1, nDCG, gate accuracy, budget) |
| `go run ./cmd/msc-eval -sweep` | `MinScore` threshold sweep over the corpus |
| `go run ./cmd/msc-eval -compare` | Cross-validated method study (synthetic) |
| `go run ./cmd/msc-eval -live -live-file f.json` | End-to-end against a real MuninnDB |
| `go run ./cmd/msc-bench -seed -probe -corpus facts` | Seed a labeled corpus into a real MuninnDB and measure retrieval + gate |
| `go run ./cmd/msc-bench -corpus squad -mode semantic` | SQuAD retrieval (Recall@k, MRR, article-level) + gate, per recall mode |

## Re-tuning on your own data

Score distributions depend on the embedding model. To re-validate `MinScore` /
`RecallMode` against a real vault:

```sh
# Seed a labeled corpus, then sweep recall modes:
go run ./cmd/msc-bench -seed -probe -corpus squad -vault msc-bench -mode semantic
# Inspect the per-threshold gate table and pick the knee.
```

The selection logic gates on `vector_score`; if your MuninnDB build renames or
omits that field, update `normalizeRelevance` in `internal/inject/inject.go`.

## Observability

`msc status` (session summary) reports the decisions so the transparent gate is
not a black box:

```
inject: 42 injected, 11 suppressed, ~38.0K tokens
recall: 18 queried, 35 reused (window)
grounding: 9 turns judged, 4 candidates dropped
budget: 2 memories truncated (raise --inject-budget)
```

`injected` vs `suppressed` shows the gate at work; `queried` vs `reused` shows the
when-to-ask trigger avoiding redundant recalls; the `grounding` and `budget` lines
appear only when those paths fire.

## Fuzzing the parsing surfaces

Every in-flight parser that ingests untrusted agent/model bytes has a Go fuzz
target (`*/fuzz_test.go`), since the proxy must never panic on malformed traffic:

- `apiformat`: `FuzzExtractUserMessage`, `FuzzExtractAssistantMessage`,
  `FuzzDetectAndExtract`, `FuzzStripSystemReminders`, `FuzzTruncate`, `FuzzExtractSSE`
- `inject`: `FuzzParseRecallResponse`, `FuzzParseWhereLeftOff`, `FuzzParseGuide`,
  `FuzzInjectContext`, `FuzzSelectAndFormat`
- `proxy`: `FuzzCleanRequest`, `FuzzCleanResponse`, `FuzzParseSSEDoc`,
  `FuzzStripInjectedContextDoc`
- `mcpclient`: `FuzzHealthURLFrom`

Seed corpora run on every `go test`. To fuzz a target: `go test ./internal/<pkg>
-run=^$ -fuzz=^FuzzName$ -fuzztime=30s`. Fuzzing hardened `truncateAt` against a
non-positive limit (negative-length slice) and is wired with regression seeds.

