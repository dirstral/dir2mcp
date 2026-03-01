# ECOSYSTEM.md

## dir2mcp in the agent web stack

### Summary
`dir2mcp` is the deployment primitive that turns private data (a directory) into a standard **MCP tool server**. The broader ecosystem vision is that MCP endpoints become **composable services** on an “agent web,” and x402 enables machine-payable access to those services.

This document describes the ecosystem layer around `dir2mcp`: discovery, trust, metering, and payments—with an optional **native x402 mode** in `dir2mcp` and optional external gateway/marketplace layers.

---

## 1) The emerging “agent web” model

The web standardized documents and links. The agent web standardizes:
- tool servers
- knowledge servers
- small specialized APIs

MCP provides a common protocol for agents to:
- discover tools
- call tools with typed schemas
- receive structured outputs and artifacts

In this model, a privately hosted MCP server becomes a reusable “service node” that an agent can incorporate into plans.

---

## 2) Where dir2mcp fits

`dir2mcp` produces a single deployable node:
- local-first indexing over a directory
- multimodal normalization (PDF/image OCR, audio transcription, optional structured extraction)
- safe, verifiable retrieval and answering
- an MCP endpoint with a small stable tool surface

`dir2mcp` intentionally does **not** do:
- marketplace listing
- billing portals
- user accounts
- complex identity systems

It focuses on:
- fast deployment
- correct MCP compliance
- safe-by-default exposure
- verifiable outputs

That constraint is what keeps it minimal and broadly usable.

---

## 3) The economic layer: native x402 with facilitator-backed settlement

### Why payments matter
If MCP endpoints can be deployed easily, the next bottleneck is: how can third parties safely expose valuable services to unknown consumers?

A pay-per-call or metered model:
- aligns incentives (providers get paid, consumers pay for value)
- reduces friction compared to long contracts/subscriptions
- supports ephemeral / programmatic usage by agents

### The key design principle
Payments should be protocol-native in `dir2mcp` while still decoupled from retrieval internals:
- enforce x402 at the HTTP/MCP boundary (request gating)
- use x402 v2 HTTP headers (`PAYMENT-REQUIRED`, `PAYMENT-SIGNATURE`, `PAYMENT-RESPONSE`)
- delegate verify/settle to a facilitator (`/v2/x402/verify`, `/v2/x402/settle`)
- use CAIP-2 network IDs in all payment requirements (for example `eip155:8453`)
- keep indexing/retrieval/answering logic payment-agnostic

This keeps the product simple for users (single binary deployment) without coupling core RAG logic to chain-specific code.

---

## 4) Reference ecosystem architecture

### Minimal components
1) **Provider node**: a dir2mcp server running on private infrastructure (with optional native x402 mode)  
2) **Facilitator**: verifies and settles x402 payments for paid calls (hosted or self-managed)  
3) **Gateway (optional)**: adds org-level policy/rate limits in front of one or more nodes  
4) **Marketplace (optional)**: discovery, listing metadata, reputation, pricing visibility  
5) **Agent client**: connects to endpoints and composes tools

### Request path
- Agent calls MCP endpoint
- If paid mode is enabled:
  - dir2mcp returns `402 Payment Required` with `PAYMENT-REQUIRED`
  - client retries with `PAYMENT-SIGNATURE`
  - dir2mcp verifies/settles via facilitator and then serves MCP response
  - server may include settlement details via `PAYMENT-RESPONSE`
- dir2mcp returns standard MCP response

This enables:
- native paywalling without mandatory extra infrastructure
- optional multi-node policy control via gateway when needed

### Hosted demo operational smoke check

For hosted endpoints, run `scripts/smoke_hosted_demo.sh` as a minimal runbook probe before sharing an endpoint:
- validates `initialize` and session issuance
- validates `tools/list` schema surface
- validates `tools/call` reachability (`200 OK`) or expected x402 gating (`402 Payment Required` with `PAYMENT-REQUIRED`)

---

## 5) What makes a “sellable” MCP knowledge service

A market-ready endpoint needs more than retrieval quality.

### Required characteristics
- **Stable schemas**: tool inputs/outputs are consistent over time  
- **Predictable performance**: bounded latency, graceful degradation while indexing  
- **Clear provenance**: citations, source slices, and verifiable references  
- **Access control**: token auth, rotation, allowlists  
- **Isolation**: strict root boundary, no traversal or symlink escapes  
- **Abuse resistance**: rate limiting, request caps, concurrency caps  
- **Metering hooks**: per-call counters, payload byte counts, optional usage estimates  
- **Multimodal support**: PDFs/audio/images are normalized into text representations but keep provenance (page/time/source)

`dir2mcp` should provide most of these natively, including optional x402 payment enforcement, because they’re useful even without a marketplace.

---

## 6) Discovery and metadata (marketplace-ready descriptors)

A marketplace needs a standard way to describe endpoints.

In x402 v2, Bazaar discovery is treated as an extension layer: facilitator services index extension metadata and expose it through discovery endpoints (for example `GET {facilitator_url}/discovery/resources`).

