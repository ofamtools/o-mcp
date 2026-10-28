<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
    <img alt="o-mcp" src="assets/logo-light.svg" width="247">
  </picture>
  <br>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/github/go-mod/go-version/unixsuperhero/o-mcp"></a>
  <a href="LICENSE"><img alt="License" src="https://img.shields.io/github/license/unixsuperhero/o-mcp"></a>
</p>

<p align="center">The MCP servers your AI harnesses configure, in one place: see them all, save their tools for offline use, call a tool directly, and find the real calls your agents already made.</p>

o-mcp (`o mcp` under [oden](https://github.com/unixsuperhero/oden)'s `o`, or `o-mcp`) is a Go recreation of `h-mcp` (`~/bin/h-mcp`, `~/bin/h-mcp.md`). It reads every harness's MCP configuration through [o-ai](https://github.com/unixsuperhero/o-ai)'s `harness` package, connects with the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) over stdio, Streamable HTTP or legacy SSE, and keeps each server's discovered tools on disk, bound to the configuration they came from. Package [`client`](client) is the same work in process, for tools such as o-slack.

## Install

Module `github.com/unixsuperhero/o-mcp`; requires Go 1.27 or newer.

```sh
go install github.com/unixsuperhero/o-mcp@latest
```

or, from a checkout:

```sh
gofmt -l . && go vet ./... && go test -race ./...
go build -o ~/bin/o-mcp .
```

ochazuke (`v0.24.0`), oden (`v0.1.1`) and the go-sdk (`v1.6.1`) are required at their tags.

## Usage

```sh
o mcp configured [--harness H] [--state all|enabled|disabled|saved|stale|unsaved]
o mcp discover SERVER | --all      # connect, list every page of tools, save them
o mcp refresh [SERVER]             # discover the saved servers again
o mcp test SERVER                  # initialize only; calls nothing
o mcp tools [SERVER]               # the saved tools, offline
o mcp tool SERVER TOOL             # one tool: schemas, annotations, a ready call command
o mcp call SERVER TOOL [KEY=VALUE...] [--data JSON | --args-file FILE|-] [--extract] [--timeout SECONDS]
o mcp harvest [--rescan]           # read Claude and Codex session logs for MCP calls
o mcp examples [SERVER [TOOL]]     # those calls, newest first
o mcp calls [SERVER [TOOL]]        # o-mcp's own call history
o mcp state                        # every list's rows as JSON Lines
o mcp catalog                      # the nouns and verbs, for a host
```

A SERVER is a name or an ID, `HARNESS:SCOPE:NAME` (`claude:user:tuios`). A name configured the same way in several harnesses is the first enabled one; configured differently, o-mcp asks for the ID.

```console
$ o mcp call tuios tuios_list_windows --data '{"session":"work"}' --extract
$ printf '%s' '{"query":"in:#eng deploy"}' | o mcp call slack search_messages --args-file -
$ o mcp call slack search_messages query='deploy' count=20
```

`call` prints the MCP result as JSON (no JSON-RPC envelope). `--data` (alias `--args`) gives the arguments inline, `--args-file FILE` or `-` (stdin) from a file; `KEY=VALUE` words override them (a value that parses as JSON is that value, else a string; `a.b=1` nests). For sensitive arguments use stdin: argv is visible to other processes and shell history. `--extract` prints only the data: `structuredContent` when present, else the one text block parsed as JSON; prose, several blocks or other content fail. A tool error (`isError`) prints the full result and exits 1. Diagnostics go to stderr.

## Concepts

### The configured inventory

`configured` lists every server Claude, Codex, omp, OpenCode, Gemini and clawed configure for the current directory (user, local and project scopes), exactly as `o-ai mcp list` sees them, plus saved servers whose configuration is gone. Disabled entries are listed; `discover --all` skips them and `discover`, `test` and `call` refuse them. Nothing here connects.

### Snapshots

`discover` initializes a session, pages through `tools/list` and writes the snapshot atomically (a temporary file renamed over the old one) to `$O_MCP_DATA`, else `$XDG_DATA_HOME/o-mcp`, else `~/.local/share/o-mcp`, one file per server under `servers/`. A failure keeps the previous snapshot; zero tools is still a snapshot. A snapshot holds the server's public record (ID, config file, transport, redacted command or URL, variable and header names), a fingerprint of that definition, the initialize answer and every tool with its schemas and annotations. `tools`, `tool` and `mcp-tool schema` read it offline. When the configuration changes, the snapshot is `stale` until discovered again; editing a config never refreshes it.

### Credentials

Credentials come only from the server's own configuration, resolved when o-mcp connects: `headers` and `http_headers` (with `${VAR}`, `${env:VAR}`, `{env:VAR}`, `${VAR:-default}` references), Codex's `bearer_token_env_var` and `env_http_headers`, a stdio server's `env` or `environment`, and `cwd`. o-mcp never stores a value: records keep variable and header names, and the call history and examples pass a secret-value redaction. A remote server that answers 401 or 403 is reported as unreachable (an OAuth-only connector, such as Claude's claude.ai connectors); in `discover --all` it is one error line and the batch goes on.

### Examples

`harvest` reads `~/.claude/projects/**/*.jsonl` (tool_use blocks named `mcp__SERVER__TOOL`, joined to their tool_result) and `~/.codex/sessions` and `archived_sessions` (`McpToolCall` items and older `mcp_tool_call_end` events) and caches each file's calls by size and modification time in `~/.cache/o-mcp/examples.json`, so a second harvest reads only what changed. Every list of examples harvests first. Examples never connect; `mcp-example again` makes the same call through o-mcp (refused when the recorded arguments were redacted).

## Nouns and verbs

| Noun | Verbs |
|---|---|
| `mcp-server` | `list` (`--harness`, `--state`), `show`, `discover` (`--all`), `refresh`, `test`, `tools`, `examples`, `calls`, `forget` (delete the snapshot, never the config), `copy-id`, `copy-address` |
| `mcp-tool` | `list` (`--server`), `show`, `schema`, `copy-schema`, `call` (`--args '{...}'`), `examples`, `calls`, `copy-command` |
| `mcp-call` | `list`, `show`, `again`, `copy-command` |
| `mcp-example` | `list` (`--server`, `--tool`, `--harness`), `show`, `harvest` (`--rescan`), `again`, `copy-command` |
| `status` | `state` (the catalog's state) |

Every verb takes its thing as words (`mcp-tool show tuios tuios_list_windows`) or as a row, `--target mcp-tool/claude:user:tuios/tuios_list_windows`. Copy verbs use `$O_MCP_CLIPBOARD`, else pbcopy, wl-copy, xclip or xsel, and answer the text.

## Package client

```go
servers, warnings := client.Configured(client.Options{})
srv, err := client.Find(servers, "slack")
sess, err := client.Connect(ctx, srv, client.Options{})
defer sess.Close()
tools, err := sess.Tools(ctx)
res, err := sess.Call(ctx, "search_messages", map[string]any{"query": "deploy"})
data, err := client.Data(res)
```

`Discover` returns a `Snapshot`; `Store` (`DefaultStore`) saves, loads and lists snapshots and records calls. `ConnectTransport` takes any go-sdk transport (an in-memory one in tests). Errors: `ErrNotFound`, `ErrAmbiguous`, `ErrDisabled`, `*UnreachableError` (`IsUnreachable`), `ErrNoData`.

## In the family

- **o-ai** owns reading the harnesses' configuration (package `harness`); o-mcp adds connecting, which o-ai never does.
- **mtm and nabe** mount `o-mcp catalog` as the MCP page: servers, tools, calls and examples in a groups rail, the selected row's record beside them. The extension namespace is `mcp`.
- **o-slack** (next) reads Slack through `client` once a Slack MCP server with a credential is configured; `o mcp configured` shows it then.

## Logo

A plug, the connector an MCP server is to a harness, in momo pink. `python3 docs/logo/build.py` writes `assets/icon.svg`, `assets/logo-light.svg` and `assets/logo-dark.svg` in the family's lettering (`--check` confirms the glyphs redraw ochazuke's and matomeru's).

MIT license.
