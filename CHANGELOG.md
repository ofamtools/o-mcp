# Changelog

## Unreleased

- Changed: requires ochazuke v0.102.0 and oden v0.3.0 (the Operations registry and three-part keys; `cmd/opname` moved the code). o-mcp declares its namespace, `mcp` (`ochazuke.DeclareNamespace`), so its keys are `mcp.<noun>.<verb>` and the catalog carries `namespace: mcp`. CLI spellings are unchanged.
- Changed: forgetting a rediscoverable server snapshot explicitly allows agents despite its owner confirmation prompt. Logout remains allowed.
