# UX Review Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix all UX issues found in the muninn-sidecar CLI tool (a transparent reverse proxy for AI coding agents).

**Architecture:** All issues are in user-facing text — startup messages, session summary output, and error messages. No logic changes, only string/copy fixes + one new warning for `--force`.

**Tech Stack:** Go, `cmd/msc/` (CLI), `internal/stats/` (session summary)

---

## UX Findings

### Content & Microcopy

| # | File | Line | Issue | Fix |
|---|------|------|-------|-----|
| 1 | `main.go` | 242 | `"dumping to muninn vault=%q"` — "dumping" is developer jargon | `"storing in vault %q"` |
| 2 | `main.go` | 162 | `"captures will be lost."` — lowercase sentence start on its own line | `"Captures will be lost."` |
| 3 | `main.go` | 50 | `"run 'msc --help' for usage"` — lowercase sentence on its own line | `"Run 'msc --help' for usage."` |
| 4 | `main.go` | ~207 | `--force` silently skips health check with no user warning | Add warning when `--force` is used |
| 5 | `stats.go` | 82 | `"flushed"` — implementation term, not user-friendly | `"saved"` |
| 6 | `stats.go` | 93 | `"errors"` — ambiguous, what kind of errors? | `"store errors"` |
| 7 | `stats.go` | 116 | `"enriched"` — inconsistent with "inject" terminology used elsewhere | `"requests"` (→ "N requests, X tokens injected") |

---

## Task 1: Fix startup message language

**Files:**
- Modify: `cmd/msc/main.go`

- [ ] Change `"dumping to muninn vault=%q"` to `"storing in vault %q"`
- [ ] Capitalize `"captures will be lost."` → `"Captures will be lost."`
- [ ] Capitalize `"run 'msc --help'"` → `"Run 'msc --help'"`
- [ ] Add `--force` warning after proxy starts

---

## Task 2: Fix session summary copy in stats.go + update tests

**Files:**
- Modify: `internal/stats/stats.go`
- Modify: `internal/stats/stats_test.go`

- [ ] Change `"%d flushed"` → `"%d saved"` in `Summary()`
- [ ] Change `"%d errors"` → `"%d store errors"` in `Summary()`
- [ ] Change `"%d enriched, %s tokens injected"` → `"%d requests, %s tokens injected"` in `Summary()`
- [ ] Update `stats_test.go` assertions to match new strings

---

## Task 3: Run tests

- [ ] Run `go test ./...` and verify all tests pass

---
