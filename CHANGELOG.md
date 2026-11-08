# Changelog

## v0.2.0 (2026-11-08)

- Changed, breaking (lexicon, ochazuke v0.105.0, one shot, no aliases; every rename is in `docs/lexicon-renames-v0.105.tsv`): requires ochazuke v0.105.0. The `mcp-example` noun is `example`; the `status` noun and its `state` verb are `mcp-server lists` (the state source; the `o-mcp state` shortcut is `o-mcp lists`); the copy verbs are `yank-id`, `yank-address`, `yank-schema` and `yank-command`; `--state` of `configured` and `mcp-server list` is `--status`; the auth record's and the auth-status answer's `state` field is `status` (an older record is read as status and written back so); `mcp-server` is the main noun. `TestLexicon` (lexiconcheck) runs with one baseline row: the namespace `mcp` is also the framework's `ochazuke.mcp` noun.
- Changed: requires ochazuke v0.102.0 and oden v0.3.0 (the Operations registry and three-part keys; `cmd/opname` moved the code). o-mcp declares its namespace, `mcp` (`ochazuke.DeclareNamespace`), so its keys are `mcp.<noun>.<verb>` and the catalog carries `namespace: mcp`. CLI spellings are unchanged.
- Changed: forgetting a rediscoverable server snapshot explicitly allows agents despite its owner confirmation prompt. Logout remains allowed.
