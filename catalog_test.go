package main

import (
	"encoding/json"
	"testing"
)

// TestCatalog: the catalog answers, every row noun has a list verb that
// declares its rows, every verb declares its history kind, and the MCP
// page lists the four nouns.
func TestCatalog(t *testing.T) {
	e := newEnv(t)
	var doc struct {
		Schema int    `json:"schema"`
		State  string `json:"state"`
		Nouns  []struct {
			Name  string `json:"name"`
			Verbs []struct {
				Verb    string `json:"name"`
				Rows    string `json:"rows"`
				History string `json:"history"`
				Builtin bool   `json:"builtin"`
			} `json:"verbs"`
		} `json:"nouns"`
		Pages []struct {
			ID    string `json:"id"`
			Lists []struct {
				Noun string `json:"noun"`
			} `json:"lists"`
		} `json:"pages"`
	}
	if err := json.Unmarshal([]byte(e.ok("catalog")), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != 1 || doc.State != "status.state" {
		t.Fatalf("schema %d, state %q", doc.Schema, doc.State)
	}
	listRows := map[string]string{}
	for _, n := range doc.Nouns {
		for _, v := range n.Verbs {
			if v.Builtin {
				continue
			}
			if v.History == "" {
				t.Errorf("%s.%s declares no history kind", n.Name, v.Verb)
			}
			if v.Verb == "list" {
				listRows[n.Name] = v.Rows
			}
		}
	}
	for _, noun := range rowNouns {
		if listRows[noun] != noun {
			t.Errorf("%s.list rows = %q", noun, listRows[noun])
		}
	}
	if len(doc.Pages) != 1 || doc.Pages[0].ID != "mcp" || len(doc.Pages[0].Lists) != 4 {
		t.Fatalf("pages = %+v", doc.Pages)
	}
}
