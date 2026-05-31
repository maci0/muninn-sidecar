# muninn-sidecar docs

- **[recall-and-injection.md](recall-and-injection.md)** — start here. A
  plain-language walkthrough + a decision-flow diagram, then the full design: how
  the sidecar decides *when* to ask MuninnDB, *what* to inject, and *when to inject
  nothing*:
  latest-turn query, `semantic` recall mode, `vector_score` confidence gate
  (auto-calibrated per vault), fitness filter (drop dead/untrusted memories),
  staleness + contradiction resolution, reuse/negative-cache triggers, and the
  optional answer-grounding rerank. The design + the evaluation behind every choice.
- **[experiments.md](experiments.md)** — the full study log: query-construction,
  lexical rerank, chunk granularity, the gate-threshold method study and the
  "threshold is not universal" finding (→ auto-calibration), the recall-floor
  bug, the hard-negative ceiling, the HuggingFace dataset zoo (§Datasets), the
  end-to-end staleness/contradiction validation, and the negative results
  (multi-recall, LLM query-rewrite, per-query shape gates).
- **[model-eval.md](model-eval.md)** — downstream usefulness across **~10 local
  models** and **seven+ task regimes**, plus frontier CLI readers, and the
  unifying law: *injection value ≈ retrieval accuracy × the model's in-context
  ability; a wrong injection never helps*.
- **[testing.md](testing.md)** — test + fuzz posture: every function tested,
  36 fuzz targets over all parsing/transform surfaces, `make cover` / `make fuzz`.

## Dataset zoo

`scripts/fetch_hf_datasets.py` pulls assorted HuggingFace datasets via the
datasets-server API and converts each to the SQuAD-style JSON the harness seeds
(`-corpus squad -squad-file`). Seeded vaults span a wide retrieval-difficulty
range — and each lands a *different* optimal gate, the empirical case for
per-vault auto-calibration:

| converter | regime | R@1 |
|---|---|---|
| `sciq` | science exam QA (support passage) | 0.49 |
| `fever` | claim verification (claim→evidence) | 0.40 |
| `scifact` | scientific abstract retrieval | 0.39 |
| `dolly` | instruction + context | 1.00 |
| `medical` | medical Q→answer | 0.45 |
| `narrativeqa` | long-narrative QA (summary) | 0.89 |
| `code` | NL→code retrieval (product-relevant) | 0.39 |
| `xquad-de` / `xquad-zh` | multilingual (German / Chinese) | 0.51 / 0.18 |
| `quora` | informal community Q&A | 0.40 |

```sh
python3 scripts/fetch_hf_datasets.py all          # write /tmp/<name>.json for each
go run ./cmd/msc-bench -seed -probe -corpus squad -squad-file /tmp/code.json \
    -vault msc-code -squad-articles 250 -n 400 -present 80 -absent 80 -mode semantic
```

## Eval/benchmark tooling (all under `cmd/`)

| tool | purpose |
|---|---|
| `msc-eval` | offline selection-quality harness + `MinScore` sweep + cross-validated method study |
| `msc-bench` | seed a labeled corpus into a real MuninnDB vault; measure retrieval + the gate; validate auto-calibration vs the empirical best. Flags: `-corpus squad\|hotpot\|agentmem\|facts\|diverse`, `-squad-file` (any dataset above), `-hard-neg` (same-article hard negatives), `-ground-url\|-ground-cmd`, `-rewrite-url\|-rewrite-cmd`, `-dump-qa` |
| `msc-qa` | downstream answer-quality across models. Flags: `-dataset squad\|hotpot\|generic`, `-model "m1,m2"` (HTTP) / `-model-cmd "claude -p"` (frontier CLI readers), `-ground-url\|-ground-cmd` (4th grounded arm), `-inject-format bare\|labeled\|scored`, `-answer-hint` (classification regimes like FEVER) |

Architecture overview: [../ARCHITECTURE.md](../ARCHITECTURE.md).
