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

**Chunk granularity does not break the ceiling.** Re-running hard negatives with
sentence-level seeding (`-hard-neg -chunk sentence`) is slightly *worse*, not
better: best gate F1 0.60 (vs 0.64 paragraph), suppress@neg 0.41 (vs 0.49),
inject@should 0.69 (vs 0.71), same lockstep. A lone answer sentence carries less
context, so its cosine to the question is noisier and *lower*, while same-article
sibling sentences still occupy the band. So neither finer chunks (here) nor
score-shape gating (§B1) separates same-topic-right from same-topic-wrong — the
ceiling is a **bi-encoder retrieval limit**, invariant to granularity and
threshold. The only lever left is a cross-encoder / answer-grounding reranker that
jointly scores (query, passage) — a model-based step, deliberately not added here
because its cost/latency must be weighed against a false-inject that is
benign-leaning in the live use case.

### B3 — LLM answer-grounding rerank — built & measured, does NOT beat the gate

Implemented the one remaining lever from §B2: an LLM answer-grounding rerank
(`msc-bench -ground-url <openai-url>` for a local model, or `-ground-cmd "claude
-p"` / `"codex exec"` / `"grok -p"` for frontier CLIs). After cosine recall it
asks the model, per top-K candidate, "does this passage contain a span that
answers the query?" and drops the no's — exactly the cross-encoder step a
bi-encoder cosine cannot do. Re-measured on the same hard-negative instrument
(grounded gate = cosine≥0.30 AND model-accepted):

| grader | inject@should | suppress@neg | note |
|---|---|---|---|
| cosine gate (frontier of the curve) | 0.71 / 0.40 | 0.49 / 0.85 | the lockstep baseline (§B2) |
| qwen2.5:1.5b | 1.00 | 0.00 | accepts everything — useless grader |
| qwen2.5:7b | 0.42 | 0.82 | lands *on* the cosine frontier |
| claude -p (strict prompt) | 0.30 | 0.75 | over-rejects true answers |
| claude -p (extractive prompt) | 0.60 | 0.60 | balanced, still on/below the cosine frontier |

**Conclusion — the precision ceiling is not cheaply breakable in-flight.** Binary
LLM grounding does not produce an (inject, suppress) point above the cosine
frontier on this instrument, for three reasons: (1) **grader quality dominates** —
small models under-reject (1.5b accepts all) or merely match cosine (7b); the
verdict is only as good as the judge. (2) **Frontier graders are too slow for the
hot path** — `claude -p` grades clear yes/no cases perfectly but costs ~3.5s per
(query, passage) call (~7–10s added per recall at top-3), versus an ~11ms cosine
recall; viable only *offline* (vault curation / eval), never as transparent
in-flight injection. (3) **Many same-article "hard negatives" are genuinely
answerable from sibling paragraphs** (encyclopedic text repeats facts), so
suppressing them is *correct* — part of the 0.49 cosine ceiling is right behavior,
not gate failure, and no honest grader should push suppression to 1.0.

**Decision:** keep the cosine gate + auto-calibration as the in-flight design; ship
the grounding rerank as an **offline** precision/curation tool (the `-ground-*`
flags). Frontier-CLI grounding is the right instrument for *evaluating* vault
precision and curating memories. But §B4 shows a *fast local* grounder is also a
viable in-flight precision step for harm-prone vaults — see below.

### B4 — Grounding's value IS real downstream (where false-injects hurt)

§B3 measured grounding on the gate metric (suppress@neg) and found no win — but
that instrument is confounded (sibling paragraphs often genuinely answer). The
honest test is **downstream answer quality on a regime where wrong-paragraph
injects demonstrably hurt**: HotpotQA, ungated (min-score 0.1), where naive cosine
injection went *negative* (§B). Added a 4th "grounded" arm to `msc-qa`
(`-ground-url` / `-ground-cmd`): the injected passages are filtered by an LLM
answer-grounding judgment before the reader sees them. Grounder = qwen2.5:7b
(fast, local), 20 questions, gold-in-recall only 25% (multi-hop retrieval is poor):

| reader | none F1 | cosine-inject F1 | grounded F1 | Δgrounded vs inject |
|---|---|---|---|---|
| qwen2.5:3b-instruct | 0.05 | 0.00 | 0.00 | +0.00 |
| granite3.1-dense:2b | 0.32 | 0.10 | **0.34** | **+0.24** |
| llama3.2:3b | 0.20 | 0.14 | **0.21** | **+0.08** |

