package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/unixsuperhero/o-mcp/client"
	"github.com/unixsuperhero/ochazuke"
	ocli "github.com/unixsuperhero/ochazuke/cli"
)

// The runtime: four nouns a host lists (rowNouns), each list's rows
// {id: "NOUN/ID", label, data, cells}, and status state, every list's
// rows as JSON Lines (the catalog's state). The MCP page (mcpPage) mounts
// them in a host. Verbs take their thing as a positional word or as
// --target NOUN/ID (a row a host selected).

const (
	serverNoun  = "mcp-server"
	toolNoun    = "mcp-tool"
	callNoun    = "mcp-call"
	exampleNoun = "mcp-example"
)

var rowNouns = []string{serverNoun, toolNoun, callNoun, exampleNoun}

// ListRow is one element of a list.
type ListRow struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Data  any      `json:"data"`
	Cells []string `json:"cells,omitempty"`
}

// stateExamples bounds the examples the state carries (the newest).
const stateExamples = 500

func serverRow(r ServerRow) ListRow {
	label := r.Name + " [" + r.Harness + "]"
	switch {
	case r.Disabled:
		label += ", disabled"
	case !r.Configured:
		label += ", not configured"
	}
	return ListRow{ID: serverNoun + "/" + r.ID, Label: label, Data: r, Cells: tables[serverNoun].cells(r)}
}

func toolRow(r ToolRow) ListRow {
	return ListRow{ID: toolNoun + "/" + r.Server + "/" + r.Tool, Label: r.ServerName + " " + r.Tool, Data: r, Cells: tables[toolNoun].cells(r)}
}

func callRow(c client.Call) ListRow {
	label := c.Server[strings.LastIndex(c.Server, ":")+1:] + " " + c.Tool + " " + c.At.Local().Format("2006-01-02 15:04")
	if c.IsError || c.Error != "" {
		label += ", error"
	}
	return ListRow{ID: callNoun + "/" + c.ID, Label: label, Data: c, Cells: tables[callNoun].cells(c)}
}

func exampleRow(e Example) ListRow {
	label := e.Server + " " + e.Tool + " " + e.At.Local().Format("2006-01-02 15:04") + " [" + e.Harness + "]"
	if e.IsError {
		label += ", error"
	}
	return ListRow{ID: exampleNoun + "/" + e.ID, Label: label, Data: e, Cells: tables[exampleNoun].cells(e)}
}

// thing is the id a verb acts on: the positional or flag argument name,
// else the --target row of noun (its id without "NOUN/").
func thing(c ochazuke.Ctx, noun, name string) string {
	if v := ochazuke.StringArg(c.Args, name, ""); v != "" {
		return v
	}
	if c.Instance != nil && c.Instance.Noun == noun {
		return strings.TrimPrefix(c.Instance.ID, noun+"/")
	}
	return ""
}

// toolRef is the server and tool a tool verb names: SERVER TOOL words,
// or a --target mcp-tool/SERVER_ID/TOOL row.
func toolRef(c ochazuke.Ctx) (string, string, error) {
	server, tool := ochazuke.StringArg(c.Args, "server", ""), ochazuke.StringArg(c.Args, "tool", "")
	if (server == "" || tool == "") && c.Instance != nil && c.Instance.Noun == toolNoun {
		id := strings.TrimPrefix(c.Instance.ID, toolNoun+"/")
		if i := strings.LastIndex(id, "/"); i > 0 {
			server, tool = id[:i], id[i+1:]
		}
	}
	if server == "" || tool == "" {
		return "", "", errors.New("name a tool: SERVER TOOL or --target mcp-tool/SERVER_ID/TOOL")
	}
	return server, tool, nil
}

func result(v any, err error) ochazuke.Result {
	if err != nil {
		return ochazuke.Failed(err)
	}
	return ochazuke.Done(v)
}

