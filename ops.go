package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/unixsuperhero/o-mcp/client"
)

// The operations behind the verbs, each a plain function of the tool:
// the runtime (shell.go) and the top-level commands (commands.go) call
// them, so the CLI and a host see the same answers.

// ServerRow is a server as o-mcp lists it: the configured server (or the
// saved record of one no longer configured) and its snapshot's state.
type ServerRow struct {
	client.Server
	Configured   bool       `json:"configured"`
	Snapshot     string     `json:"snapshot"` // none, saved, stale (the config changed since)
	Tools        int        `json:"tools"`    // saved tools, -1 when none are saved
	DiscoveredAt *time.Time `json:"discoveredAt,omitempty"`
}

// snapshotState is how a snapshot relates to the configured server.
func snapshotState(snap *client.Snapshot, s *client.Server) string {
	switch {
	case snap == nil:
		return "none"
	case s != nil && snap.Fingerprint != s.Fingerprint():
		return "stale"
	}
	return "saved"
}

// serverRows are the configured servers, then the saved snapshots whose
// server is no longer configured, with their snapshot state.
func (t *Tool) serverRows() ([]ServerRow, []string) {
	servers, warnings := client.Configured(t.options())
	saved, err := t.store().List()
	if err != nil {
		warnings = append(warnings, err.Error())
	}
	snaps := map[string]*client.Snapshot{}
	for i := range saved {
		snaps[saved[i].Server.ID] = &saved[i]
	}
	var out []ServerRow
	row := func(s client.Server, configured bool) ServerRow {
		r := ServerRow{Server: s, Configured: configured, Tools: -1}
		snap := snaps[s.ID]
		cur := &s
		if !configured {
			cur = nil
		}
		r.Snapshot = snapshotState(snap, cur)
		if snap != nil {
			r.Tools = len(snap.Tools)
			at := snap.DiscoveredAt
			r.DiscoveredAt = &at
		}
		return r
	}
	seen := map[string]bool{}
	for _, s := range servers {
		seen[s.ID] = true
		out = append(out, row(s, true))
	}
	for _, snap := range saved {
		if !seen[snap.Server.ID] {
			out = append(out, row(snap.Server, false))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, warnings
}

// filterServers keeps the rows of harness (any when "") in state: all,
// enabled, disabled, saved (a snapshot exists), stale, unsaved.
func filterServers(rows []ServerRow, harness, state string) ([]ServerRow, error) {
	var out []ServerRow
	for _, r := range rows {
		if harness != "" && r.Harness != harness {
			continue
		}
		keep := true
		switch state {
		case "", "all":
		case "enabled":
			keep = !r.Disabled
		case "disabled":
			keep = r.Disabled
		case "saved":
			keep = r.Snapshot != "none"
		case "stale":
			keep = r.Snapshot == "stale"
		case "unsaved":
			keep = r.Snapshot == "none"
		default:
			return nil, fmt.Errorf("unknown --state %q: all, enabled, disabled, saved, stale or unsaved", state)
		}
		if keep {
			out = append(out, r)
		}
	}
	return out, nil
}

// server is the configured server ref names (client.Find).
func (t *Tool) server(ref string) (client.Server, error) {
	if ref == "" {
		return client.Server{}, errors.New("name a server: SERVER (a name or HARNESS:SCOPE:NAME) or --target mcp-server/ID")
	}
	servers, _ := client.Configured(t.options())
	return client.Find(servers, ref)
}

// snapshot is the saved snapshot of the server ref names: the configured
// server's, else a saved one by ID or name (usable offline, with its
// configuration gone).
func (t *Tool) snapshot(ref string) (client.Snapshot, error) {
	st := t.store()
	if s, err := t.server(ref); err == nil {
		snap, err := st.Load(s.ID)
		if errors.Is(err, os.ErrNotExist) {
			return snap, fmt.Errorf("%s has no saved tools: run %s discover %s", s.ID, t.Prog, ref)
		}
		return snap, err
	} else if !errors.Is(err, client.ErrNotFound) {
		return client.Snapshot{}, err
	}
	saved, _ := st.List()
	var named []client.Snapshot
	for _, snap := range saved {
		if snap.Server.ID == ref {
			return snap, nil
		}
		if snap.Server.Name == ref {
			named = append(named, snap)
		}
	}
	if len(named) == 1 {
		return named[0], nil
	}
	if len(named) > 1 {
		return client.Snapshot{}, fmt.Errorf("%d saved servers are named %q: name one by ID", len(named), ref)
	}
	return client.Snapshot{}, fmt.Errorf("%w named %q, and nothing is saved under that name", client.ErrNotFound, ref)
}

// DiscoverResult is a discovery's answer: the servers saved and the ones
// that failed (Unreachable: the server needs a credential its config does
// not give, such as an OAuth-only connector).
type DiscoverResult struct {
	Servers []DiscoveredServer `json:"servers"`
	Errors  []DiscoverError    `json:"errors"`
}

type DiscoveredServer struct {
	ID           string    `json:"id"`
	Tools        int       `json:"tools"`
	DiscoveredAt time.Time `json:"discoveredAt"`
}

type DiscoverError struct {
	ID          string `json:"id"`
	Error       string `json:"error"`
	Unreachable bool   `json:"unreachable,omitempty"`
}

// discover connects to each server, lists its tools and saves the
// snapshot atomically; a failure keeps the previous snapshot. Disabled
// servers are skipped in a batch (all) and refused alone.
func (t *Tool) discover(ctx context.Context, servers []client.Server, batch bool) DiscoverResult {
	res := DiscoverResult{Servers: []DiscoveredServer{}, Errors: []DiscoverError{}}
	st := t.store()
	for _, s := range servers {
		if batch && s.Disabled {
			continue
		}
		snap, err := client.Discover(ctx, s, t.options())
		if err == nil {
			err = st.Save(snap)
		}
		if err != nil {
			res.Errors = append(res.Errors, DiscoverError{ID: s.ID, Error: err.Error(), Unreachable: client.IsUnreachable(err)})
			continue
		}
		res.Servers = append(res.Servers, DiscoveredServer{ID: s.ID, Tools: len(snap.Tools), DiscoveredAt: snap.DiscoveredAt})
	}
	return res
}

// discoverRefs is discover of one server, every enabled configured server
// (all), or every saved one (refresh with no ref).
func (t *Tool) discoverRefs(ctx context.Context, ref string, all, refresh bool) (DiscoverResult, error) {
	switch {
	case ref != "":
		s, err := t.server(ref)
		if err != nil {
			return DiscoverResult{}, err
		}
		res := t.discover(ctx, []client.Server{s}, false)
		if len(res.Errors) > 0 {
			e := res.Errors[0]
			return res, errors.New(e.Error)
		}
		return res, nil
	case all:
		servers, _ := client.Configured(t.options())
		return t.discover(ctx, servers, true), nil
	case refresh:
		servers, _ := client.Configured(t.options())
		saved, _ := t.store().List()
		has := map[string]bool{}
		for _, snap := range saved {
			has[snap.Server.ID] = true
		}
		var again []client.Server
		for _, s := range servers {
			if has[s.ID] {
				again = append(again, s)
			}
		}
		return t.discover(ctx, again, true), nil
	}
	return DiscoverResult{}, errors.New("name a server, or --all for every enabled configured server")
}

// testServer initializes a session and closes it, calling nothing.
func (t *Tool) testServer(ctx context.Context, ref string) (map[string]any, error) {
	s, err := t.server(ref)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	sess, err := client.Connect(ctx, s, t.options())
	if err != nil {
		return map[string]any{"id": s.ID, "ok": false, "unreachable": client.IsUnreachable(err), "error": err.Error()}, err
	}
	defer sess.Close()
	init := sess.Initialize()
	return map[string]any{"id": s.ID, "ok": true, "ms": time.Since(start).Milliseconds(), "serverInfo": init.ServerInfo, "protocolVersion": init.ProtocolVersion, "capabilities": init.Capabilities}, nil
}

// ToolRow is one saved tool of one server.
type ToolRow struct {
	Server      string   `json:"server"`     // the server ID
	ServerName  string   `json:"serverName"` // its configured name
	Tool        string   `json:"tool"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Hints       []string `json:"hints,omitempty"` // read-only, destructive, idempotent, open-world
	Params      []string `json:"params,omitempty"`
	Required    []string `json:"required,omitempty"`
	Stale       bool     `json:"stale,omitempty"`
	Configured  bool     `json:"configured"`
}

func toolHints(tl *mcp.Tool) []string {
	a := tl.Annotations
	if a == nil {
		return nil
	}
	var out []string
	if a.ReadOnlyHint {
		out = append(out, "read-only")
	}
	if a.DestructiveHint != nil && *a.DestructiveHint {
		out = append(out, "destructive")
	}
	if a.IdempotentHint {
		out = append(out, "idempotent")
	}
	if a.OpenWorldHint != nil && *a.OpenWorldHint {
		out = append(out, "open-world")
	}
	return out
}

// schemaParams are the property names and the required ones of a JSON
// schema object.
func schemaParams(schema any) (params, required []string) {
	m, _ := schema.(map[string]any)
	props, _ := m["properties"].(map[string]any)
	for k := range props {
		params = append(params, k)
	}
	sort.Strings(params)
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required = append(required, s)
			}
		}
	}
	return params, required
}

// toolRows are the saved tools of every snapshot (of server when set: a
// name or ID).
func (t *Tool) toolRows(server string) ([]ToolRow, error) {
	var snaps []client.Snapshot
	if server != "" {
		snap, err := t.snapshot(server)
		if err != nil {
			return nil, err
		}
		snaps = []client.Snapshot{snap}
	} else {
		var err error
		if snaps, err = t.store().List(); err != nil && len(snaps) == 0 {
			return nil, err
		}
	}
	servers, _ := client.Configured(t.options())
	current := map[string]client.Server{}
	for _, s := range servers {
		current[s.ID] = s
	}
	out := []ToolRow{}
	for _, snap := range snaps {
		cur, configured := current[snap.Server.ID]
		var c *client.Server
		if configured {
			c = &cur
		}
		stale := snapshotState(&snap, c) == "stale"
		for _, tl := range snap.Tools {
			params, required := schemaParams(tl.InputSchema)
			out = append(out, ToolRow{Server: snap.Server.ID, ServerName: snap.Server.Name, Tool: tl.Name, Title: tl.Title, Description: tl.Description, Hints: toolHints(tl), Params: params, Required: required, Stale: stale, Configured: configured})
		}
	}
	return out, nil
}

// toolDetail is one saved tool in full, with its example count and the
// command that calls it with the latest observed arguments.
func (t *Tool) toolDetail(server, tool string) (map[string]any, error) {
	snap, err := t.snapshot(server)
	if err != nil {
		return nil, err
	}
	tl, ok := snap.Tool(tool)
	if !ok {
		return nil, fmt.Errorf("%s has no saved tool %q (%d saved tools; discover again if it is new)", snap.Server.ID, tool, len(snap.Tools))
	}
	out := map[string]any{"server": snap.Server.ID, "serverName": snap.Server.Name, "discoveredAt": snap.DiscoveredAt, "tool": tl}
	examples, _ := t.examples(snap.Server.Name, tool, "", 0)
	out["examples"] = len(examples)
	args := map[string]any{}
	for _, e := range examples {
		if !e.IsError {
			args = e.Arguments
			break
		}
	}
	out["command"] = t.callCommand(snap.Server.Name, tool, args)
	return out, nil
}

// callCommand is the command line that calls tool with args.
func (t *Tool) callCommand(server, tool string, args map[string]any) string {
	b, _ := json.Marshal(args)
	return t.Prog + " call " + shellQuote(server) + " " + shellQuote(tool) + " --data " + shellQuote(string(b))
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// call calls one tool and records the call in the history.
func (t *Tool) call(ctx context.Context, ref, tool string, args map[string]any) (*mcp.CallToolResult, client.Server, error) {
	s, err := t.server(ref)
	if err != nil {
		return nil, s, err
	}
	start := t.now()
	rec := client.Call{At: start.UTC(), Server: s.ID, Tool: tool, Arguments: args}
	defer func() {
		rec.DurationMS = t.now().Sub(start).Milliseconds()
		if err := t.store().RecordCall(rec); err != nil {
			fmt.Fprintln(t.Stderr, "warning: call not recorded:", err)
		}
	}()
	sess, err := client.Connect(ctx, s, t.options())
	if err != nil {
		rec.Error = err.Error()
		return nil, s, err
	}
	defer sess.Close()
	res, err := sess.Call(ctx, tool, args)
	if err != nil {
		rec.Error = err.Error()
		return nil, s, err
	}
	rec.IsError = res.IsError
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	rec.Preview = preview(strings.Join(parts, "\n"))
	return res, s, nil
}

// examples are the harvested calls to server (a name as the harnesses
// spell it; any when "") and tool, of harness, newest first, at most n
// (all when n <= 0).
func (t *Tool) examples(server, tool, harness string, n int) ([]Example, error) {
	all, _, err := t.Harvest(false)
	if err != nil && len(all) == 0 {
		return nil, err
	}
	out := []Example{}
	for _, e := range all {
		if (server == "" || e.Server == server) && (tool == "" || e.Tool == tool) && (harness == "" || e.Harness == harness) {
			out = append(out, e)
			if n > 0 && len(out) >= n {
				break
			}
		}
	}
	return out, nil
}

// calls is o-mcp's own call history, of server (an ID or name) and tool.
func (t *Tool) calls(server, tool string) ([]client.Call, error) {
	all, err := t.store().Calls()
	if err != nil {
		return nil, err
	}
	out := []client.Call{}
	for _, c := range all {
		name := c.Server[strings.LastIndex(c.Server, ":")+1:]
		if (server == "" || c.Server == server || name == server) && (tool == "" || c.Tool == tool) {
			out = append(out, c)
		}
	}
	return out, nil
}
