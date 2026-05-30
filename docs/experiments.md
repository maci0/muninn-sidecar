# Recall & Injection — Experiment Log

Empirical studies behind the auto-recall/injection design. Each entry: hypothesis
→ method → result → decision. Live experiments run against a real MuninnDB
instance via `cmd/msc-bench`; offline studies via `cmd/msc-eval`. Numbers are from
a seeded SQuAD-dev corpus (`msc-squad` vault) and the `facts` corpus unless noted.

## A1 — Query construction (BIGGEST WIN)

**Hypothesis:** folding the last N conversation turns into one recall query
dilutes intent. **Method:** `msc-bench -query-transform distractors` prepends N
prior unrelated turns to the gold question; `emphasis` puts the gold turn first.
**Result (live, semantic mode):**

| query | R@1 | article R@1 |
|---|---|---|
| latest turn only (baseline) | 0.55 | 0.98 |
| + 2 prior unrelated turns | **0.21** | 0.51 |
| same, latest-turn-first (emphasis) | 0.19 | 0.51 |

Prepending unrelated turns **roughly halves retrieval**, and reordering does *not*
recover it — the embedding pools all tokens, so distractor context dilutes the
signal regardless of position. **Decision (shipped):** the proxy now queries with
the **latest user turn only** (`ExtractUserQuery`), not the last-3-turns concat.
Continuity across turns is already provided by the session window, not the query.

## A2 — Lexical rerank

**Hypothesis:** re-ranking vector candidates by query↔content token overlap helps
exact-entity matches. **Method:** `-rerank lexical` (vector + λ·Jaccard).
**Result:** R@1 0.55 → **0.48** (worse). SQuAD questions paraphrase the answer
paragraph, so lexical overlap is low and adds noise. **Decision:** rejected; pure
vector ranking.

## A3 — Chunk granularity

**Hypothesis:** finer chunks localize the answer better. **Method:** seed SQuAD at
sentence vs paragraph granularity (`-chunk sentence`), gold = the answer-bearing
sentence. **Result:** exact R@1 **0.07** (sentence) vs **0.55** (paragraph);
article-level 0.39 vs 0.98. Sentence chunks lose the surrounding context the
question embedding needs, and multiply near-identical siblings. **Decision:**
paragraph-level chunking; the `store` package should keep exchange chunks coarse.

## A4 — Recall limit (k)

**Method:** `-limit {5,10,20}`. **Result:** R@1 0.53 / 0.55 / 0.54 — flat.
**Decision:** keep limit 10; more candidates don't help retrieval and cost payload.

## B — Gate threshold shape

**Method:** cross-validated method study (`msc-eval -compare`, synthetic cosine
distributions) comparing gate variants. **Held-out F1:**

| method | F1 | note |
|---|---|---|
| absolute (single threshold) | **0.983** | production |
| absolute + cap-N | 0.983 | cap adds nothing (≤ few relevant/turn) |
| absolute + sep-gate | 0.983 | separation signal tunes to off |
| absolute + z-gate | 0.983 | adaptive shape signal tunes to off |
| absfloor + margin | 0.945 | relative-to-top band worse |
| absfloor + relative | 0.956 | |
| relative-only / top-k | 0.84 / 0.72 | can't suppress |

**Decision:** keep the single absolute `vector_score ≥ MinScore` gate; cap-N and
margin variants give no benefit. (Threshold value 0.6 justified in
[recall-and-injection.md](recall-and-injection.md).)

### B1 — Per-query *shape* gates for the WHEN decision — tried, REJECTED

Hypothesis: a query *with* a relevant memory should have a top hit that stands
clear of the noise floor, while a query with *nothing* relevant has scores
bunched low — so the **separation** (top1−top2) or an **adaptive z-score** of the
top hit could cut false injects that a flat threshold lets through. Added both as
candidates layered on `absolute` (`absolute+sepgate`, `absolute+zgate`); each
reduces to plain `absolute` at its degenerate param (sep=0 / z=0), so CV can only
reveal upside.

