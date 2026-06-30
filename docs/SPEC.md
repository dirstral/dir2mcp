# SPEC.md — moved to dirstral-spec

This document is **no longer maintained in this repository**. The canonical,
version-controlled copy lives in the [`dirstral-spec`](https://github.com/dirstral/dirstral-spec)
repository and is vendored here as a **pinned git submodule** so there is a
single source of truth and this copy can no longer drift.

- In-tree (pinned submodule): [`dirstral-spec/docs/SPEC.md`](../dirstral-spec/docs/SPEC.md)
- Upstream: <https://github.com/dirstral/dirstral-spec/blob/main/docs/SPEC.md>

The spec version dir2mcp targets is recorded in the **compatibility matrix** in
[`dirstral-spec/spec/versioning.md`](../dirstral-spec/spec/versioning.md) — the
single source of truth — and the vendored spec's own current version is in that
file's header. This stub intentionally **no longer hardcodes a version number**:
the previous `0.16.x` claim had drifted from both the matrix and the vendored
spec, which is exactly the duplication this submodule layout exists to prevent.

If the `dirstral-spec/` directory is empty, fetch the submodule:

```bash
git submodule update --init --recursive
```
