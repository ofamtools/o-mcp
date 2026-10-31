<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-animated-dark.svg">
    <img alt="o-mcp" src="assets/logo-animated-light.svg" width="247">
  </picture>
  <br>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/github/go-mod/go-version/ofamtools/o-mcp"></a>
  <a href="LICENSE"><img alt="License" src="https://img.shields.io/github/license/ofamtools/o-mcp"></a>
</p>

<p align="center">The MCP servers your AI harnesses configure, in one place: see them all, save their tools for offline use, call a tool directly, and find the real calls your agents already made.</p>

o-mcp (`o mcp` under [oden](https://github.com/ofamtools/oden)'s `o`, or `o-mcp`) is a Go recreation of `h-mcp` (`~/bin/h-mcp`, `~/bin/h-mcp.md`). It reads every harness's MCP configuration through [o-ai](https://github.com/ofamtools/o-ai)'s `harness` package, connects with the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) over stdio, Streamable HTTP or legacy SSE, and keeps each server's discovered tools on disk, bound to the configuration they came from. Package [`client`](client) is the same work in process, for tools such as o-slack.

## Install

Module `ofam.dev/o-mcp`; requires Go 1.27 or newer.

```sh
go install ofam.dev/o-mcp@latest
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
o mcp login SERVER [--force]       # OAuth sign-in in the browser; tokens go to the macOS Keychain
o mcp logout SERVER                # revoke (when offered) and delete the Keychain item
o mcp auth status [SERVER]         # signed in or not, issuer, expiry, scopes; never a token
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

Credentials come only from the server's own configuration, resolved when o-mcp connects: `headers` and `http_headers` (with `${VAR}`, `${env:VAR}`, `{env:VAR}`, `${VAR:-default}` references), Codex's `bearer_token_env_var` and `env_http_headers`, a stdio server's `env` or `environment`, and `cwd`. o-mcp never stores a value: records keep variable and header names, and the call history and examples pass a secret-value redaction. A remote server whose configured credential is rejected (401) or that answers 403 is reported as unreachable; in `discover --all` it is one error line and the batch goes on.

### Sign-in (OAuth)

A remote server whose config gives no `Authorization` header and no bearer-token variable may ask for an OAuth sign-in instead (a gateway such as Runlayer does). Then `configured` shows `needs sign-in` once a connect has met it, and `test`, `discover` and `call` fail with `needs sign-in`, never `unreachable`, naming the command: `o-mcp login SERVER`.

`o-mcp login SERVER` runs the MCP authorization flow (OAuth 2.1). It asks the server without a token and reads its 401 `WWW-Authenticate` challenge. It fetches the protected-resource metadata (the challenge's `resource_metadata`, else the well-known URLs) and then the authorization server's metadata. It registers a client: the config entry's `oauth.clientId` when set (Claude Code's spelling; `oauth.scopes` and `oauth.callbackPort` are read too), else dynamic client registration; without either it says to set `oauth.clientId`. It then opens the browser with `open`, printing the URL as a fallback, for the authorization-code flow with PKCE (S256) and a redirect to `127.0.0.1` on a random port. It exchanges the code, stores the tokens, and connects once to check. It is the only command that opens a browser: `call`, `discover`, `test`, the batch and every tool on package `client` (o-slack's scheduled poll) fail with the hint instead, so a schedule never pops a window. `--force` signs in again when already signed in.

Every connect then sends the stored token. It refreshes the token a minute before it expires, and once when the server answers 401. If the refresh fails, the error says `sign in again: o-mcp login SERVER`. `o-mcp logout SERVER` revokes the tokens when the authorization server offers revocation and deletes them. `o-mcp auth status [SERVER]` shows the state (`signed-in`, `needs-sign-in`, `signed-out`), issuer, scopes, expiry and whether a refresh token is stored. It never shows a token.

Where things live:

- **Tokens:** only in the macOS login Keychain, as generic passwords with service `o-mcp` and account `SERVER_ID|ISSUER` (for example `claude:user:runlayer|https://auth.example.com`). One item holds the access and refresh tokens, the client id and secret, and the token and revocation endpoints. o-mcp drives `/usr/bin/security` (no cgo): writes go to `security -i` on stdin, so a token never appears on a command line, and a long item is split over `ACCOUNT`, `ACCOUNT#2`, and so on. On another OS sign-in fails rather than store tokens elsewhere.
- **The record:** `auth/` in the data directory, one file per server. It holds the state, issuer, scopes, expiry and the Keychain account, and no token. `configured` reads only these files, never the Keychain.

Tokens never go in a file, the catalog, the call history or JSON output, and the auth errors are redacted.

### Examples

`harvest` reads `~/.claude/projects/**/*.jsonl` (tool_use blocks named `mcp__SERVER__TOOL`, joined to their tool_result) and `~/.codex/sessions` and `archived_sessions` (`McpToolCall` items and older `mcp_tool_call_end` events) and caches each file's calls by size and modification time in `~/.cache/o-mcp/examples.json`, so a second harvest reads only what changed. Every list of examples harvests first. Examples never connect; `mcp-example again` makes the same call through o-mcp (refused when the recorded arguments were redacted).

## Nouns and verbs

| Noun | Verbs |
|---|---|
| `mcp-server` | `list` (`--harness`, `--state`, including `signed-in` and `needs-sign-in`), `show`, `discover` (`--all`), `refresh`, `test`, `login` (`--force`), `logout`, `auth-status`, `tools`, `examples`, `calls`, `forget` (delete the snapshot, never the config), `copy-id`, `copy-address` |
| `mcp-tool` | `list` (`--server`), `show`, `schema`, `copy-schema`, `call` (`--args '{...}'`), `examples`, `calls`, `copy-command` |
| `mcp-call` | `list`, `show`, `again`, `copy-command` |
| `mcp-example` | `list` (`--server`, `--tool`, `--harness`), `show`, `harvest` (`--rescan`), `again`, `copy-command` |
| `status` | `state` (the catalog's state) |

Every verb takes its thing as words (`mcp-tool show tuios tuios_list_windows`) or as a row, `--target mcp-tool/claude:user:tuios/tuios_list_windows`. Copy verbs use `$O_MCP_CLIPBOARD`, else pbcopy, wl-copy, xclip or xsel, and answer the text.

## MCP page

The catalog page carries each row's real data. Its detail shows metadata followed by Request (`arguments`) and Response (`result`, parsed from its JSON string) sections for examples and calls. Each section shows a structure tree on the left and the selected node's pretty-printed data on the right; below 64 content cells they stack. Prose and truncated JSON fall back to complete plain text. Responses remain the existing redacted previews (up to 2048 bytes), not full responses. Call rows expose their saved `preview` as `result` too. Ochazuke's shared `detail.sections` setting defaults to `true`; `false` restores the plain record detail and removes JSON trees from focus order. No new app setting is needed.

Tools reads saved snapshots offline. When empty: **no tools yet: run `mcp-server discover --all`**. Calls is **o-mcp's own calls; harvested examples are under Examples; `mcp-example again` re-runs one here**. The catalog currently has no empty-list text field, so the group labels carry short reminders and this section carries the full guidance.

Default normal-mode bindings are catalog data, scoped to rows in their group:

| Group | Key | Verb | Scope |
|---|---|---|---|
| Servers | `g d` | `mcp-server.discover` | Selected server |
| Servers | `g r` | `mcp-server.refresh` | Selected server |
| Examples | `g a` | `mcp-example.again` | Selected example; makes a real call |

Enter a group to focus its rows before using these bindings. They avoid the framework's motions and the host's CRUD keys. Hosts import them only where they do not clash with their own bindings; the host's binding rows can remap or unbind them. Batch discovery remains available through the existing verb's `all` argument. The groups rail itself has no app-specific bindings: the host mounts catalog bindings on domain nouns, not individual rail instances.

Example output now names the transcript file `transcript`, leaving `cwd` available for the host's project association. Legacy `path` is accepted on input, including existing harvest caches, for one release; output emits only `transcript` so the transcript cannot override the project directory.

The JSON sections use ochazuke v0.79.0. Focus each tree with the shared window motions (`ctrl+w h/l`); standard tree motions and folding select the data. `J/K` scroll the selected value, `y` copies it completely, and `g v` opens it in a read-only text view. These keys come from the framework's binding rows. Opening payloads in o-json remains dependent on host action support.

Compared with Vim: these three app actions operate on MCP servers and recorded calls; there is no corresponding Vim action or `:help` tag (n/a). The shared JSON trees use `h/l` to collapse/expand, similar to Vim's `zc`/`zo` (`:help zc`, `:help zo`); `ctrl+w h/l` follows drawing order, similar to Vim's window motions (`:help CTRL-W_h`, `:help CTRL-W_l`). The data follows the selected tree node; Vim windows have independent buffers and cursors.

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

`Discover` returns a `Snapshot`. `Store` (`DefaultStore`) saves, loads and lists snapshots and records calls. `ConnectTransport` takes any go-sdk transport (an in-memory one in tests). Errors: `ErrNotFound`, `ErrAmbiguous`, `ErrDisabled`, `*UnreachableError` (`IsUnreachable`), `*NeedsLoginError` (`IsNeedsLogin`; its `Command()` is `o-mcp login NAME`), `ErrNoData`.

Sign-in: `Connect` uses the stored tokens transparently and refreshes them. `Login(ctx, srv, opts)` runs the browser flow, and `Logout`, `Status`, `AuthState` (from the record alone) and `AuthRecords` manage it. `Options.Interactive` lets `Connect` run `Login` when the server needs it; it is off by default, so a library call never opens a browser. `Options.Keyring` defaults to `Keychain()`; tests pass `NewMemoryKeyring()`. `Options.OpenURL`, `Prompt`, `SignInTimeout` and `AuthDir` are the other knobs. `Server.OAuth()` reports whether sign-in applies.

## In the family

- **o-ai** owns reading the harnesses' configuration (package `harness`); o-mcp adds connecting, which o-ai never does.
- **mtm and nabe** mount `o-mcp catalog` as the MCP page: servers, tools, calls and examples in a groups rail, the selected row's record beside them. The extension namespace is `mcp`.
- **o-slack** reads Slack through `client`: a gateway entry with a header or bearer-token variable, or one signed in with `o-mcp login` (Runlayer). Its poll never opens a browser; it names the login command instead.

## Logo

A plug, the connector an MCP server is to a harness, in momo pink. `python3 docs/logo/build.py` writes `assets/icon.svg`, `assets/logo-light.svg` and `assets/logo-dark.svg` in the family's lettering (`--check` confirms the glyphs redraw ochazuke's and matomeru's).

MIT license.
