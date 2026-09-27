#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")" || exit 1

# Clear env vars from a parent msc session to avoid nested detection. The
# upstream sentinel is per-agent (MSC_UPSTREAM_<AGENT>), not a bare
# MSC_UPSTREAM, so name the agents this script drives.
unset MSC_UPSTREAM_CLAUDE MSC_UPSTREAM_CODEX MSC_UPSTREAM_QWEN \
    ANTHROPIC_BASE_URL CLAUDECODE 2>/dev/null || true

# Every external command the script shells out to, named once. Without these
# checks a missing jq turns the quality checks into a cascade of "0 matches"
# passes and a missing curl fails somewhere in the middle of a run.
missing=""
for tool in curl jq make go; do
    command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [[ -n "$missing" ]]; then
    echo "FAIL: required tools not on PATH:$missing" >&2
    exit 1
fi

# An unresponsive MuninnDB must fail the run, not wedge it: curl gets both a
# connect and an overall deadline, so a socket that accepts and then says
# nothing still ends in an error this script can report.
CURL_CONNECT_TIMEOUT="${CURL_CONNECT_TIMEOUT:-5}"
CURL_MAX_TIME="${CURL_MAX_TIME:-60}"
# MuninnDB enriches after a capture lands; the recall below reads what it wrote.
SETTLE_SECONDS="${SETTLE_SECONDS:-2}"

DEBUG=""
if [[ "${1:-}" == "-d" ]]; then
    DEBUG="-d"
fi

VAULT="msc-test-$$"
MCP_URL="${MUNINN_MCP_URL:-http://127.0.0.1:8750/mcp}"
TOKEN_FILE="${MUNINN_TOKEN_FILE:-$HOME/.muninn/mcp.token}"
TOKEN=$(cat "$TOKEN_FILE" 2>/dev/null || echo "")
ERRORS=0

if [[ -z "$TOKEN" ]]; then
    echo "WARN: no MuninnDB token found at $TOKEN_FILE — requests may fail"
fi

# The auth header goes in a 600-mode file, not in curl's argv: the process
# table is world-readable, so a token passed as an argument stays visible to
# every local user for as long as curl runs. `curl -H @file` reads the lines
# verbatim, so no token character has to survive config or shell escaping.
# The EXIT trap below removes it.
HEADER_FILE="$(mktemp)" || exit 1
chmod 600 "$HEADER_FILE"
{
    echo "Content-Type: application/json"
    echo "Authorization: Bearer $TOKEN"
} > "$HEADER_FILE"

mcp_call() {
    local tool="$1"
    local args="$2"
    curl -s --connect-timeout "$CURL_CONNECT_TIMEOUT" --max-time "$CURL_MAX_TIME" \
        "$MCP_URL" \
        -H "@$HEADER_FILE" \
        -d "{\"jsonrpc\":\"2.0\",\"method\":\"tools/call\",\"params\":{\"name\":\"$tool\",\"arguments\":$args},\"id\":1}"
}

# Run one agent capture and count a non-zero exit as a failure. `set -e` is
# off (the quality checks below report every problem in one pass), so without
# this an agent that dies on every call still ends the script green.
run_agent() {
    local label="$1"
    shift
    local status=0
    ./msc $DEBUG --vault "$VAULT" "$@" 2>&1 || status=$?
    echo "--- exit: $status ---"
    if [[ "$status" -ne 0 ]]; then
        echo "FAIL: $label exited $status"
        ERRORS=$((ERRORS + 1))
    fi
    echo ""
}

# shellcheck disable=SC2329,SC2317  # invoked via the trap below; <0.10 says the body is unreachable (SC2317), >=0.10 the function uninvoked (SC2329)
cleanup() {
    echo ""
    echo "=== Cleanup: deleting test vault memories ==="
    local ids
    ids=$(mcp_call "muninn_recall" "{\"vault\":\"$VAULT\",\"context\":[\"capital city country\"],\"limit\":50,\"threshold\":0.0}" \
        | jq -r '.result.content[0].text' 2>/dev/null \
        | jq -r '.memories[].id' 2>/dev/null)

    local count=0
    for id in $ids; do
        mcp_call "muninn_forget" "{\"vault\":\"$VAULT\",\"id\":\"$id\"}" > /dev/null 2>&1
        count=$((count + 1))
    done
    echo "Deleted $count memories from vault '$VAULT'"
    rm -f msc "$HEADER_FILE"
}
trap cleanup EXIT

echo "=== Building msc ==="
# `make build`, not a hand-rolled go build: this has to exercise the binary the
# Makefile ships, same flags and same version stamp, or the script drifts into
# testing something nobody releases.
if ! make build; then
    echo "FAIL: build failed"
    exit 1
