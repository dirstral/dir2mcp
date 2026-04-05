#!/usr/bin/env bash
# inspector-smoke.sh — headless MCP inspector smoke test for dir2mcp.
#
# Usage: scripts/inspector-smoke.sh <binary> <corpus-dir>
#
# Starts dir2mcp up against the given corpus, waits for the server to be
# ready (polling via HTTP), runs two inspector CLI calls (tools/list and
# tools/call list_files), then stops the server.  Exits non-zero on any
# failure.

set -euo pipefail

BINARY="${1:?first arg must be path to dir2mcp binary}"
CORPUS="${2:?second arg must be path to corpus directory}"

# Allow callers to override the port; otherwise pick an available local port.
choose_port() {
  if [[ -n "${MCP_PORT:-}" ]]; then
    printf '%s\n' "$MCP_PORT"
    return
  fi

  python3 - <<'PY'
import socket

sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.bind(("127.0.0.1", 0))
print(sock.getsockname()[1])
sock.close()
PY
}

PORT="$(choose_port)"
MCP_URL="http://127.0.0.1:${PORT}/mcp"

SERVER_PID=""

cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

# A fake API key satisfies the startup check; the smoke test never actually
# calls Mistral.  Real embeddings are not needed because we run --read-only.
export MISTRAL_API_KEY="${MISTRAL_API_KEY:-smoke-test}"

# Start the server in the background.  Global flags (--dir, --non-interactive,
# --quiet) must appear before the subcommand; up-specific flags follow.
"$BINARY" \
  --dir "$CORPUS" \
  --non-interactive \
  --quiet \
  up \
  --listen "127.0.0.1:${PORT}" \
  --auth none \
  --read-only \
  &
SERVER_PID=$!

echo "[smoke] dir2mcp pid=${SERVER_PID} url=${MCP_URL}"

# Poll until the server accepts an HTTP connection or the timeout elapses.
# We probe with a minimal initialize request; a 200-range or 400-range JSON
# response both confirm the server is up.
TIMEOUT=10
elapsed=0
until curl -sf -o /dev/null --max-time 1 \
      -X POST "$MCP_URL" \
      -H "Content-Type: application/json" \
      -d '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}' 2>/dev/null; do
  if [[ $elapsed -ge $TIMEOUT ]]; then
    echo "[smoke] ERROR: server did not become ready within ${TIMEOUT}s" >&2
    exit 1
  fi
  sleep 1
  elapsed=$((elapsed + 1))
done

echo "[smoke] server ready after ${elapsed}s"

# Run the inspector in CLI mode.  The first positional argument is the server
# URL; --method selects the JSON-RPC method.
run_inspector() {
  local label="$1"; shift
  echo "[smoke] ${label}"
  npx --yes @modelcontextprotocol/inspector --cli "$MCP_URL" "$@"
}

run_inspector "tools/list"      --method tools/list
run_inspector "list_files call" --method tools/call --tool-name dir2mcp.list_files

echo "[smoke] all checks passed"