**Grounding recovers the harm of bad injection — and beats the threshold gate at
it.** Naive injection cost granite −0.22 (0.32→0.10); grounding restores it to
0.34 (≈ baseline, +0.01 vs none). The earlier production-gate confirming run only
recovered granite to −0.10 at min-score 0.6; **grounding recovers to +0.01 —
better than the cosine gate**, because it drops paragraphs by answer-presence
rather than by a score band that wrong paragraphs still clear. Grounding doesn't
*add* over the no-context baseline on multi-hop (no single paragraph fully answers,
so it correctly drops them → ≈ none), but it *eliminates the downstream harm* that
the threshold gate only partially contains.

**Refined decision:** the cosine gate + auto-calibration stays the default
in-flight (≈11ms, no model). For **harm-prone vaults** (weak retrieval / many
on-topic-but-wrong neighbours, e.g. multi-hop), a *fast local* grounder
(`-ground-url`, ~1s/judge) is a viable in-flight precision upgrade that beats the
threshold at harm-recovery. **Frontier CLIs** (`-ground-cmd`) remain offline-only
(~3.5s/judge, §B3) — best for vault audit/curation. The grounding lever is real;
its place is set by the grader's latency.

**Grounding is listwise (one call per turn), not per-passage.** The judge grades
all candidates that cleared the gate in a single call (`internal/grounding`,
shared by the injector and both eval tools). This is what makes even a slow
grader practical: an inject turn costs *one* round-trip regardless of how many
candidates survived the gate — measured ~289ms for a local qwen2.5:7b judging 3
passages (vs ~1.9s when the same judge ran 3 separate calls), and one frontier
`claude -p` call instead of K. Batching preserved the harm-recovery downstream
(granite on HotpotQA: grounded F1 0.25 vs cosine-inject 0.10, +0.15). It also
narrows the frontier-CLI gap: a single ~3.5s call per *inject* turn (the gate
already suppresses most turns) is borderline-viable in-flight, not only offline.

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

### HuggingFace dataset zoo (reproducible via `scripts/fetch_hf_datasets.py`)

To stress recall/injection across many real retrieval regimes, `scripts/
fetch_hf_datasets.py` pulls assorted HF datasets via the datasets-server API and
converts each to the SQuAD-style JSON the harness seeds (`-corpus squad
-squad-file`). Seeded into dedicated vaults and probed (80 present + 80 absent,
semantic), they span a wide retrieval-difficulty range — and each lands a
*different* optimal gate, which is the empirical case for per-vault
auto-calibration (§F) rather than a fixed threshold:

| vault | dataset | regime | R@1 | best gate T | suppress@neg |
|---|---|---|---|---|---|
| msc-dolly | databricks-dolly-15k (closed_qa) | instruction + context | 1.00 | 0.575 | 1.00 |
| msc-sciq | allenai/sciq | science exam QA (support passage) | 0.49 | 0.70 | 0.99 |
| msc-fever | copenlu/fever_gold_evidence | claim verification (claim→evidence) | 0.40 | 0.625 | 0.97 |
| msc-scifact | BeIR/scifact-generated-queries | scientific abstract retrieval | 0.39 | 0.70 | 0.84 |
| msc-medical | lavita/medical-qa | medical Q→answer | 0.45 | 0.75 | 0.85 |
| msc-narrativeqa | deepmind/narrativeqa (summary) | long-narrative QA | 0.89 | 0.575 | 0.83 |
| msc-code | Nan-Do/code-search-net-python | NL→code retrieval | 0.39 | 0.70 | 0.93 |

Findings: (1) retrieval difficulty is regime-dependent — instruction+context with
distinct contexts is trivial (R@1 1.0), while claim/abstract retrieval is hard
(R@1 ~0.4) because the query wording diverges from the evidence. (2) The optimal
gate ranges 0.575–0.70 across vaults — no single threshold is right, confirming
auto-calibration on real data. (3) **scifact's lower suppress@neg (0.84)** is the
hard-negative ceiling (§B2) appearing *naturally*: scientific abstracts are
topically dense, so an absent claim partially matches seeded abstracts — the same
"on-topic-but-wrong" wall, now observed in a real domain vault rather than a
constructed instrument. (4) These low-cosine vaults are exactly where the recall
floor fix (0.4→0.05, §recall floor) matters — the old floor would have withheld
the moderate-cosine evidence the calibrated gate wants. (5) **Long-narrative
summaries retrieve well** (narrativeqa R@1 0.89): a plot question matches its
dense summary strongly, and the ~820-token summaries seed fine (exercising the
budget path) — long-document recall is not a problem in the *summary* setting. (6) **NL→code is
the product-relevant regime** (a coding agent recalling code by intent): a
natural-language summary matches stored Python at R@1 0.39 — the embedding handles
code but the NL↔code gap is real, like the claim↔evidence gap. Its optimal gate is
higher (0.70) and suppression is clean (0.93), so the sidecar serves its actual
use case correctly *because* auto-calibration adapts the threshold per vault
rather than assuming the prose-tuned 0.6.

