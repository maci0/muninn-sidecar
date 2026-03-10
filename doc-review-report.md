## Executive Summary
- Major documentation problems: Several docstrings and help text have become outdated due to recent architectural shifts (changing the default vault, adding `muninn_guide` fetching, removing the turn-cooldown, and expanding the context filter markers). A broken docstring in `apiformat.go` was also introduced during refactoring.
- Overall documentation quality assessment: Good, but suffering from "documentation drift" due to rapid feature iteration.
- Top 3 highest impact improvements:
  1. Fix the broken `ExtractUserQuery` docstring in `apiformat.go`.
  2. Update the CLI usage help text to reflect the new dynamic `--vault` default behavior.
  3. Update `Injector` and `Enrich` docstrings to explain that both `where_left_off` and `guide` are fetched on session start.

## Incorrect or Misleading Documentation

**CLI Usage Help text for `--vault`**
- Severity: Medium
- Category: Incorrect Documentation
- Location: `cmd/msc/main.go`, `usage()` function
- Why it matters: The CLI help text claims the default vault is "sidecar", but we recently updated it to default to the current working directory's name.
- Evidence: `      --vault NAME   MuninnDB vault name (default: sidecar)`
- Recommended change: Update the help text to say `(default: current directory name, fallback: sidecar)`.
- Expected benefit: Accurate CLI usage instructions.

**Broken docstring for `ExtractUserQuery`**
- Severity: Medium
- Category: Incorrect Documentation
- Location: `internal/apiformat/apiformat.go`
- Why it matters: A recent refactoring accidentally chopped the first line off the `ExtractUserQuery` docstring, leaving a floating sentence fragment.
- Evidence: `// user message and returns its text content for the given format.`
- Recommended change: Restore the full docstring: `// ExtractUserQuery walks the messages/contents backward to find the last user message and returns its text content for the given format.`
- Expected benefit: Restored API documentation.

## Outdated Documentation

**`Injector` and `Enrich` session-start docstrings**
- Severity: Low
- Category: Outdated Documentation
- Location: `internal/inject/inject.go`
- Why it matters: The docstrings claim that only `muninn_where_left_off` is called on session start, missing the newly added `muninn_guide`.
- Evidence: `// Session-start: call where_left_off once on first enrichment.` and `// On the first call (session start), it also calls muninn_where_left_off to provide continuity...`
- Recommended change: Update the comments to mention both `where_left_off` and `guide`.
- Expected benefit: Accurate reflection of the session initialization sequence.

**`stripInjectedContext` docstring**
- Severity: Low
- Category: Outdated Documentation
- Location: `internal/proxy/filter.go`
- Why it matters: The docstring says it removes blocks "whose text starts with the marker", implying a single marker (`<retrieved-context`). We updated the logic to use `hasInjectedMarker` which strips multiple tags and the `source="muninn"` attribute.
- Evidence: `//   - Anthropic: removes system[] blocks whose text starts with the marker`
- Recommended change: Update the docstring to say it removes blocks "containing Muninn injected context markers".
- Expected benefit: Accurate description of the filtering logic.

**`formatAndDedup` docstring misses tags**
- Severity: Low
- Category: Outdated Documentation
- Location: `internal/store/muninn.go`
- Why it matters: The docstring lists several things the formatter does (strips reminders, deduplicates) but misses the newly added behavior of appending metadata tags to the content footer.
- Evidence: `// formatAndDedup formats an exchange, strips system-reminders, skips empty captures, and deduplicates by concept hash.`
- Recommended change: Add "and appends metadata tags to the content footer".
- Expected benefit: Complete description of the formatting pipeline.

## Low-Value or Redundant Comments
- None identified in the recently modified files.

## Missing Documentation
- None identified in the recently modified files.

## Consistency Issues
- None identified in the recently modified files.

## Suggested Improvements
- Apply the fixes above.

## Deletions
- Remove the leftover `// Need to advance turn tracking so m1 isn't suppressed.` comment in `internal/inject/inject_test.go` (line 395) since turn tracking was completely removed.