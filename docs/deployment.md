# Deploy dir2mcp behind a tunnel or reverse proxy

How to serve dir2mcp beyond `127.0.0.1`: a smoke test for a hosted endpoint,
Cloudflare and ngrok tunnels, and the `Host` header that every reverse proxy
must send. For a GPU host with the corpus on NFS or S3, see
[dual-machine deployment](dual-machine-deployment.md).

## Hosted demo smoke runbook

For a quick hosted readiness check (issue #19 scope), run:

```bash
DIR2MCP_DEMO_URL="https://your-host.example/mcp" \
DIR2MCP_DEMO_TOKEN="<optional-bearer-token>" \
./scripts/smoke_hosted_demo.sh
```

Notes:
- `DIR2MCP_DEMO_TOKEN` is required whenever auth is enabled.
- The script now runs the full MCP init sequence (`initialize` -> `notifications/initialized` -> `tools/list` -> `tools/call`).
- If your endpoint is x402-gated, `tools/call` returning HTTP `402` with `PAYMENT-REQUIRED` is treated as healthy.

What it verifies:
- `initialize` returns HTTP 200 and a valid `MCP-Session-Id`
- `tools/list` returns HTTP 200 with tool metadata
- `tools/call` for `dir2mcp_list_files` returns HTTP 200, or HTTP 402 with `PAYMENT-REQUIRED` when x402 is enabled

## Tunnel setup (copy/paste)

> **Port note:** `dir2mcp up` binds a **random** loopback port by default (`127.0.0.1:0`) and prints its actual MCP URL on startup — it is not `:8087`. Either substitute the printed port in the commands below, or pin a fixed port first with `dir2mcp up --listen 127.0.0.1:8087` (or `listen_addr: 127.0.0.1:8087` in `.dir2mcp.yaml`). The `:8087` below is a placeholder for whichever local port your server is actually listening on.

Cloudflare quick tunnel (no account-required quick mode):

```bash
cloudflared tunnel --url http://127.0.0.1:<PORT> --no-autoupdate \
  --http-host-header 127.0.0.1:<PORT>
```

> **`--http-host-header` is required, not optional.** The MCP SDK auto-enables
> DNS-rebinding protection for a loopback-bound server, and then refuses any
> request whose `Host` header is not a loopback name. A tunnel forwards the
> PUBLIC hostname by default, so without this flag **every request returns 403**
> and the body does not say why. The flag makes the forwarded `Host` truthful and
> keeps the protection ON. Do not turn the protection off instead. Verified on a
> live deployment (issue #853). For the full explanation, for nginx, Caddy and
> Traefik, and for what each alternative costs, read
> [Reverse proxy and tunnel: the `Host` header](#reverse-proxy-and-tunnel-the-host-header).

A quick tunnel gets a NEW random hostname every time it starts, including after a
reboot, so treat the URL as ephemeral and re-read it from the running process
rather than from an earlier log line.

ngrok (requires verified account + authtoken):

```bash
ngrok config add-authtoken <YOUR_NGROK_TOKEN>
ngrok http http://127.0.0.1:<PORT> --host-header=127.0.0.1:<PORT>
```

Get ngrok public URL from local API:

```bash
curl -sS http://127.0.0.1:4040/api/tunnels \
  | jq -r '.tunnels[] | select(.proto=="https") | .public_url'
```

If you do not have `jq`, copy the public URL from the ngrok web UI (`http://127.0.0.1:4040`).

Then run the hosted smoke probe against either tunnel URL:

```bash
DIR2MCP_DEMO_URL="https://<public-url>/mcp" \
DIR2MCP_DEMO_TOKEN="$(cat .dir2mcp/secret.token)" \
./scripts/smoke_hosted_demo.sh
```

## Reverse proxy and tunnel: the `Host` header

**Symptom.** The server starts, the token is correct, and every request through the proxy fails, including `initialize`:

```text
403 Forbidden: invalid Host header "example.trycloudflare.com"
```

**Cause.** The MCP Go SDK enables DNS-rebinding protection for a connection that arrives on a loopback address. It then refuses any request whose `Host` header is not a loopback name. The guard is correct and should stay on: a browser page loaded from a public name that reaches a private loopback server is exactly the attack it stops. A reverse proxy has the same shape as that attack, and the guard cannot tell the two apart, because a proxy forwards the public hostname by default.

The default `listen_addr` is `127.0.0.1:0`, a loopback address, so every operator who puts a proxy in front of the default configuration meets this on the first request.

**Recommended fix: make the forwarded `Host` truthful.** Tell the proxy to send the address it really connects to. The protection stays on. Substitute your own port for `8087` in every row.

| Proxy | Setting |
|---|---|
| cloudflared (`--url` mode) | `--http-host-header 127.0.0.1:8087` |
| cloudflared (ingress rules) | `originRequest.httpHostHeader: 127.0.0.1:8087` |
| ngrok | `--host-header=127.0.0.1:8087` (deprecated by ngrok; see the traffic policy below) |
| nginx | `proxy_set_header Host 127.0.0.1:8087;` |
| Caddy | `reverse_proxy 127.0.0.1:8087 { header_up Host {upstream_hostport} }` |
| Traefik | `passHostHeader: false` on the service load balancer |

The cloudflared `--url` row is measured end to end against a quick tunnel. The other rows come from each proxy's own documentation and are not measured here.

The cloudflared flag applies only to `--url` mode. With ingress rules, set the same value under `originRequest` instead:

```yaml
ingress:
  - hostname: mcp.example.com
    service: http://127.0.0.1:8087
    originRequest:
      httpHostHeader: 127.0.0.1:8087
  - service: http_status:404
```

ngrok marks `--host-header` deprecated and points at a traffic policy instead. The `add-headers` action treats `host` as a replacement, not an addition:

```yaml
# ngrok traffic policy
on_http_request:
  - actions:
      - type: "add-headers"
        config:
          headers:
            host: "127.0.0.1:8087"
```

The guard accepts the literal name `localhost` and any loopback IP literal, such as `127.0.0.1` or `[::1]`. It does not compare the port. It does not resolve names, so a DNS name that points at `127.0.0.1` is still refused. Send the real address, so the value stays honest in logs and after a port change.

**Alternative: bind a non-loopback address.** The guard runs only when the connection arrives on a loopback address. Start the server on a routable address, then point the proxy at that address:

```bash
dir2mcp up --listen 0.0.0.0:8087
# point the proxy at http://<host-lan-ip>:8087, NOT at http://127.0.0.1:8087
```

`--public` sets `0.0.0.0` for you, but it does not fix this on its own. A proxy that still connects to `127.0.0.1` still arrives on a loopback address, and the guard still refuses it. The trade is that the port becomes reachable from the whole network: keep auth on (`--public` requires auth unless `--force-insecure` is set) and restrict the port with a firewall.

**Not recommended: turn the guard off.** The SDK reads `MCPGODEBUG=disablelocalhostprotection=1` and then skips the check for the whole process. dir2mcp has no setting of its own for it. This switch is separate from `disablecrossoriginprotection`, which controls the `Origin` and `Content-Type` checks, so it turns off the `Host` check only. Know the trade before you use it: the server then accepts any `Host` from anywhere, so a page in a browser on the same machine can reach the endpoint through a rebound DNS name. The bearer token is the only thing that stops that page, which makes `--auth none` (or a leaked token) an open door. The SDK also marks the switch temporary and plans to remove it (the pinned v1.4.1 says v1.6.0; upstream has since postponed the removal to v1.8.0), so a deployment that depends on it breaks at a future SDK update. Fix the proxy instead.

**These settings do not help, in spite of the names.**

- `allowed_origins` lists the browser `Origin` values this server accepts (CORS and CSRF). It never reads the `Host` header.
- `trusted_proxies` lists the CIDRs whose `X-Forwarded-For` the rate limiter may believe. It never reads the `Host` header.
