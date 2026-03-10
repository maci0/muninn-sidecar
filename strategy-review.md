# Context Injection Strategy Review

Our strategy of automatically injecting context into LLM API calls via a transparent proxy is a very clever way to provide "memory" to agents that don't natively support it. However, reviewing the codebase reveals several structural challenges that will prevent it from feeling like a seamless, automated memory system.

Here are the main issues and architectural flaws with the current approach, along with recommendations for making it truly feasible.

### 1. The "Stateless Proxy" Context Amnesia
**The Problem:** The proxy injects `<retrieved-context>` into the request just before sending it to Anthropic/OpenAI/Gemini. The upstream LLM sees it, but the local agent client (e.g., Claude Code, Aider) *does not*. 
In Turn 1, the user asks "fix the auth bug". We retrieve Auth memories and inject them. The LLM fixes the bug.
In Turn 2, the user says "run the tests". The local agent sends the conversation history to the proxy. *The Auth memories are missing from this history* because the agent never saw them. The proxy searches for "run the tests", finds test memories, and injects those.
If the LLM needs to reference the Auth rules again to understand the test output, it can't—it has sudden amnesia because the proxy is swapping out the context block under the hood.
**The Fix:** The proxy needs its own "Session Context Window". Instead of strictly replacing context every turn, the proxy should maintain a rolling list of injected memories for the session and inject a combined block that slowly decays, ensuring stable context over a multi-turn task.

### 2. The Semantic Search "Yes" Problem
**The Problem:** In `apiformat.go`, `ExtractUserQuery` explicitly pulls *only the very last user message* to use as the search query for `muninn_recall`. 
If a user writes a 3-paragraph detailed prompt, this works well. But in interactive CLI agents, users frequently send short follow-ups: "yes", "looks good", "why did that fail?", or "run the linter". 
Running a vector semantic search on the word "yes" will return garbage or completely irrelevant memories, wasting tokens and distracting the LLM.
**The Fix:** The recall query should be richer. The proxy could concatenate the last 2-3 turns (e.g., the last assistant message + the last user message) to provide a much stronger semantic anchor for the `muninn_recall` tool.

### 3. The 2-Turn Cooldown Works Against Us
**The Problem:** In `inject.go`, we have a `turnRing` that enforces a 2-turn cooldown (`filterRecent`), explicitly dropping memories that were recently injected. 
If a user is working on a complex `Database` task for 5 turns, the `Database` memory will be injected in Turn 1, artificially blocked in Turns 2 and 3 (causing amnesia), and then re-injected in Turn 4. This causes the LLM's understanding of the codebase rules to blink in and out of existence.
**The Fix:** Remove the strict cooldown. Instead, deduplicate the *current* context window. If a memory is consistently highly scored by `muninn_recall` across multiple turns, it *should* remain in the context block because the user is clearly still working on that topic. 

### 4. Leaking Markers (`filter.go`)
**The Problem:** In `internal/proxy/filter.go`, `stripInjectedContext` only looks for `injectedContextPrefix = "<retrieved-context"`. However, we recently added `<session-context source="muninn">` (where left off) and `<global-guide source="muninn">` (guide). 
If an agent *does* reflect these back (e.g., if it has a feature to summarize its own system prompt), they will bypass the filter, get captured by `store`, and be permanently embedded as recursive noise in MuninnDB.
**The Fix:** Update `filter.go` to aggressively strip anything matching `source="muninn"` or explicitly add filters for `<session-context` and `<global-guide`.

### 5. Cost and Latency (TTFT)
**The Problem:** Making an HTTP call to MuninnDB (`muninn_recall`) on *every single LLM request* blocks the request. This adds 200-500ms of latency before the LLM even starts generating (Time To First Token). Furthermore, injecting up to 2048 tokens of context on every single turn will rapidly drain API credits for models like Claude 3.5 Sonnet.
**The Fix:** The proxy could run `muninn_recall` asynchronously or only when a significant topic shift is detected. Alternatively, keep the token budget tight (e.g., 1000 tokens) and rely on the fact that caching (like Anthropic's Prompt Caching) will mitigate the cost if the system prompt remains relatively stable (which reinforces the need to fix Issue #1 and #3).

### Summary
The strategy is highly innovative, but currently acts too *discretely* per-turn. To make it feel like true automated memory, the Proxy needs to act more like a continuous "Context Manager" that maintains a stable, slowly-evolving system prompt, rather than a stateless interceptor that aggressively swaps and blocks context based solely on the last 5 words the user typed.