fi

echo ""
echo "=== Test vault: $VAULT ==="
echo ""

# --- Agent capture tests ---
# Each agent asks about a different country's capital. Later we test
# that memories from one agent are retrievable by another (cross-agent recall).

echo "=== Test 1: Claude (inject on) ==="
run_agent "claude (inject on)" claude -p "What is the capital of France? Reply in one word."

echo "=== Test 2: Claude (--no-inject) ==="
run_agent "claude (--no-inject)" --no-inject claude -p "What is the capital of Japan? Reply in one word."

echo "=== Test 3: qwen (Gemini-CLI fork) ==="
run_agent "qwen" qwen --prompt "What is the capital of Germany? Reply in one word."

echo "=== Test 4: Codex ==="
run_agent "codex" codex exec --full-auto "What is the capital of Italy? Reply in one word only, nothing else."

# Give MuninnDB a moment to finish enrichment.
sleep "$SETTLE_SECONDS"

echo "=== Vault status ==="
mcp_call "muninn_status" "{\"vault\":\"$VAULT\"}" | jq '.result.content[0].text' -r | jq .
echo ""

echo "=== Stored memories ==="
MEMORIES=$(mcp_call "muninn_recall" "{\"vault\":\"$VAULT\",\"context\":[\"capital city country\"],\"limit\":50,\"threshold\":0.0}" \
    | jq -r '.result.content[0].text')
echo "$MEMORIES" | jq '.memories[] | {id: .id[0:12], concept: .concept[0:120], score: (.score * 100 | floor / 100)}' 2>/dev/null
echo ""

echo "=== Quality checks ==="
MEM_COUNT=$(echo "$MEMORIES" | jq '.memories | length' 2>/dev/null || echo 0)
echo "Total memories: $MEM_COUNT"

# Every check below counts matches in a memory list, so an empty list (nothing
# captured, or a MuninnDB that never answered) passes all of them vacuously.
# Fail on the empty case first.
if [[ "$MEM_COUNT" -lt 1 ]]; then
    echo "FAIL: no memories were captured or recalled, remaining checks are vacuous"
    ERRORS=$((ERRORS + 1))
    exit $ERRORS
fi

# Check: no system-reminder in concepts or content.
SR_COUNT=$(echo "$MEMORIES" | jq '[.memories[] | select(.content | test("system-reminder"))] | length' 2>/dev/null || echo 0)
if [[ "$SR_COUNT" -gt 0 ]]; then
    echo "FAIL: $SR_COUNT memories contain system-reminder tags"
    ERRORS=$((ERRORS + 1))
else
    echo "PASS: no system-reminder tags in stored memories"
fi

# Check: no HTTP metadata fallback concepts (like "[claude] POST /v1/messages").
HTTP_COUNT=$(echo "$MEMORIES" | jq '[.memories[] | select(.concept | test("^\\["))] | length' 2>/dev/null || echo 0)
if [[ "$HTTP_COUNT" -gt 0 ]]; then
    echo "WARN: $HTTP_COUNT memories use HTTP metadata fallback concepts"
else
    echo "PASS: all concepts use user message text"
fi

# Check: no count_tokens captures.
CT_COUNT=$(echo "$MEMORIES" | jq '[.memories[] | select(.concept | test("count_tokens"))] | length' 2>/dev/null || echo 0)
if [[ "$CT_COUNT" -gt 0 ]]; then
    echo "FAIL: $CT_COUNT memories from count_tokens calls"
    ERRORS=$((ERRORS + 1))
else
    echo "PASS: no count_tokens captures"
fi

# Check: no duplicate concepts.
DUP_COUNT=$(echo "$MEMORIES" | jq '[.memories[].concept] | group_by(.) | map(select(length > 1)) | length' 2>/dev/null || echo 0)
if [[ "$DUP_COUNT" -gt 0 ]]; then
    echo "FAIL: $DUP_COUNT duplicate concept groups"
    ERRORS=$((ERRORS + 1))
else
    echo "PASS: no duplicate concepts"
fi

echo ""

# --- Inter-agent memory retrieval test ---
# Ask Claude about "Germany" — it should recall the qwen memory about Berlin.
echo "=== Test 5: Cross-agent recall (Claude recalling qwen memory) ==="
run_agent "claude (cross-agent recall)" claude -p "What do you know about Germany's capital from your context? Just state what you recall."

echo ""

echo ""
if [[ "$ERRORS" -gt 0 ]]; then
    echo "=== $ERRORS quality check(s) FAILED ==="
else
    echo "=== All quality checks passed ==="
fi
exit $ERRORS