**Result:** both tie `absolute` bit-for-bit (F1 0.983, 99% gate, 1% wasted, 1.98
inj) — cross-validation tunes the shape knob to **off**. The signal is confounded
by the generative reality the synthetic set models faithfully: (1) when several
relevant memories are recalled, top1−top2 is *small* (the good case looks like the
bunched-noise case), and (2) a tight noise-only cluster has *low* variance, which
*inflates* the top hit's z — so neither separation nor z-score distinguishes
"relevant present" from "noise only" better than the absolute level does. The
absolute cosine level remains the single best discriminator; vault-level noise
drift is handled by auto-calibration (§F), not per-query shape. Kept permanently
in the study as regression-tested evidence.

### B2 — Hard negatives expose the gate's ceiling (real SQuAD data)

Every prior suppression measurement used **easy** negatives: probes from disjoint,
off-topic held-out articles. `msc-bench -corpus squad -hard-neg` instead draws
negatives from held-out **paragraphs of the seeded articles** — same topic, same
entities, heavy lexical overlap, but the answer is in an unseeded sibling
paragraph. This is the one suppression case the synthetic study cannot model.

Production gate (auto-calibrated cosine), 80 present + 80 negative probes each:

| negatives | best gate F1 | gate acc | suppress@neg | inject@should |
|---|---|---|---|---|
| easy (disjoint articles) | 0.95 | 0.96 | **0.99** | 0.93 |
| hard (same-article paragraphs) | 0.64 | 0.60 | **0.49** | 0.71 |

The hard-negative gate curve shows `suppress@neg` and `inject@should` moving in
**lockstep** — raising the threshold from 0.625→0.725 lifts suppression
0.49→0.84 but collapses true injection 0.71→0.33. No single threshold both injects
when it should and suppresses hard negatives, because the answer-bearing paragraph
and its same-article siblings sit in the **same cosine band**.

**Conclusion:** the cosine gate is near-perfect at rejecting off-topic noise
(suppress 0.99) but cannot reject *on-topic-but-answerless* passages (suppress
~0.49) — and no score-shape gate can (§B1: top1≈top2 in both present and hard-neg
cases, so there is no separating signal). This is a **retrieval-precision
ceiling**, the same wall HotpotQA hit (multi-hop §): distinguishing "relevant
topic" from "actually answers the query" needs answer-grounding / cross-encoder
reranking, not a better threshold. It bounds what the *when-to-inject* decision can
achieve alone and marks reranking-for-precision as the next real lever. In the live
msc use case this is benign-leaning: injecting a same-topic project memory that
lacks the exact answer is closer to useful context than to the off-topic
distractor arm — but it is not free, so it sets the agenda for retrieval precision.

## C — Recall trigger (when to ask)

**Shipped:** (1) **negative cache** — a repeated intent that already recalled
nothing skips re-querying; (2) **opt-in semantic trigger** (`QuerySimReuse < 1`)
— reuse the window when the query's word-set Jaccard vs the last query ≥ the
threshold, not just on exact match. Exact-match reuse is the default (safe).
Unit-tested (`TestNegativeCache`, `TestSemanticReuseTrigger`,
`TestRecallReuseOnUnchangedQuery`). These also serve as transcript-style replays
(G18): identical/near-identical/dissimilar query sequences.

## D — MMR diversity

**Conclusion (no code change):** the existing near-duplicate removal (concept
match + content Jaccard ≥ 0.8) *is* an in-proxy MMR-lite. A full MMR needs
per-candidate embeddings, which the proxy does not have (MuninnDB returns scores,
not vectors). Diversity is therefore approximated by token overlap, which is the
best available signal in-flight.

## Datasets

The retrieval + usefulness studies run against multiple seeded vaults:

- **SQuAD v2** (`msc-squad`) — single-hop extractive QA over Wikipedia paragraphs.
  Article-level retrieval R@1 ≈ 0.93–0.98; injection lifts answers strongly (see
  [model-eval.md](model-eval.md)).
- **HotpotQA distractor** (`msc-hotpot`, via HF datasets-server → `msc-bench
  -corpus hotpot`) — **multi-hop** QA needing two linked paragraphs. Single-shot
  semantic recall surfaces the gold paragraph only ~R@1 0.05: a multi-hop question
  doesn't match either supporting paragraph well on its own. This is a real
  limitation — multi-hop needs decomposed/iterative retrieval, which a single
  recall call doesn't do — and it caps the downstream lift (top-5 recalled
  paragraphs are injected, only sometimes containing a supporting hop).
