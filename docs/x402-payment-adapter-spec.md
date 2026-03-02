# x402 Payment Adapter Specification

This document defines the **x402 payment adapter** contract referenced from `VISION.md`. The adapter sits between a dir2mcp node and a third-party facilitator to enable optional HTTP 402 gating on selected MCP routes while keeping retrieval internals payment-agnostic.

> **x402 request gating** is a lightweight HTTP standard (originating from Coinbase's x402 project) that lets a server require clients to present payment proofs before processing certain requests. dir2mcp uses this mechanism to optionally throttle or monetize access to sensitive MCP endpoints without hard‑coding any particular payment network.

## Purpose

Provide a clear, versioned contract so that:

* dir2mcp can emit and consume payment requirements without embedding blockchain logic.
* Facilitator providers can be swapped via configuration.
* Discovery and billing layers can interoperate with any compliant adapter.

## Usage in dir2mcp

When x402 gating is enabled (`--x402` flag in the CLI or `x402.mode` in config), dir2mcp invokes the adapter at the MCP request boundary (typically `POST /mcp` for `tools/call`, or selected tool names). Depending on the facilitator's response, the server either allows the request to proceed or returns `402 Payment Required` with `PAYMENT-REQUIRED` header data. Clients must then obtain and attach `PAYMENT-SIGNATURE` before retrying.
### Example configuration snippet

```yaml
# config.yaml
x402:
  mode: required            # off, on, required
  adapter: "coinbase"      # named adapter from internal/x402
  facilitator:
    api_key: "..."         # credentials for the external service
```

> **Security note:** never commit API keys or sensitive credentials (such as
> `x402.facilitator.api_key`) to version control.
> Store them securely using environment variables, a secret manager (e.g.
> HashiCorp Vault, AWS Secrets Manager), or encrypted configuration files.
## Normative baseline

The adapter MUST align with x402 v2 concepts and headers:

* `PAYMENT-REQUIRED` for payment challenges (`HTTP 402`)
* `PAYMENT-SIGNATURE` for client payment proof
* `PAYMENT-RESPONSE` for settlement/receipt metadata

Reference materials:

* x402 spec repository: <https://github.com/coinbase/x402/tree/main/specs>
* Facilitator API reference: <https://docs.cdp.coinbase.com/api-reference/v2/rest-api/x402-facilitator/x402-facilitator>
* Core concepts and migration notes:
	* <https://docs.cdp.coinbase.com/x402/core-concepts/how-it-works>
	* <https://docs.cdp.coinbase.com/x402/core-concepts/http-402>
	* <https://docs.cdp.coinbase.com/x402/migration-guide>

## Adapter contract

The contract defines, at a minimum, the following elements:  

* **Facilitator operations** – call facilitator verify and settle endpoints and map their responses into dir2mcp transport behavior. Adapters must implement verify (to validate payment proofs) and settle (to finalize payments) operations according to their facilitator's API specification.
* **Authentication** – adapter-to-facilitator auth must be explicit (for example API key auth for hosted facilitator, mTLS or signed requests for self-managed deployments).  
* **Payment state model** – canonical states `required -> verified -> settled` with failure branches (`invalid`, `rejected`, `expired`, `failed`). dir2mcp does not persist custodial payment state; facilitator is source of truth for verify/settle outcomes.  
* **Error codes and retries** – standard HTTP handling (`402`, `4xx`, `5xx`), idempotent settle calls, bounded retry/backoff for transient failures, and explicit non-retryable classes for invalid signatures/requirements mismatch.  
* **Network normalization** – adapters must use CAIP-2 network identifiers at all payment-related boundaries.  Examples include:
  * `eip155:8453`
  * `eip155:84532`
  * Solana mainnet: `solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d`
  * Solana devnet: `solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG`
  * Solana testnet: `solana:4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY`.  
* **Discovery passthrough (optional)** – if Bazaar is enabled, expose extension metadata to facilitator discovery ingestion rather than implementing a custom discovery protocol in dir2mcp.  

## Out of scope

This adapter does not define:

* wallet key management,
* custodial fund handling,
* marketplace ranking/reputation logic,
* non-x402 billing schemes.

---

> **Note:** this document is paired with the global MCP [SPEC.md](../SPEC.md). whenever the
> protocol version, message formats, field definitions, or error codes evolve you must keep
> both documents in sync. reviewers should use the following checklist when making changes:
>
> 1. bump the protocol version in **both** specs
> 2. reconcile any example request/response messages and payload schemas
> 3. update compatibility/upgrade notes or migration guidance in both places
> 4. verify field definitions (names, types, required/optional semantics) match
> 5. refresh error code tables and retry logic descriptions
>
> Failing to update either doc can lead to incompatible implementations, so include a
> comment in your PR pointing to the related edits.
