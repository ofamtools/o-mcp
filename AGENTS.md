# o-mcp: agent guide

The family's MCP tool (`o-mcp`, so `o mcp`), a Go recreation of `~/bin/h-mcp` (design: `~/bin/h-mcp.md`; plan: `~/hub/o-plans/plans/inbox-and-signals.md`). An oden app with an ochazuke runtime behind `ochazuke/cli`, plus package `client`, the in-process API other tools (o-slack) import.

## Plans and todos

Family plans, todos and the roadmap live in `~/hub/o-plans/` (`README.md` is the index). Do not add a TODO.md or docs/plans/ to this repo.

## Where does X live

| Concern | File |
|---|---|
| `Tool`, `main`, the oden app, paths (data, cache) | `main.go` |
| The top-level commands: `call` (its own parsing, stdin, exit 1 on a tool error) and the forwards to runtime verbs | `commands.go` |
| The runtime: nouns `mcp-server`, `mcp-tool`, `mcp-call`, `mcp-example`, `status state`, the MCP page, Reveal, clipboard | `shell.go` |
| The operations behind the verbs (server rows, discover, call, tool detail) | `ops.go` |
| Columns and cells per noun | `table.go` |
| Session-log harvest (Claude, Codex) and its cache | `examples.go` |
| Configured servers (o-ai's `harness`), Find, errors | `client/client.go` |
| Connect, transports, credential resolution, Session (Tools, Call) | `client/connect.go` |
| OAuth sign-in: the go-sdk OAuthHandler, Login, Logout, Status, refresh, records, NeedsLoginError | `client/oauth.go` |
| Keyring: the macOS Keychain through `/usr/bin/security`, MemoryKeyring | `client/keyring.go` |
| Arguments (ParseArgs, SetArg) and `Data` | `client/args.go` |
| Snapshots, Store (atomic save), call history | `client/store.go` |
| The fake MCP server tests use (in memory, HTTP, stdio as the test binary), the fake OAuth server and browser | `internal/fake` |

## Invariants

- Read-only on configuration: o-mcp never edits a harness's config. `forget` deletes a snapshot only.
- Never store a secret value: records keep variable and header names; the raw config entry stays unexported in `client.Server` and is never marshalled; the call history and examples pass `client.RedactValues`. A fingerprint hashes the public definition only.
- Disabled servers are listed; a batch skips them; a single connect refuses them (`ErrDisabled`).
- A 401 from a server sign-in applies to (`Server.OAuth`: no configured Authorization credential) is `NeedsLoginError`, naming `o-mcp login NAME`; any other 401/403 is `UnreachableError`. Neither stops a batch.
- Sign-in tokens live only in the macOS Keychain (service `o-mcp`, account `SERVER_ID|ISSUER`), never in a file, log, the catalog, the history or JSON output; a secret never goes on a command line (`security -i` on stdin). The `auth/` record holds no secret.
- Only `login` (`client.Login`, or `Connect` with `Options.Interactive`) opens a browser. `call`, `discover`, `test` and library callers fail with the hint instead.
- A snapshot write is atomic; a failed discovery keeps the old snapshot.
- Every list verb declares `.Rows(noun)` and every verb a history kind (`TestCatalog`); every row carries one cell per column (`TestStateCarriesEveryNoun`).
- Tests never read the real home, call a real server, open a browser or touch the real Keychain: fixture homes from `testdata/`, the fake servers from `internal/fake` (`fake.Browser` drives the redirect), `client.NewMemoryKeyring()`, `$O_MCP_CLIPBOARD=true`. `TestRealKeychain` runs only with `O_MCP_KEYCHAIN_TEST=1`.

## Every commit

`gofmt -l . && go vet ./... && go test -race ./...` green (with `GOWORK=off`); `go run . catalog` prints schema 1. Conventional messages, never amend.
