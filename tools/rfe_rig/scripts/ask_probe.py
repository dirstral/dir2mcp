import json
import sys

sys.path.insert(0, "/mnt/data/rfe-val/rig")
from tools.rfe_rig.mcp_client import MCP, read_token  # noqa: E402

m = MCP("http://127.0.0.1:8791/mcp",
        read_token("/mnt/data/rfe-val/retrieval/.dir2mcp/secret.token")).connect()
r = m.call("tools/call", {"name": "dir2mcp_ask",
                          "arguments": {"question": "What is said about Crimea in these recordings?"}})
res = r.get("result") or {}
sc = res.get("structuredContent") or {}
print("keys:", sorted(sc.keys()))
for k, v in sc.items():
    if k == "answer":
        print("answer head:", " ".join(str(v).split())[:500])
        continue
    print(k, "=", json.dumps(v, ensure_ascii=False)[:700])
print("content types:", [c.get("type") for c in res.get("content") or []])
print("isError:", res.get("isError"), "error:", r.get("error"))
