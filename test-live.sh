#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"

# Clear env vars from parent msc/claude session to avoid nested detection.
unset MSC_UPSTREAM ANTHROPIC_BASE_URL CLAUDECODE 2>/dev/null || true

DEBUG=""
if [[ "${1:-}" == "-d" ]]; then
    DEBUG="-d"
fi

VAULT="msc-test-$$"
MCP_URL="http://127.0.0.1:8750/mcp"
TOKEN=$(cat ~/.muninn/mcp.token 2>/dev/null || echo "")
ERRORS=0

mcp_call() {
    local tool="$1"
    local args="$2"
    curl -s "$MCP_URL" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $TOKEN" \
        -d "{\"jsonrpc\":\"2.0\",\"method\":\"tools/call\",\"params\":{\"name\":\"$tool\",\"arguments\":$args},\"id\":1}"
}

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
}
trap cleanup EXIT

echo "=== Building msc ==="
go build -o msc ./cmd/msc/

echo ""
echo "=== Test vault: $VAULT ==="
echo ""

# --- Agent capture tests ---
# Each agent asks about a different country's capital. Later we test
# that memories from one agent are retrievable by another (cross-agent recall).

echo "=== Test 1: Claude (inject on) ==="
./msc $DEBUG --vault "$VAULT" claude -p "What is the capital of France? Reply in one word." 2>&1
echo "--- exit: $? ---"
echo ""

echo "=== Test 2: Claude (--no-inject) ==="
./msc $DEBUG --vault "$VAULT" --no-inject claude -p "What is the capital of Japan? Reply in one word." 2>&1
echo "--- exit: $? ---"
echo ""

echo "=== Test 3: Gemini ==="
./msc $DEBUG --vault "$VAULT" gemini --prompt "What is the capital of Germany? Reply in one word." 2>&1
echo "--- exit: $? ---"
echo ""

echo "=== Test 4: Codex ==="
./msc $DEBUG --vault "$VAULT" codex exec --full-auto "What is the capital of Italy? Reply in one word only, nothing else." 2>&1
echo "--- exit: $? ---"
echo ""

# Give MuninnDB a moment to finish enrichment.
sleep 2

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
# Ask Claude about "Germany" — it should recall the Gemini memory about Berlin.
echo "=== Test 5: Cross-agent recall (Claude recalling Gemini memory) ==="
./msc $DEBUG --vault "$VAULT" claude -p "What do you know about Germany's capital from your context? Just state what you recall." 2>&1
echo "--- exit: $? ---"
echo ""

echo ""
if [[ "$ERRORS" -gt 0 ]]; then
    echo "=== $ERRORS quality check(s) FAILED ==="
else
    echo "=== All quality checks passed ==="
fi