**Grounding is grader-domain-dependent (scifact negative).** Running the listwise
answer-grounding rerank (§B4) on scifact's *natural* hard negatives with a general
qwen2.5:7b grader **collapsed** suppression (0.84 → 0.12): the 7b judge can't tell
"does this scientific abstract answer the query?" and over-accepts same-topic
abstracts, so it injects the very near-misses the cosine gate was correctly
suppressing. On a specialized domain the cosine gate *beats* a general grounder.
This sharpens §B3/§B4: grounding only helps when the judge is competent in the
vault's domain — a frontier judge or a domain-tuned one. It is rightly **opt-in
and default-off**; a mediocre or domain-mismatched grader makes precision worse,
not better. (Reconfirms grounding quality = grader quality, now on a real
specialized-domain vault.)

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

### LLM query rewrite/decomposition — tried (local AND frontier), REJECTED

The §B4 grounding work fixes injection *precision* (drop wrong injects) but can
only filter what recall surfaced; it can't recover a paragraph recall missed. So
the obvious recall-side counterpart: have an LLM rewrite/decompose the query into
focused sub-queries before recall, then merge (`msc-bench -rewrite-url` for a fast
local model, `-rewrite-cmd "claude -p"` for a frontier agent). Measured on the
multi-hop hotpot vault (N=12 present probes — small, but consistent across three
conditions):

| condition | R@1 | R@3 | R@5 | MRR | latency/probe |
|---|---|---|---|---|---|
| baseline (no rewrite) | 0.17 | 0.25 | 0.33 | 0.229 | ~290ms |
| qwen2.5:7b rewrite | 0.17 | 0.25 | 0.25 | 0.220 | ~1s |
| **claude -p rewrite** | 0.17 | 0.25 | 0.33 | 0.225 | **~5.5s** |

**No lift — not even from the frontier model**, at 5.5s/probe. The reason is
structural, not a model-quality gap: parallel decomposition cannot satisfy a
multi-hop question because the second hop's query *depends on the first hop's
answer* — no upfront rewrite, however capable, can phrase "the director of the
film that won X" as a lookup without first retrieving X. The sub-queries recall
the same paragraphs the original already found (or noise), so merging doesn't add
the bridge fact. True multi-hop needs *iterative* retrieve→read→retrieve (multiple
recalls + model calls per turn), which is far outside a transparent sidecar's
latency budget. **Decision: not shipped.** Kept as `msc-bench -rewrite-*` flags
for future datasets where a single underspecified turn (not a reasoning chain)
genuinely benefits from rewriting; the confidence gate remains the multi-hop
mitigation. This closes the recall side as the grounding work closed the
precision side: frontier models help injection precision in-flight, but do not
improve single-shot recall on reasoning-chain queries.

## ⚠️ Recall floor must sit below the gate's calibration floor (bug fixed)

MuninnDB's recall `threshold` filters on its **composite `score`** (recency/graph-
inflated), a *different axis* from the `vector_score` cosine the gate uses.
Verified against a live vault: at `threshold=0.4` a memory with cosine **0.449**
was withheld that `threshold=0.05` returned (its composite was 0.376). Since
auto-calibration (§F) can lower the cosine gate `MinScore` to as little as 0.10,
a recall floor of 0.4 would silently withhold high-cosine-but-low-composite
memories the calibrated gate would have injected — calibration below 0.4 was
partly moot. **Fix:** the default recall floor is now **0.05** (below
`calibMinThreshold`=0.10), so the server-side composite pre-filter never pre-empts
the client-side cosine gate; the gate + calibration do the real suppression.
`TestRecallFloorBelowCalibrationFloor` guards the invariant.

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

