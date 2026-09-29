#!/bin/sh
# Entrypoint of the dir2mcp image. Any first argument other than "up" runs
# dir2mcp as is (dir2mcp version, dir2mcp status ...). For "up", the config
# comes from the corpus when it carries one, else from the image default.
set -eu
if [ "${1:-}" = "up" ]; then
  shift
  cfg=/etc/dir2mcp/config.yaml
  if [ -f /corpus/.dir2mcp.yaml ]; then
    cfg=/corpus/.dir2mcp.yaml
  fi
  exec dir2mcp up --config "$cfg" "$@"
fi
exec dir2mcp "$@"
