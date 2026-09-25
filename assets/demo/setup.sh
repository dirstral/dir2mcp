# Fixture for assets/demo/demo.tape. The tape sources this file from the repo
# root while the recording is hidden. Do not run it on its own.
#
# It makes a throwaway HOME and a small notes folder under /tmp, writes the
# local Ollama config from the README, and puts the repo build of dir2mcp
# first on PATH. Override the model endpoint with DEMO_OLLAMA_URL and the chat
# model with DEMO_CHAT_MODEL.

DEMO_REPO="$(pwd)"
# `make demo` creates DEMO_ROOT and removes it on exit, also when the
# recording fails. A direct `vhs` run makes its own.
DEMO_ROOT="${DEMO_ROOT:-$(mktemp -d /tmp/dir2mcp-demo.XXXXXX)}"
export HOME="$DEMO_ROOT/home"
mkdir -p "$HOME"
export PATH="$DEMO_REPO:$PATH"
export PS1='$ '
cp -R "$DEMO_REPO/assets/demo/notes" "$DEMO_ROOT/notes"
cd "$DEMO_ROOT/notes" || return 1

cat > .dir2mcp.yaml <<EOF
providers:
  local:
    kind: openai
    base_url: ${DEMO_OLLAMA_URL:-http://127.0.0.1:11434/v1}
    embed_text_model: nomic-embed-text
    embed_code_model: nomic-embed-text
    chat_model: ${DEMO_CHAT_MODEL:-qwen2.5:7b}
model:
  embed: {provider: local}
  chat: {provider: local}
EOF

# demo_wait blocks until indexing has stopped and EVERY chunk is embedded
# (embedded_ok == chunks_total > 0). embedded_pending=0 alone is not enough: a
# chunk whose embedding failed is not pending either, and the question could
# then miss its note. It gives up after 60 s; the tape waits longer than that.
demo_wait() {
  for _ in $(seq 1 60); do
    if dir2mcp --json status 2>/dev/null | python3 -c '
import json, sys
ix = json.load(sys.stdin).get("snapshot", {}).get("indexing", {})
total = ix.get("chunks_total", 0)
sys.exit(0 if ix.get("running") is False and total > 0 and ix.get("embedded_ok") == total else 1)
'; then
      return 0
    fi
    sleep 1
  done
  echo "demo_wait: indexing did not finish" >&2
  return 1
}