**Validation on the real HF vaults (`msc-bench` now prints AUTO-CALIBRATED vs the
empirical best).** Feeding each vault's observed cosines to the production
`CalibrateThreshold` and comparing to the labeled balanced-accuracy optimum:

| vault | cluster separation | empirical best T | auto-calibrated T | Δ |
|---|---|---|---|---|
| dolly | clean (R@1 1.0) | 0.575 | 0.600 | **0.025** |
| code | moderate | 0.700 | 0.640 | 0.06 |
| scifact | overlapping | 0.700 | 0.580 | 0.12 |
| fever | overlapping | 0.625 | 0.500 | 0.125 |
| sciq | overlapping | 0.700 | 0.560 | 0.14 |
| medical | overlapping | 0.750 | 0.600 | 0.15 |

Calibration **nails** a cleanly-separated vault (dolly Δ0.025) but the Otsu valley
sits **0.12–0.15 below** the balanced-accuracy optimum when the noise and relevant
cosine clusters *overlap* — it is systematically too permissive on exactly the
hard moderate-cosine vaults (including the product-relevant code regime). The
valley (min intra-class variance) is not the precision-optimal point once the
clusters touch. **Open improvement:** an overlap-aware upward bias on the
calibrated threshold (toward the relevant cluster) should close this without
regressing clean vaults — to be validated across all vaults before shipping.

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

## H — Session-window decay (validated as precision-favoring; tuning harness)

The multi-turn window decays a non-refreshed memory by `decayFactor` (0.7) per
turn and evicts below `decayFloor` (0.2). `RunWindowStudy` (eval_window.go)
simulates multi-turn sessions — memories recalled once, then genuinely relevant
for a short span while fresh recall no longer surfaces them (the case carry-
forward exists for) — and sweeps the decay rate at the **production injection
gate** (decayed score must clear `MinScore` ~0.6 to be injected; `decayFloor`
only governs window retention for a later refresh, not injection):

| decay | precision | recall | F1 |
|---|---|---|---|
| **0.7 (production)** | **0.98** | 0.55 | 0.69 |
| 0.9 (best on this model) | 0.69 | 0.93 | 0.79 |

**Reading:** gated at 0.6, decay=0.7 makes carry-forward injection very precise
(0.98 — a re-injected memory is almost always still relevant) but short (a 0.85
memory falls below 0.6 after ~1 turn), so the window's lasting injection comes
from *re-recall/refresh*, not pure decay. A slower decay extends carry-forward
(more recall, less precision).

**Decision: keep 0.7.** The model's relevance-lifespan distribution is principled
but **uncalibrated** — there is no real multi-turn relevance dataset, unlike the
gate study whose distribution is fit to observed cosine — so this is a tuning
*tool*, not a retune mandate. 0.7 is the precision-favoring choice, consistent
with the sidecar's whole "suppress rather than inject noise" philosophy, and the
test (`TestWindowDecayStudy`) only asserts it is non-degenerate. The harness is
ready to retune the moment real session data exists.

## G — Injection format/presentation (the shipped format, validated downstream)

Every prior downstream eval (§E, §B4) injected **bare content**, but the live
proxy wraps each memory as `[concept] (relevance: 0.XX)\ncontent`
(`formatContextBlock`). So the shipped presentation was never actually tested —
the concept label and relevance number could help the reader weight memories or
distract it. Added `msc-qa -inject-format {bare|labeled|scored}` (scored = live
format) and compared on advqa (N=40, concepts are arbitrary `adv-N`, the
worst case for labels), inj F1:

| reader | bare | labeled | scored |
|---|---|---|---|
| gemma2:2b (small) | **0.39** | 0.38 | 0.33 |
| qwen2.5:7b (capable) | 0.34 | 0.36 | **0.40** |

Two clean effects: (1) the **concept label is neutral** — bare ≈ labeled for both
models, so naming a memory in-context neither helps nor hurts (even when the
concept is meaningless). (2) the **relevance number is model-dependent** — it
*distracts* the small model (gemma2 0.39→0.33, −0.06) but *helps* the capable one
(qwen7b 0.34→0.40, +0.06), which can use the confidence signal to weight
competing memories.

**Decision:** keep the shipped `scored` format. The sidecar's real readers are
capable agent models (claude/gpt/etc.), for which the relevance signal is
net-positive; only sub-3B local models see mild noise from it. Previously assumed,
now measured. The `-inject-format` flag stays for tuning a deployment dominated by
small local models (where `bare`/`labeled` would shave the score-noise).

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