func (t *Tool) shell() *ocli.Shell {
	r := ochazuke.NewRuntime()
	lists := map[string]*ochazuke.List{}
	for _, noun := range rowNouns {
		list, err := r.AddList("list/"+noun, nil)
		if err != nil {
			panic(err) // a fresh runtime takes every list
		}
		lists[noun] = list
	}
	ctx := context.Background()
	warn := func(ws []string) {
		for _, w := range ws {
			fmt.Fprintln(t.Stderr, "warning:", w)
		}
	}
	serverList := func(c ochazuke.Ctx) ([]ListRow, error) {
		rows, warnings := t.serverRows()
		warn(warnings)
		rows, err := filterServers(rows, ochazuke.StringArg(c.Args, "harness", ""), ochazuke.StringArg(c.Args, "state", ""))
		out := []ListRow{}
		for _, r := range rows {
			out = append(out, serverRow(r))
		}
		return out, err
	}
	toolList := func(server string) ([]ListRow, error) {
		rows, err := t.toolRows(server)
		out := []ListRow{}
		for _, r := range rows {
			out = append(out, toolRow(r))
		}
		return out, err
	}
	callList := func(server, tool string, n int) ([]ListRow, error) {
		calls, err := t.calls(server, tool)
		out := []ListRow{}
		for _, c := range calls {
			if n > 0 && len(out) >= n {
				break
			}
			out = append(out, callRow(c))
		}
		return out, err
	}
	exampleList := func(server, tool, harness string, n int) ([]ListRow, error) {
		ex, err := t.examples(server, tool, harness, n)
		out := []ListRow{}
		for _, e := range ex {
			out = append(out, exampleRow(e))
		}
		return out, err
	}
	// serverName is the configured name of a server ref (examples are
	// keyed by the name the harnesses spell), the ref itself when it is
	// not configured.
	serverName := func(ref string) string {
		if s, err := t.server(ref); err == nil {
			return s.Name
		}
		if snap, err := t.snapshot(ref); err == nil {
			return snap.Server.Name
		}
		return ref
	}
	findCall := func(id string) (client.Call, error) {
		calls, err := t.store().Calls()
		for _, c := range calls {
			if c.ID == id {
				return c, nil
			}
		}
		if err == nil {
			err = fmt.Errorf("no recorded call %q", id)
		}
		return client.Call{}, err
	}
	findExample := func(id string) (Example, error) {
		all, _, err := t.Harvest(false)
		for _, e := range all {
			if e.ID == id {
				return e, nil
			}
		}
		if err == nil {
			err = fmt.Errorf("no example %q (harvest reads the session logs)", id)
		}
		return Example{}, err
	}
	// again calls a tool with recorded arguments; a redacted value would
	// send the placeholder, so it refuses.
	again := func(server, tool string, args map[string]any) (any, error) {
		b, _ := json.Marshal(args)
		if strings.Contains(string(b), "[REDACTED]") {
			return nil, errors.New("the recorded arguments were redacted; call the tool with the real arguments")
		}
		res, _, err := t.call(ctx, server, tool, args)
		if err != nil {
			return nil, err
		}
		if res.IsError {
			return res, errors.New("the tool answered an error (isError)")
		}
		return res, nil
	}

	// mcp-server
	r.Define(serverNoun).Columns(tables[serverNoun].columns...).
		Verb("list", func(c ochazuke.Ctx) ochazuke.Result { return result(serverList(c)) },
			"the servers every harness configures (disabled ones too) and saved servers no longer configured: {id, label, data: the server, its snapshot state and tool count, cells}").Rows(serverNoun).Fetch().
		Verb("show", func(c ochazuke.Ctx) ochazuke.Result {
			ref := thing(c, serverNoun, "server")
			rows, _ := t.serverRows()
			s, err := t.server(ref)
			id := ref
			if err == nil {
				id = s.ID
			}
			for _, row := range rows {
				if row.ID == id || (err != nil && row.Name == ref && !row.Configured) {
					out := map[string]any{"server": row}
					if snap, err := t.store().Load(row.ID); err == nil {
						out["initialize"] = snap.Initialize
					}
					return ochazuke.Done(out)
				}
			}
			if err == nil {
				err = fmt.Errorf("no server %q", ref)
			}
			return ochazuke.Failed(err)
		}, "one server: its configuration (names of variables and headers, never values), snapshot state and saved initialize answer").Positional("server").Fetch().
		Verb("discover", func(c ochazuke.Ctx) ochazuke.Result {
			all, _ := c.Args["all"].(bool)
			return result(t.discoverRefs(ctx, thing(c, serverNoun, "server"), all, false))
		}, "connect, list every page of tools and save the snapshot atomically, bound to its config (a failure keeps the old one): SERVER, or --all for every enabled configured server (failures listed in errors, unreachable OAuth-only servers marked)").Positional("server").
		Verb("refresh", func(c ochazuke.Ctx) ochazuke.Result {
			return result(t.discoverRefs(ctx, thing(c, serverNoun, "server"), false, true))
		}, "discover again: SERVER, or every server with a saved snapshot").Positional("server").
		Verb("test", func(c ochazuke.Ctx) ochazuke.Result {
			return result(t.testServer(ctx, thing(c, serverNoun, "server")))
		}, "connect and initialize, calling nothing: serverInfo, protocol version, capabilities (an OAuth server not signed in: needsLogin and the login command; never opens a browser)").Positional("server").Fetch().
		Verb("login", func(c ochazuke.Ctx) ochazuke.Result {
			force, _ := c.Args["force"].(bool)
			return result(t.login(ctx, thing(c, serverNoun, "server"), force))
		}, "sign in to an OAuth server in the browser (the MCP authorization flow: PKCE, a loopback redirect), keep the tokens in the macOS Keychain and connect once to check; --force signs in again when signed in").Positional("server").
		Verb("logout", func(c ochazuke.Ctx) ochazuke.Result {
			return result(t.logout(ctx, thing(c, serverNoun, "server")))
		}, "revoke the server's sign-in when its authorization server allows it, and delete the Keychain item and the record").Positional("server").
		Verb("auth-status", func(c ochazuke.Ctx) ochazuke.Result {
			return result(t.authStatus(thing(c, serverNoun, "server")))
		}, "the sign-in of SERVER, or of every OAuth server: signed-in, needs-sign-in or signed-out, issuer, expiry, scopes and the login command; never a token").Positional("server").Fetch().
		Verb("tools", func(c ochazuke.Ctx) ochazuke.Result {
			return result(toolList(thing(c, serverNoun, "server")))
		}, "the server's saved tools (mcp-tool rows; offline)").Positional("server").Rows(toolNoun).Fetch().
		Verb("examples", func(c ochazuke.Ctx) ochazuke.Result {
			return result(exampleList(serverName(thing(c, serverNoun, "server")), "", ochazuke.StringArg(c.Args, "harness", ""), ochazuke.IntArg(c.Args, "limit", 50)))
		}, "real calls to the server's tools from Claude and Codex session logs, newest first (mcp-example rows)").Positional("server").Rows(exampleNoun).Fetch().
		Verb("calls", func(c ochazuke.Ctx) ochazuke.Result {
			return result(callList(thing(c, serverNoun, "server"), "", ochazuke.IntArg(c.Args, "limit", 0)))
		}, "o-mcp's own calls to the server, newest first (mcp-call rows)").Positional("server").Rows(callNoun).Fetch().
		Verb("forget", func(c ochazuke.Ctx) ochazuke.Result {
			snap, err := t.snapshot(thing(c, serverNoun, "server"))
			if err != nil {
				return ochazuke.Failed(err)
			}
			return result(map[string]any{"forgot": snap.Server.ID, "tools": len(snap.Tools)}, t.store().Remove(snap.Server.ID))
		}, "delete the server's saved snapshot (never its configuration; discover saves it again)").Positional("server").Confirm("Forget the saved tools of {{id}}?").
		Verb("copy-id", func(c ochazuke.Ctx) ochazuke.Result {
			s, err := t.server(thing(c, serverNoun, "server"))
			if err != nil {
				return ochazuke.Failed(err)
			}
			return ochazuke.Done(t.copy(s.ID))
		}, "copy the server's ID (HARNESS:SCOPE:NAME) to the clipboard and answer it").Positional("server").
		Verb("copy-address", func(c ochazuke.Ctx) ochazuke.Result {
			s, err := t.server(thing(c, serverNoun, "server"))
			if err != nil {
				return ochazuke.Failed(err)
			}
			return ochazuke.Done(t.copy(s.Address()))
		}, "copy the server's URL or command line (redacted) to the clipboard and answer it").Positional("server")
	serverParam := ochazuke.Parameter{Type: "string", Noun: serverNoun, Description: "a server name or ID (HARNESS:SCOPE:NAME)"}
	harnessParam := ochazuke.Parameter{Type: "string", Description: "only this harness's (claude, codex, omp, opencode, gemini, ...)"}
	limitParam := ochazuke.Parameter{Type: "integer", Description: "at most this many rows, newest first (0: all)"}
	params(r, "mcp-server.list", map[string]ochazuke.Parameter{
		"harness": harnessParam,
		"state":   {Type: "string", Enum: []string{"all", "enabled", "disabled", "saved", "stale", "unsaved", "signed-in", "needs-sign-in"}, Description: "the view: all (default), enabled, disabled, saved (has a snapshot), stale (its config changed since), unsaved, signed-in or needs-sign-in (OAuth)"},
	})
	for _, v := range []string{"show", "discover", "refresh", "test", "tools", "calls", "forget", "copy-id", "copy-address", "logout", "auth-status"} {
		params(r, "mcp-server."+v, map[string]ochazuke.Parameter{"server": serverParam})
	}
	params(r, "mcp-server.discover", map[string]ochazuke.Parameter{"server": serverParam, "all": {Type: "boolean", Description: "every enabled configured server; the batch continues past failures"}})
	params(r, "mcp-server.calls", map[string]ochazuke.Parameter{"server": serverParam, "limit": limitParam})
	params(r, "mcp-server.examples", map[string]ochazuke.Parameter{"server": serverParam, "harness": harnessParam, "limit": limitParam})
	params(r, "mcp-server.login", map[string]ochazuke.Parameter{"server": serverParam, "force": {Type: "boolean", Description: "sign in again even when signed in"}})
	setHistory(r, ochazuke.HistoryChange, "mcp-server.discover", "mcp-server.refresh", "mcp-server.forget", "mcp-server.login", "mcp-server.logout")
	setHistory(r, ochazuke.HistoryAction, "mcp-server.copy-id", "mcp-server.copy-address")

	// mcp-tool
	r.Define(toolNoun).Columns(tables[toolNoun].columns...).
		Verb("list", func(c ochazuke.Ctx) ochazuke.Result {
			return result(toolList(ochazuke.StringArg(c.Args, "server", "")))
		}, "the saved tools of every server, or of SERVER: {id, label, data: {server, tool, description, hints, params, stale}, cells}; offline").Positional("server").Rows(toolNoun).Fetch().
		Verb("show", func(c ochazuke.Ctx) ochazuke.Result {
			server, tool, err := toolRef(c)
			if err != nil {
				return ochazuke.Failed(err)
			}
			return result(t.toolDetail(server, tool))
		}, "one saved tool: description, input and output schemas, annotations, how many examples, and the call command with the latest observed arguments").Positional("server", "tool").Fetch().
		Verb("schema", func(c ochazuke.Ctx) ochazuke.Result {
			return result(t.toolSchema(c))
		}, "the tool's input schema (JSON Schema)").Positional("server", "tool").Fetch().
		Verb("copy-schema", func(c ochazuke.Ctx) ochazuke.Result {
			schema, err := t.toolSchema(c)
			if err != nil {
				return ochazuke.Failed(err)
			}
			b, _ := json.MarshalIndent(schema, "", "  ")
			return ochazuke.Done(t.copy(string(b)))
		}, "copy the tool's input schema to the clipboard and answer it").Positional("server", "tool").
		Verb("call", func(c ochazuke.Ctx) ochazuke.Result {
			server, tool, err := toolRef(c)
			if err != nil {
				return ochazuke.Failed(err)
			}
			args, _ := c.Args["args"].(map[string]any)
			return result(again(server, tool, args))
		}, "call the tool with --args '{...}' (a JSON object; {} by default) and answer the MCP result; a call can change external data").Positional("server", "tool").
		Verb("examples", func(c ochazuke.Ctx) ochazuke.Result {
			server, tool, err := toolRef(c)
			if err != nil {
				return ochazuke.Failed(err)
			}
			return result(exampleList(serverName(server), tool, ochazuke.StringArg(c.Args, "harness", ""), ochazuke.IntArg(c.Args, "limit", 20)))
		}, "real calls to the tool from Claude and Codex session logs, newest first (mcp-example rows)").Positional("server", "tool").Rows(exampleNoun).Fetch().
		Verb("calls", func(c ochazuke.Ctx) ochazuke.Result {
			server, tool, err := toolRef(c)
			if err != nil {
				return ochazuke.Failed(err)
			}
			return result(callList(server, tool, ochazuke.IntArg(c.Args, "limit", 0)))
		}, "o-mcp's own calls to the tool, newest first (mcp-call rows)").Positional("server", "tool").Rows(callNoun).Fetch().
		Verb("copy-command", func(c ochazuke.Ctx) ochazuke.Result {
			server, tool, err := toolRef(c)
			if err != nil {
				return ochazuke.Failed(err)
			}
			d, err := t.toolDetail(server, tool)
			if err != nil {
				return ochazuke.Failed(err)
			}
			return ochazuke.Done(t.copy(d["command"].(string)))
		}, "copy the command that calls the tool with the latest observed arguments").Positional("server", "tool")
	toolParams := map[string]ochazuke.Parameter{"server": serverParam, "tool": {Type: "string", Description: "the tool's name"}}
	for _, v := range []string{"show", "schema", "copy-schema", "copy-command"} {
		params(r, "mcp-tool."+v, toolParams)
	}
	params(r, "mcp-tool.list", map[string]ochazuke.Parameter{"server": serverParam})
	params(r, "mcp-tool.call", merged(toolParams, map[string]ochazuke.Parameter{"args": {Type: "object", Description: "the tool's arguments, a JSON object"}}))
	params(r, "mcp-tool.examples", merged(toolParams, map[string]ochazuke.Parameter{"harness": harnessParam, "limit": limitParam}))
	params(r, "mcp-tool.calls", merged(toolParams, map[string]ochazuke.Parameter{"limit": limitParam}))
	setHistory(r, ochazuke.HistoryChange, "mcp-tool.call")
	setHistory(r, ochazuke.HistoryAction, "mcp-tool.copy-schema", "mcp-tool.copy-command")

	// mcp-call
	r.Define(callNoun).Columns(tables[callNoun].columns...).
		Verb("list", func(c ochazuke.Ctx) ochazuke.Result {
			return result(callList(ochazuke.StringArg(c.Args, "server", ""), ochazuke.StringArg(c.Args, "tool", ""), ochazuke.IntArg(c.Args, "limit", 0)))
		}, "o-mcp's call history, newest first: {id, label, data: {at, server, tool, arguments (secret-looking values redacted), isError, durationMs, preview}, cells}").Positional("server", "tool").Rows(callNoun).Fetch().
		Verb("show", func(c ochazuke.Ctx) ochazuke.Result {
			return result(findCall(thing(c, callNoun, "call")))
		}, "one recorded call").Positional("call").Fetch().
		Verb("again", func(c ochazuke.Ctx) ochazuke.Result {
			rec, err := findCall(thing(c, callNoun, "call"))
			if err != nil {
				return ochazuke.Failed(err)
			}
			return result(again(rec.Server, rec.Tool, rec.Arguments))
		}, "call the same tool with the same arguments again (refused when they were redacted)").Positional("call").
		Verb("copy-command", func(c ochazuke.Ctx) ochazuke.Result {
			rec, err := findCall(thing(c, callNoun, "call"))
			if err != nil {
				return ochazuke.Failed(err)
			}
			return ochazuke.Done(t.copy(t.callCommand(rec.Server, rec.Tool, rec.Arguments)))
		}, "copy the command line of the call").Positional("call")
	callParam := map[string]ochazuke.Parameter{"call": {Type: "string", Noun: callNoun, Description: "a call id from mcp-call list"}}
	for _, v := range []string{"show", "again", "copy-command"} {
		params(r, "mcp-call."+v, callParam)
	}
	params(r, "mcp-call.list", map[string]ochazuke.Parameter{"server": serverParam, "tool": {Type: "string", Description: "only this tool's"}, "limit": limitParam})
	setHistory(r, ochazuke.HistoryChange, "mcp-call.again")
	setHistory(r, ochazuke.HistoryAction, "mcp-call.copy-command")

	// mcp-example
	r.Define(exampleNoun).Columns(tables[exampleNoun].columns...).
		Verb("list", func(c ochazuke.Ctx) ochazuke.Result {
			return result(exampleList(ochazuke.StringArg(c.Args, "server", ""), ochazuke.StringArg(c.Args, "tool", ""), ochazuke.StringArg(c.Args, "harness", ""), ochazuke.IntArg(c.Args, "limit", 200)))
		}, "real MCP calls from Claude and Codex session logs, newest first: {id, label, data: {harness, server, tool, at, session, path, arguments, isError, result (its start)}, cells}").Positional("server", "tool").Rows(exampleNoun).Fetch().
		Verb("show", func(c ochazuke.Ctx) ochazuke.Result {
			e, err := findExample(thing(c, exampleNoun, "example"))
			if err != nil {
				return ochazuke.Failed(err)
			}
			return ochazuke.Done(map[string]any{"example": e, "command": t.callCommand(e.Server, e.Tool, e.Arguments)})
		}, "one example and the command that makes the same call").Positional("example").Fetch().
		Verb("harvest", func(c ochazuke.Ctx) ochazuke.Result {
			rescan, _ := c.Args["rescan"].(bool)
			_, rep, err := t.Harvest(rescan)
			return result(rep, err)
		}, "read the session logs that changed since the last harvest (--rescan: all of them) into the examples cache").Fetch().
		Verb("again", func(c ochazuke.Ctx) ochazuke.Result {
			e, err := findExample(thing(c, exampleNoun, "example"))
			if err != nil {
				return ochazuke.Failed(err)
			}
			return result(again(e.Server, e.Tool, e.Arguments))
		}, "make the example's call through o-mcp: the configured server of its name, the same arguments").Positional("example").
		Verb("copy-command", func(c ochazuke.Ctx) ochazuke.Result {
			e, err := findExample(thing(c, exampleNoun, "example"))
			if err != nil {
				return ochazuke.Failed(err)
			}
			return ochazuke.Done(t.copy(t.callCommand(e.Server, e.Tool, e.Arguments)))
		}, "copy the command that makes the example's call").Positional("example")
	exParam := map[string]ochazuke.Parameter{"example": {Type: "string", Noun: exampleNoun, Description: "an example id (HARNESS:CALL_ID) from mcp-example list"}}
	for _, v := range []string{"show", "again", "copy-command"} {
		params(r, "mcp-example."+v, exParam)
	}
	params(r, "mcp-example.list", map[string]ochazuke.Parameter{"server": {Type: "string", Description: "a server name as the harnesses spell it"}, "tool": {Type: "string", Description: "only this tool's"}, "harness": harnessParam, "limit": limitParam})
	params(r, "mcp-example.harvest", map[string]ochazuke.Parameter{"rescan": {Type: "boolean", Description: "read every session log again, not only the changed ones"}})
	setHistory(r, ochazuke.HistoryChange, "mcp-example.again")
	setHistory(r, ochazuke.HistoryAction, "mcp-example.copy-command")

	// status: the catalog's state
	r.Define("status").
		Verb("state", func(c ochazuke.Ctx) ochazuke.Result {
			var out ocli.Lines
			add := func(noun string, rows []ListRow, err error) {
				if err != nil {
					fmt.Fprintln(t.Stderr, "warning:", noun+":", err)
				}
				for _, row := range rows {
					out = append(out, ocli.Row{Noun: noun, ID: row.ID, Label: row.Label, Cells: row.Cells})
				}
			}
			rows, err := serverList(c)
			add(serverNoun, rows, err)
			rows, err = toolList("")
			add(toolNoun, rows, err)
			rows, err = callList("", "", 0)
			add(callNoun, rows, err)
			rows, err = exampleList("", "", "", stateExamples)
			add(exampleNoun, rows, err)
			return ochazuke.Done(out)
		}, "every list's rows in one answer, JSON Lines {noun, id, label, cells} for "+strings.Join(rowNouns, ", ")+" (the newest examples only; the catalog's state)").State().Fetch()

	if err := ocli.DeclarePage(r, mcpPage()); err != nil {
		panic(err)
	}
	for noun, tb := range tables {
		lists[noun].SetColumns(tb.columns...)
	}

	sh := ocli.New(r, "o-mcp", t.Stdin, t.Stdout, t.Stderr)
	sh.Usage = ocli.DefaultUsage(t.Prog)
	sh.Version = Version
	// Reveal makes a --target row exist: the verbs look the thing up by
	// its id themselves.
	sh.Reveal = func(target string) error {
		noun, id, ok := strings.Cut(target, "/")
		if !ok || id == "" || lists[noun] == nil {
			return fmt.Errorf("not a row id (NOUN/ID from a list): %s", target)
		}
		return lists[noun].SetItems([]ochazuke.ListItem{{ID: target, Label: id, Noun: noun, Data: id}})
	}
	return sh
}