For concreteness, the Bazaar/CDP discovery metadata specification (https://docs.cdp.coinbase.com/x402/bazaar) defines a JSON Schema for the descriptor payload returned by that endpoint. The schema covers the sections listed below – service identity, capabilities, operational guarantees, policy, metering and trust – and is the authoritative reference implementers should follow. A minimal illustrative snippet looks like:

```json
{
  "$schema": "https://json-schema.org/draft-07/schema#",
  "title": "MCP Endpoint Descriptor",
  "type": "object",
  "properties": {
    "identity": {
      "type": "object",
      "properties": {
        "name": {"type": "string"},
        "description": {"type": "string"},
        "version": {"type": "string"},
        "support_url": {"type": "string", "format": "uri"}
      },
      "required": ["name","description"]
    },
    "capabilities": {
      "type": "object",
      "properties": {
        "tools": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "name": {
                "type": "string",
                "description": "The unique name of the MCP tool"
              },
              "schema": {
                "type": "object",
                "description": "JSON Schema describing the tool's input parameters"
              }
            },
            "required": ["name", "schema"]
          }
        },
        "source_types": {"type": "array","items": {"type":"string"}},
        "citation_formats": {"type": "array","items": {"type":"string"}}
      }
    },
    "operational_guarantees": {
      "type": "object",
      "properties": {
        "max_request_size": {"type": "integer"},
        "max_concurrency": {"type": "integer"},
        "latency_ms": {"type": "object","properties":{"p50":{"type":"integer"},"p95":{"type":"integer"}}}
      }
    },
    "policy": {
      "type": "object",
      "properties": {
        "export_restrictions": {"type":"string"},
        "log_retention_days": {"type":"integer"}
      }
    },
    "metering": {
      "type": "object",
      "properties": {
        "billing_unit": {"type":"string"},
        "price_schedule": {"type":"string"}
      }
    },
    "trust": {
      "type": "object",
      "properties": {
        "schema_hash": {"type":"string"},
        "signature": {"type":"string"}
      }
    }
  },
  "required": ["identity","capabilities"]
}
```

> **Note:** This snippet is a **simplified illustrative example** only and should not be treated as authoritative. Implementers must consult the full JSON Schema at https://docs.cdp.coinbase.com/x402/bazaar for all required fields and validation rules.

### Suggested endpoint descriptor fields
- Service identity:
  - name, description, version
  - operator contact / support URL
- Capabilities:
  - tool list and schemas
  - supported source types (code, text, pdf, audio transcript, structured)
  - citation formats (line/page/time)
- Operational guarantees:
  - max request size
  - max concurrency
  - typical latency ranges (optional)
- Policy:
  - export restrictions
  - retention (how long logs are kept)
- Metering:
  - billing unit (per call / per KB / per minute)
  - price schedule
- Trust:
  - signatures/attestation (future)
  - audit hash of tool schemas (future)

A good future direction is for `dir2mcp` to generate an endpoint descriptor automatically from a running server, and the generated manifest SHOULD conform to the Bazaar/CDP discovery metadata schema described above.

---

## 7) Trust and reputation (the hard problem)

A marketplace of agent services will fail without trust.

### Provider-side concerns
- accidental data leakage
- prompt injection via retrieved content
- abusive scraping and bulk export attempts

### Consumer-side concerns
- malicious endpoints returning poisoned outputs
- fake citations
- undisclosed transformations

### Practical trust steps (incremental)
- Always provide verifiable citations
- Provide `open_file` (or “source slice”) tools for inspection
- Implement strict root isolation + explicit allowlist patterns
- Publish schemas + hash/sign them (future)
- Provide transparent metering + optional logs

The trust layer can evolve independently of `dir2mcp` as long as the protocol boundary stays stable.

---

## 8) Suggested roadmap for ecosystem readiness (without bloating dir2mcp)

### Phase 1: deployable node
- correct MCP compliance
- stable tool schemas
- safe-by-default server exposure
- provenance + inspection tools
- multimodal normalization into RAG
- optional native x402 paid mode (facilitator-backed)

### Phase 2: marketplace-friendly node
- generate endpoint descriptor (manifest)
- add metering counters and limits
- add audit logging options

### Phase 3: paid node
- publish a turnkey paid deployment recipe (dir2mcp native x402 + facilitator)
- provide optional gateway recipe for multi-node policy enforcement
- add pricing hints in descriptor (compatible with Bazaar discovery metadata)

`dir2mcp` can remain a minimal binary throughout.

---

## 9) Design rules (to avoid ecosystem dead-ends)

1) **Never fork MCP semantics**  
Do not invent custom call/response formats; remain interoperable.

2) **Keep payment boundaries clean**  
Prefer native x402 enforcement in dir2mcp; keep facilitator and optional gateway concerns at the edge.

3) **Prefer retrieval-first**  
Allow “answering” to be optional; retrieval is the core service.

4) **Verifiable by default**  
Citations + source access are prerequisites for trust.

5) **Composable over complete**  
A node should do one thing well: serve knowledge from this directory.

---

## 10) What success looks like

### Near term
- People deploy private knowledge servers in minutes.
- Agents connect without custom adapters.
- Endpoints are safe, verifiable, and reliable.
- Multimodal corpora are supported via normalized text representations.

### Long term
- A marketplace indexes specialized MCP endpoints.
- Agents dynamically select services based on capability + cost + trust.
- x402-style payments make it feasible to buy/sell access programmatically.
- The agent web becomes a network of composable, interoperable services.

`dir2mcp` is the deployable building block that makes that network possible.