- **facts** (`msc-bench -corpus facts`) — distinct-subject synthetic corpus,
  near-perfect retrieval; used for clean gate calibration.
- **agent-memory** (`msc-bench -corpus agentmem -dump-qa f.json`) — the **most
  product-relevant** regime: synthetic project decisions / config / ownership a
  coding agent would store and recall ("Which datastore does the X service use?").
  Distinct coined subjects, short answer spans. R@1 ≈ 0.32 (coined siblings share
  subwords). `-dump-qa` emits a generic `[{question,answer}]` file that
  `msc-qa -dataset generic` scores against the seeded vault — the reusable path
  for any future dataset.

`msc-bench -corpus {squad|hotpot|agentmem|facts}` seeds; `msc-qa -dataset
{squad|hotpot|generic}` evaluates downstream.

## Multi-recall for multi-hop — tried, REJECTED (no-LLM constraint)

The HotpotQA residual (single-shot recall misses the 2nd hop) suggested a
transparent fix: split the query into capitalized entity spans, recall each, and
merge by best cosine — no LLM call (`msc-bench -multi-recall`, `msc-qa
-multi-recall`). Measured both ways:

- **Retrieval** (hotpot gold@1): 0.10 → **0.07** (slightly worse — extra entity
  matches push the single gold paragraph down).
- **Downstream** (nemotron-mini, hotpot): Δinj F1 +0.32 → **+0.29** (slightly worse).

Merging more paragraphs adds noise without reliably surfacing the missing hop;
entity spans recall their *own* paragraphs, not necessarily the bridge fact.
**Decision: not shipped to production.** A real multi-hop fix needs
reasoning-based query decomposition (an LLM call), which violates the in-flight,
no-extra-latency, no-LLM constraint of the transparent proxy. The flag stays in
the eval tools for future experiments. The cheaper, shipped mitigation remains
the confidence gate: suppress the low-confidence multi-hop recalls rather than
inject wrong context.

## ⚠️ Cross-cutting — the gate threshold is NOT universal (key finding)

Running `msc-qa` build-only (answer-coverage of the gated recall context) on the
SQuAD vault exposed a robustness problem with a *fixed* threshold:

| gate (MinScore) | questions whose recalled context contained the answer |
|---|---|
| 0.0 (ungated) | **92%** |
| 0.30 | 0% |
| 0.60 (production default) | 0% |

A raw recall on that vault showed `vector_score = 0` (absent) and a floored
composite `score ≈ 0.2` for correct paragraphs. So `normalizeRelevance` falls
back to the 0.2 composite, and the 0.6 gate **suppresses everything** — even
though the right paragraph is retrieved (92% coverage ungated).

**Why:** cosine magnitude is content-shaped. The `facts` corpus (short query vs
short memory) yielded cosines 0.6–0.85, so 0.6 was a clean valley. A short
question vs a long Wikipedia paragraph yields much lower cosines, and in some
modes MuninnDB returns `vector_score` unpopulated, leaving only a floored
composite. **A single hand-picked 0.6 does not transfer across deployments.**

**Decision (SHIPPED, default-on):** the gate now self-tunes. The injector samples
effective recall scores and periodically retunes `MinScore` to the noise/relevant
valley via Otsu (`observeCalibration` + `CalibrateThreshold`), so it adapts to
whatever the deployment's embedding/query shape produces — including low-cosine
vaults where the fixed 0.6 suppressed everything. The 0.6 default is now just the
*prior* used until enough samples are seen. Auto-calibration only adopts a valley
when the distribution is meaningfully bimodal (cluster-mean gap ≥ 0.08); on a
unimodal sample it keeps the prior, so it never latches onto noise. Disable with
`--no-auto-calibrate`. `TestAutoCalibrateLowersGate` proves a vault where the 0.6
gate suppresses everything starts injecting after calibration.

## E — Downstream answer quality (gold metric) — RUN (real model)

Ran `cmd/msc-qa` end-to-end through a local ollama model (`qwen2.5:1.5b-instruct`)
over the seeded SQuAD vault, scoring SQuAD EM / token-F1 in three arms:

qwen2.5:1.5b-instruct, N=40:

