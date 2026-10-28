package main

import (
	"strconv"
	"strings"

	"github.com/unixsuperhero/o-mcp/client"
	"github.com/unixsuperhero/ochazuke"
)

// Tables: every row noun declares its columns (the catalog's columns) and
// every row carries one cell per column, so a host draws the list as a
// table. A cell is a field the row already holds.

type table struct {
	columns []ochazuke.Column
	cells   func(any) []string
}

var right = ochazuke.AlignRight

var tables = map[string]table{
	serverNoun: {
		columns: []ochazuke.Column{{Title: "server", Min: 8}, {Title: "harness", Priority: 1}, {Title: "scope", Priority: 4}, {Title: "transport", Priority: 3}, {Title: "state", Priority: 1}, {Title: "tools", Align: right, Priority: 2}, {Title: "address", Priority: 5}},
		cells: func(v any) []string {
			r := v.(ServerRow)
			state := "enabled"
			if r.Disabled {
				state = "disabled"
			}
			if !r.Configured {
				state = "not configured"
			}
			if r.Snapshot == "stale" {
				state += ", stale"
			}
			switch r.Auth {
			case client.SignedIn:
				state += ", signed in"
			case client.NeedsSignIn:
				state += ", needs sign-in"
			}
			tools := ""
			if r.Tools >= 0 {
				tools = strconv.Itoa(r.Tools)
			}
			return []string{r.Name, r.Harness, r.Scope, r.Transport, state, tools, r.Address()}
		},
	},
	toolNoun: {
		// A tool is named by its server and its name: both pinned.
		columns: []ochazuke.Column{{Title: "server", Min: 6, Priority: 1, Pinned: true}, {Title: "tool", Min: 8, Pinned: true}, {Title: "hints", Priority: 3}, {Title: "description", Priority: 2}},
		cells: func(v any) []string {
			r := v.(ToolRow)
			return []string{r.ServerName, r.Tool, strings.Join(r.Hints, ","), firstLine(r.Description)}
		},
	},
	callNoun: {
		columns: []ochazuke.Column{{Title: "at", Width: 16}, {Title: "server", Priority: 1}, {Title: "tool", Min: 8}, {Title: "result", Priority: 2}, {Title: "ms", Align: right, Priority: 3}},
		cells: func(v any) []string {
			c := v.(client.Call)
			res := "ok"
			switch {
			case c.Error != "":
				res = "failed"
			case c.IsError:
				res = "error"
			}
			return []string{c.At.Local().Format("2006-01-02 15:04"), c.Server, c.Tool, res, strconv.FormatInt(c.DurationMS, 10)}
		},
	},
	exampleNoun: {
		columns: []ochazuke.Column{{Title: "at", Width: 16}, {Title: "harness", Priority: 2}, {Title: "server", Priority: 1}, {Title: "tool", Min: 8}, {Title: "result", Priority: 3}, {Title: "arguments", Priority: 4}},
		cells: func(v any) []string {
			e := v.(Example)
			res := "ok"
			if e.IsError {
				res = "error"
			}
			return []string{e.At.Local().Format("2006-01-02 15:04"), e.Harness, e.Server, e.Tool, res, firstLine(compactJSON(e.Arguments))}
		},
	},
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