// mcpPage is the data page a host (mtm) mounts: the four lists in a
// groups rail and the selected row's record beside them.
func mcpPage() ocli.CatalogPage {
	return ocli.CatalogPage{
		ID: "mcp", Title: "MCP", Layout: ocli.LayoutGroups,
		Lists: []ocli.CatalogList{
			{ID: "mcp-servers", Noun: serverNoun, Title: "Servers"},
			{ID: "mcp-tools", Noun: toolNoun, Title: "Tools"},
			{ID: "mcp-calls", Noun: callNoun, Title: "Calls"},
			{ID: "mcp-examples", Noun: exampleNoun, Title: "Examples"},
		},
		Detail: &ocli.CatalogDetail{},
	}
}

// toolSchema is a saved tool's input schema.
func (t *Tool) toolSchema(c ochazuke.Ctx) (any, error) {
	server, tool, err := toolRef(c)
	if err != nil {
		return nil, err
	}
	snap, err := t.snapshot(server)
	if err != nil {
		return nil, err
	}
	tl, ok := snap.Tool(tool)
	if !ok {
		return nil, fmt.Errorf("%s has no saved tool %q", snap.Server.ID, tool)
	}
	return tl.InputSchema, nil
}

// copy writes text to the clipboard ($O_MCP_CLIPBOARD, else pbcopy,
// wl-copy, xclip or xsel; o-md's) and answers it, with whether it was
// copied.
func (t *Tool) copy(text string) map[string]any {
	var argv []string
	if cmd := t.getenv("O_MCP_CLIPBOARD"); cmd != "" {
		argv = strings.Fields(cmd)
	} else {
		candidates := [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}}
		if runtime.GOOS == "darwin" {
			candidates = append([][]string{{"pbcopy"}}, candidates...)
		}
		for _, cand := range candidates {
			if _, err := exec.LookPath(cand[0]); err == nil {
				argv = cand
				break
			}
		}
	}
	copied := false
	if len(argv) > 0 {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin = strings.NewReader(text)
		cmd.Stderr = os.Stderr
		copied = cmd.Run() == nil
	}
	return map[string]any{"text": text, "copied": copied}
}

func params(r *ochazuke.Runtime, key string, ps map[string]ochazuke.Parameter) {
	op, ok := r.Operations[key]
	if !ok {
		panic("params: no operation " + key)
	}
	op.Parameters = ps
	r.Operations[key] = op
}

func merged(ms ...map[string]ochazuke.Parameter) map[string]ochazuke.Parameter {
	out := map[string]ochazuke.Parameter{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// setHistory declares the history kind of each named verb (o-inventory's):
// Fetch marks a read where it is declared; a change or an action is set
// here. An unknown key panics.
func setHistory(r *ochazuke.Runtime, kind string, keys ...string) {
	for _, key := range keys {
		op, ok := r.Operations[key]
		if !ok {
			panic("setHistory: no operation " + key)
		}
		op.HistoryKind = kind
		r.Operations[key] = op
	}
}
