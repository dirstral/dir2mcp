# Fixture for assets/demo/demo.tape. The tape sources this file from the repo
# root while the recording is hidden. Do not run it on its own.
#
# It makes a throwaway HOME and a small notes folder under /tmp, writes the
# local Ollama config from the README, and puts the repo build of dir2mcp
# first on PATH. Override the model endpoint with DEMO_OLLAMA_URL and the chat
# model with DEMO_CHAT_MODEL.

DEMO_REPO="$(pwd)"
DEMO_ROOT="$(mktemp -d /tmp/dir2mcp-demo.XXXXXX)"
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

# demo_wait blocks until the daemon has embedded every chunk.
demo_wait() {
  local s
  for _ in $(seq 1 120); do
    s="$(dir2mcp --json status 2>/dev/null)"
    case "$s" in
      *'"running":false'*'"embedded_pending":0,'*)
        case "$s" in *'"chunks_total":0,'*) ;; *) return 0 ;; esac ;;
    esac
    sleep 1
  done
  echo "demo_wait: indexing did not finish" >&2
  return 1
}
