#!/usr/bin/env bash
set -euo pipefail

URL="${DIR2MCP_DEMO_URL:-}"
TOKEN="${DIR2MCP_DEMO_TOKEN:-}"
PROTOCOL_VERSION="${DIR2MCP_PROTOCOL_VERSION:-2025-11-25}"

if [[ -z "${URL}" ]]; then
  echo "error: set DIR2MCP_DEMO_URL to the full MCP endpoint URL (for example https://host.example/mcp)" >&2
  exit 2
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

auth_headers=()
if [[ -n "${TOKEN}" ]]; then
  auth_headers=(-H "Authorization: Bearer ${TOKEN}")
fi

post_json() {
  local body="$1"
  local session_id="${2:-}"

  local headers_file="${tmp_dir}/headers.txt"
  local body_file="${tmp_dir}/body.json"
  local -a headers
  headers=(
    -H "Content-Type: application/json"
    -H "Accept: application/json, text/event-stream"
  )
  if [[ -n "${session_id}" ]]; then
    headers+=(
      -H "MCP-Protocol-Version: ${PROTOCOL_VERSION}"
      -H "MCP-Session-Id: ${session_id}"
    )
  fi

  local http_code
  http_code="$(
    curl -sS -o "${body_file}" -D "${headers_file}" -w "%{http_code}" \
      -X POST "${URL}" \
      "${headers[@]}" \
      "${auth_headers[@]}" \
      --data "${body}"
  )"

  printf '%s\n%s\n%s\n' "${http_code}" "${headers_file}" "${body_file}"
}

extract_result_line() {
  local value="$1"
  local line_no="$2"
  printf '%s\n' "${value}" | sed -n "${line_no}p"
}

echo "[1/3] initialize"
init_payload='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"clientInfo":{"name":"smoke-hosted-demo","version":"0.1.0"}}}'
init_result="$(post_json "${init_payload}")"
init_code="$(extract_result_line "${init_result}" 1)"
init_headers="$(extract_result_line "${init_result}" 2)"
init_body="$(extract_result_line "${init_result}" 3)"

if [[ "${init_code}" != "200" ]]; then
  echo "error: initialize failed with HTTP ${init_code}" >&2
  cat "${init_body}" >&2
  exit 1
fi

session_id="$(sed -nE '/^[[:space:]]*[Mm][Cc][Pp]-[Ss]ession-[Ii]d:/ { s/^[[:space:]]*[Mm][Cc][Pp]-[Ss]ession-[Ii]d:[[:space:]]*(.*)[[:space:]]*$/\1/; p; q; }' "${init_headers}" | tr -d '\r')"
if [[ -z "${session_id}" ]]; then
  echo "error: initialize succeeded but MCP-Session-Id header is missing" >&2
  exit 1
fi
if ! grep -q '"jsonrpc"' "${init_body}"; then
  echo "error: initialize response is not valid JSON-RPC" >&2
  cat "${init_body}" >&2
  exit 1
fi

echo "[2/4] notifications/initialized"
initialized_payload='{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}'
initialized_result="$(post_json "${initialized_payload}" "${session_id}")"
initialized_code="$(extract_result_line "${initialized_result}" 1)"
initialized_body="$(extract_result_line "${initialized_result}" 3)"
if [[ "${initialized_code}" != "202" && "${initialized_code}" != "200" && "${initialized_code}" != "204" ]]; then
  echo "error: notifications/initialized failed with HTTP ${initialized_code}" >&2
  cat "${initialized_body}" >&2
  exit 1
fi

echo "[3/4] tools/list"
list_payload='{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
list_result="$(post_json "${list_payload}" "${session_id}")"
list_code="$(extract_result_line "${list_result}" 1)"
list_body="$(extract_result_line "${list_result}" 3)"

if [[ "${list_code}" != "200" ]]; then
  echo "error: tools/list failed with HTTP ${list_code}" >&2
  cat "${list_body}" >&2
  exit 1
fi
if ! grep -q '"tools"' "${list_body}"; then
  echo "error: tools/list response does not include tools payload" >&2
  cat "${list_body}" >&2
  exit 1
fi

echo "[4/4] tools/call dir2mcp.list_files"
call_payload='{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"dir2mcp.list_files","arguments":{"limit":1}}}'
call_result="$(post_json "${call_payload}" "${session_id}")"
call_code="$(extract_result_line "${call_result}" 1)"
call_headers="$(extract_result_line "${call_result}" 2)"
call_body="$(extract_result_line "${call_result}" 3)"

if [[ "${call_code}" == "200" ]]; then
  if ! grep -q '"jsonrpc"' "${call_body}"; then
    echo "error: tools/call returned HTTP 200 but response is not JSON-RPC" >&2
    cat "${call_body}" >&2
    exit 1
  fi
elif [[ "${call_code}" == "402" ]]; then
  if ! grep -qi '^PAYMENT-REQUIRED:' "${call_headers}"; then
    echo "error: tools/call returned 402 without PAYMENT-REQUIRED header" >&2
    cat "${call_body}" >&2
    exit 1
  fi
  echo "note: tools/call is payment-gated (x402), initialize and tools/list are healthy"
else
  echo "error: tools/call failed with unexpected HTTP ${call_code}" >&2
  cat "${call_body}" >&2
  exit 1
fi

echo "ok: hosted demo smoke checks passed for ${URL}"