| arm | EM | F1 | uses-ctx |
|---|---|---|---|
| none (question only) | 0.05 | 0.08 | 5% |
| **injected (recalled memory)** | **0.675** | **0.751** | 78% |
| distractor (irrelevant memory) | 0.000 | 0.000 | 0% |

**Injecting the right memory lifts answers by EM +0.625 / F1 +0.676 (5%→68% EM).
Injecting an irrelevant memory drops them to 0.** A cross-model sweep is in
[model-eval.md](model-eval.md). Two conclusions:
(1) auto-injection is genuinely useful downstream, not just a retrieval metric;
(2) a *wrong* injection actively harms — empirical proof that the confidence gate
(suppress low-confidence turns) is essential, and that injecting noise is worse
than injecting nothing. N is small (local thinking models time out at scale; the
1.5b instruct model was used for throughput), but the effect is large and clean
(0 failed calls). Note: a separate run with `qwen3.6` (a thinking model) at the
default token budget produced empty answers — fixed by raising `max_tokens`; the
harness now also skips slow/failed calls rather than aborting.

### E (build-only, no model)

**Harness:** `cmd/msc-qa` replays SQuAD QA through an OpenAI-compatible model in
three arms (none / injected / distractor), scoring SQuAD EM + token-F1, plus
answer-coverage and a distraction-harm delta. Scoring is unit-tested
(`score_test.go`). **Not run:** no model endpoint available in this environment.
Repro:

```sh
go run ./cmd/msc-bench -seed -corpus squad -vault msc-squad   # ensure seeded
go run ./cmd/msc-qa -vault msc-squad -model-url <openai-compatible-url> -model <name> -n 100
```

Build-only mode (no model) reports how often the gated recall context even
contains the gold answer — the ceiling on achievable injected-answer help.

## F — Auto-calibration of MinScore (SHIPPED, default-on)

`CalibrateThreshold(scores)` finds the noise/relevant valley via Otsu's method,
clamped to a **wide** [0.10, 0.90] so it can adapt to low-cosine vaults, and only
adopts the valley when the clusters are separated by ≥ 0.08 (else keeps the
prior). The injector wires this **online**: `observeCalibration` accumulates a
rolling window (cap 400) of effective recall scores, first calibrates after 40
samples, then refreshes every 30 recalls to track drift. On by default in the
sidecar (`--no-auto-calibrate` to disable); off for direct `New()` callers so
tests stay deterministic. Tested: `TestCalibrateThreshold` (bimodal high/low +
unimodal fallback), `TestAutoCalibrateLowersGate` (end-to-end gate drop).

## G20 — Performance

`msc-bench` reports recall latency: **avg ~71 ms/call** against the local
MuninnDB. The when-to-ask trigger removes most of these calls in tool-use chains
(reuse instead of re-query). Stats (`msc status`) expose recalls queried vs
reused and injected vs suppressed.

## G19 — Multi-hop (HotpotQA) — BUILT, dataset fetch blocked

**Harness:** `msc-bench -corpus hotpot` seeds supporting paragraphs and probes
with multi-hop questions — the fair test for whether `deep` recall (graph
traversal) beats `semantic` when answering needs two linked facts. **Not run:**
the public HotpotQA mirror (`curtis.ml.cmu.edu`) was unreachable in this
environment. Repro: download `hotpot_dev_distractor_v1.json`, then
`go run ./cmd/msc-bench -seed -probe -corpus hotpot -squad-file hotpot.json -vault msc-hotpot -mode deep`
(compare to `-mode semantic`).

## Summary of shipped changes

1. **Query = latest user turn** (A1) — the largest retrieval improvement.
2. **Auto-calibration of the gate, default-on** (F + cross-cutting) — the sidecar
   self-tunes `MinScore` to each vault's score distribution; fixes the
   low-cosine-vault failure where a fixed 0.6 suppressed everything.
3. **Negative cache + opt-in semantic recall trigger** (C).
4. Confirmed-optimal-unchanged: single absolute `vector_score ≥ MinScore` gate (B,
   now auto-tuned), `semantic` recall mode, paragraph chunking (A3), limit 10 (A4).
5. New tooling: `msc-bench` (query-transform, rerank, limit, chunk, mode, hotpot),
   `msc-qa` (downstream), `msc-eval -compare` (method study).
