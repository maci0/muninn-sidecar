# muninn-sidecar docs

- **[recall-and-injection.md](recall-and-injection.md)** — how the sidecar
  decides *when* to ask MuninnDB, *what* to inject, and *when to inject nothing*:
  latest-turn query, `semantic` recall mode, `vector_score` confidence gate
  (auto-calibrated per vault), reuse/negative-cache triggers. The design + the
  evaluation behind every choice.
- **[experiments.md](experiments.md)** — the full study log: query-construction,
  lexical rerank, chunk granularity, gate-threshold method study, the
  "threshold is not universal" finding (→ auto-calibration), datasets, and the
  multi-recall negative result.
- **[model-eval.md](model-eval.md)** — downstream usefulness across **10 local
  models** and **4 task regimes** (agent-memory, SQuAD, BoolQ, HotpotQA), and the
  unifying law: *injection value ≈ retrieval accuracy × the model's in-context
  ability; a wrong injection never helps*.
- **[testing.md](testing.md)** — test + fuzz posture: every function tested
  (0 at 0% coverage), 31 fuzz targets over all parsing/transform surfaces,
  `make cover` / `make fuzz`.

Eval/benchmark tooling (all under `cmd/`):

| tool | purpose |
|---|---|
| `msc-eval` | offline selection-quality harness + `MinScore` sweep + cross-validated method study |
| `msc-bench` | seed a labeled corpus into a real MuninnDB vault, measure retrieval + the gate (`-corpus squad\|hotpot\|agentmem\|facts`, `-multi-recall`, `-dump-qa`) |
| `msc-qa` | downstream answer-quality across models (`-dataset squad\|hotpot\|generic`, `-model "m1,m2"`) |

Architecture overview: [../ARCHITECTURE.md](../ARCHITECTURE.md).
