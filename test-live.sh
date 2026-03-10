#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

# Clear env vars from parent msc/claude session to avoid nested detection.
unset MSC_UPSTREAM ANTHROPIC_BASE_URL CLAUDECODE 2>/dev/null || true

go build -o msc ./cmd/msc/

echo "=== Test 1: Claude capture only ==="
./msc -d claude -p "What is 2+2? Reply with just the number." 2>&1
echo

echo "=== Test 2: Claude with inject ==="
./msc -d --inject claude -p "What is 2+2? Reply with just the number." 2>&1
echo

echo "=== Test 3: Gemini capture only ==="
./msc -d gemini -p "What is 2+2? Reply with just the number." 2>&1
echo

echo "=== Test 4: Gemini with inject ==="
./msc -d --inject gemini -p "What is 2+2? Reply with just the number." 2>&1
echo

echo "=== All tests done ==="
