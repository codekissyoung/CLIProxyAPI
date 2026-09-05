#!/usr/bin/env bash
# Pinned Codex Responses probe.
#
# Required by AGENTS.md after any merge touching Codex transport code: prove
# that a deployed CLIProxyAPI host can still complete a streaming Codex
# Responses call end-to-end (uTLS handshake → chatgpt.com/backend-api/codex →
# terminal stream event).
#
# Usage:
#   CLIPROXY_API_KEY=<proxy-api-key> ./scripts/codex_responses_probe.sh [base-url] [model]
#
# Defaults: base-url http://127.0.0.1:8317, model gpt-5.
# Exit codes: 0 = probe passed; 1 = usage/config error; 2 = HTTP failure;
# 3 = stream ended without a terminal Responses event.

set -euo pipefail

BASE_URL="${1:-http://127.0.0.1:8317}"
MODEL="${2:-gpt-5}"
API_KEY="${CLIPROXY_API_KEY:-}"

if [[ -z "$API_KEY" ]]; then
  echo "error: CLIPROXY_API_KEY env var (a key from the target's api-keys) is required" >&2
  exit 1
fi

body="$(mktemp)"
trap 'rm -f "$body"' EXIT

http_code="$(curl -sS -o "$body" -w '%{http_code}' --max-time 120 \
  -X POST "$BASE_URL/v1/responses" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"input\":\"Reply with the single word: pong\",\"stream\":true,\"max_output_tokens\":16}")"

if [[ "$http_code" != "200" ]]; then
  echo "FAIL: HTTP $http_code from $BASE_URL/v1/responses" >&2
  head -c 500 "$body" >&2
  exit 2
fi

if grep -qE '"type"\s*:\s*"response\.(completed|done|incomplete)"' "$body"; then
  echo "OK: $BASE_URL completed a streaming Codex Responses call (model=$MODEL)"
  exit 0
fi

echo "FAIL: stream ended without a terminal Responses event" >&2
tail -c 500 "$body" >&2
exit 3
