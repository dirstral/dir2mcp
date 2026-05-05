# Rollout Draft: Issue #146 (Install Tracks + Provider Modes)

Status date: 2026-05-06  
Scope: documentation draft only. Implementation is in progress.

## 1) Dual Install Tracks

Current state:
- `dir2mcp` is the primary install target and current stable track.

Planned:
- `dir2mcp` track: minimal/default install for standard MCP serving.
- `dir2mcp-full` track: expanded install profile for users who want optional provider integrations and heavier ingestion dependencies pre-bundled.

Rollout intent:
- Keep `dir2mcp` as the lowest-friction default.
- Offer `dir2mcp-full` for users who explicitly choose broader provider/dependency coverage.

## 2) Planned Provider Modes

Current state:
- Existing runtime remains Mistral-first with current provider wiring.
- The mode selector below is not yet fully shipped.

Planned modes:
- `auto` (planned): choose best available provider path at runtime using configured credentials/capabilities.
- `native` (planned): force built-in/native provider path only.
- `docling` (planned): prefer Docling-backed path where supported by the operation.

## 3) Planned Fallback Semantics

Current state:
- No new #146 mode-switch fallback contract is finalized in released behavior yet.

Planned semantics:
- `auto`: attempt preferred path, then fallback to the next supported configured path.
- `native`: no cross-provider fallback; fail with actionable configuration/runtime error if native path is unavailable.
- `docling`: attempt Docling path first; fallback behavior remains implementation-defined until finalized (must be documented before GA).

Guardrails for rollout:
- Fallback must never silently bypass required auth/payment controls.
- Errors must stay machine-parseable and explicit about which provider path failed.

## 4) Troubleshooting Quick Checks

Use these checks during rollout validation:
1. Verify installed binary track:
   - `dir2mcp version`
   - package/tap metadata confirms `dir2mcp` vs `dir2mcp-full`.
2. Verify mode intent in effective config:
   - `dir2mcp config print`
   - confirm selected provider mode and related provider credentials.
3. Verify MCP health path:
   - initialize -> `tools/list` -> one representative `tools/call`.
4. Verify expected fallback behavior:
   - remove/disable primary provider credential and confirm mode-specific behavior (`auto` fallback vs `native` fail-fast).
5. Verify logs/events:
   - confirm provider selection and fallback/failure reason are visible and non-secret.

## 5) Staged Rollout Checklist

1. Documentation gate:
   - Publish track definitions and mode semantics as planned/in-progress.
2. Limited internal validation:
   - Exercise all three planned modes against representative corpora.
3. Preview release (`dir2mcp-full` opt-in):
   - Keep default users on existing `dir2mcp` track.
4. Telemetry and failure review:
   - Track mode selection outcomes, fallback frequency, and hard failures.
5. Public guidance update:
   - Move planned wording to current-state wording only after behavior is confirmed shipped.
6. GA decision:
   - Promote broader defaults only when fallback/error semantics are stable and documented.
