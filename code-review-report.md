## Executive Summary
- The codebase is in excellent shape following recent refactoring. The logic is clean, well-abstracted, and thread-safe.
- There are a few remaining opportunities to optimize performance (running independent network calls concurrently) and reduce minor code duplication.
- Top 3 highest ROI cleanup opportunities:
  1. Make session initialization HTTP calls (`fetchWhereLeftOff` and `fetchGuide`) concurrent to reduce startup latency.
  2. Eliminate the duplicated list of internal/reserved commands between `cmd/msc/main.go` and `internal/agents/agents.go`.
  3. Consolidate repetitive flag value extraction logic in `parseFlags`.

## Detailed Findings

### [Sequential HTTP Calls on Session Start]
- Severity: Medium
- Category: Refactoring opportunities / Performance
- Location: `internal/inject/inject.go` (`Enrich` session start)
- Confidence: High
- Why this is a problem: `fetchWhereLeftOff` and `fetchGuide` both make HTTP JSON-RPC calls to the MuninnDB server. Currently, they run sequentially. Since they are independent, running them serially doubles the network latency incurred on the very first LLM request of a session.
- Evidence: 
  ```go
  wlo := inj.fetchWhereLeftOff(ctxW)
  // ... waits for completion ...
  guide := inj.fetchGuide(ctxG)
  ```
- Recommendation: Run both fetches in parallel using goroutines and channels or a `sync.WaitGroup`.
- Expected benefit: Faster time-to-first-token (TTFT) for the first request.
- Estimated effort: Low.

### [Duplicated Internal Commands List]
- Severity: Low
- Category: Duplication
- Location: `cmd/msc/main.go` (`internalCommands`) and `internal/agents/agents.go` (`reservedNames`)
- Confidence: High
- Why this is a problem: The exact same list of commands (`list`, `status`, `help`, `version`, `completion`) is hardcoded in two separate packages. If a new internal command is added, developers must remember to update both lists to prevent an agent name from shadowing it and to ensure flags are parsed correctly.
- Evidence: `internalCommands` map in `main.go` vs `reservedNames` map in `agents.go`.
- Recommendation: Export `IsReserved(name string) bool` or `ReservedCommands` from the `agents` package and use it in `main.go`.
- Expected benefit: Single source of truth, preventing future bugs.
- Estimated effort: Low.

### [Repetitive Flag Value Extraction Boilerplate]
- Severity: Low
- Category: Opportunities to reduce lines of code
- Location: `cmd/msc/main.go` (`parseFlags`)
- Confidence: High
- Why this is a problem: The manual flag parser duplicates bounds-checking and assignment logic for every flag that requires a value (`--vault`, `--mcp-url`, `--token`).
- Evidence: Three identical blocks of `if hasVal { ... } else { i++; if i >= len(args) { return error } ... }`.
- Recommendation: Group these flags under a single `case` statement, extract the value once, and then assign to the appropriate `opts` field.
- Expected benefit: Shorter, cleaner flag parsing loop.
- Estimated effort: Low.

## Quick Wins
- Combine flag value extraction logic in `main.go`.
- Export `IsReserved` from `agents.go` and use it in `main.go`.

## Refactor Plan
1. Immediate cleanups: Run `fetchWhereLeftOff` and `fetchGuide` concurrently in `inject.go`.
2. Safe simplifications: DRY up the internal commands list and flag value extraction.