package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"ofam.dev/o-mcp/client"
	"ofam.dev/ochazuke"
	ocli "ofam.dev/ochazuke/cli"
	"ofam.dev/ochazuke/keytest"
)

func TestMCPPagePayloads(t *testing.T) {
	for _, response := range []string{
		`{"values":[true,null,9007199254740993]}`,
		"message\n```\n## untrusted heading",
		`{"unfinished":`,
		"",
	} {
		arguments := map[string]any{"request": []any{"complete", true}}
		for _, row := range []ListRow{
			exampleRow(Example{Arguments: arguments, Result: response}),
			callRow(client.Call{Arguments: arguments, Preview: response}),
		} {
			b, err := json.Marshal(row.Data)
			if err != nil {
				t.Fatal(err)
			}
			var data map[string]any
			if err := json.Unmarshal(b, &data); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(data["arguments"], arguments) || (data["result"] != response && !(response == "" && data["result"] == nil)) {
				t.Fatalf("row must carry unchanged request/response data: %s", b)
			}
			if _, ok := data["payload"]; ok {
				t.Fatalf("row still carries the Markdown projection: %s", b)
			}
			if strings.HasPrefix(row.ID, callNoun+"/") && response != "" && data["preview"] != response {
				t.Fatalf("call preview contract changed: %s", b)
			}
		}
	}
}

func TestMCPPageJSONSections(t *testing.T) {
	e := newEnv(t)
	var doc ocli.CatalogDoc
	if err := json.Unmarshal([]byte(e.ok("catalog")), &doc); err != nil {
		t.Fatal(err)
	}
	sections := doc.Pages[0].Detail.Sections
	if len(sections) != 3 || sections[0].Kind != "fields" {
		t.Fatalf("metadata must precede two JSON sections: %+v", sections)
	}
	want := []ocli.CatalogDetailSection{
		{Kind: "json", Title: "Request", Field: "arguments"},
		{Kind: "json", Title: "Response", Field: "result", Parse: true},
	}
	if !reflect.DeepEqual(sections[1:], want) {
		t.Fatalf("JSON sections = %+v, want %+v", sections[1:], want)
	}
}

func TestExampleTranscriptCompatibility(t *testing.T) {
	for _, input := range []string{
		`{"path":"~/sessions/old.jsonl","cwd":"/project"}`,
		`{"transcript":"~/sessions/old.jsonl","path":"ignored","cwd":"/project"}`,
	} {
		var example Example
		if err := json.Unmarshal([]byte(input), &example); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(example)
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		json.Unmarshal(b, &data)
		if data["transcript"] != "~/sessions/old.jsonl" || data["cwd"] != "/project" || data["path"] != nil {
			t.Fatalf("legacy input must emit transcript without a competing project path: %s", b)
		}
	}
	// Existing version-1 caches must work without forcing a rescan.
	e := newEnv(t)
	e.ok("harvest")
	b, err := os.ReadFile(e.tool.harvestPath())
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.ReplaceAll(string(b), `"transcript":`, `"path":`))
	if err := os.WriteFile(e.tool.harvestPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	examples, report, err := e.tool.Harvest(false)
	if err != nil || report.Read != 0 || len(examples) != 4 {
		t.Fatalf("cached harvest = %+v, %v", report, err)
	}
	for _, row := range rows(t, e.ok("mcp-example", "list")) {
		data := row.Data.(map[string]any)
		if data["transcript"] == nil || data["path"] != nil {
			t.Fatalf("cached example = %#v", data)
		}
	}
}

// mountedMCPPage exercises the same JSON wire shape a catalog host receives.
func mountedMCPPage(t *testing.T, row ListRow, width, height int) *ochazuke.Model {
	t.Helper()
	e := newEnv(t)
	var doc ocli.CatalogDoc
	if err := json.Unmarshal([]byte(e.ok("catalog")), &doc); err != nil {
		t.Fatal(err)
	}
	r := ochazuke.NewRuntime()
	r.Config().Disable()
	m := ochazuke.NewModel(r)
	m.Width, m.Height = width, height
	m.Appearance.Theme = "mono"
	views, err := ocli.MountPages(m, doc, ocli.PageConfig{})
	if err != nil || len(views) != 1 {
		t.Fatalf("mount: %v, %v", views, err)
	}
	wire, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var selected ocli.Row
	if err := json.Unmarshal(wire, &selected); err != nil {
		t.Fatal(err)
	}
	selected.Noun, _, _ = strings.Cut(row.ID, "/")
	v := views[0]
	if err := v.SetState([]ocli.Row{selected}); err != nil {
		t.Fatal(err)
	}
	if err := r.OpenPage(v.Page()); err != nil {
		t.Fatal(err)
	}
	list := "mcp-examples"
	if selected.Noun == callNoun {
		list = "mcp-calls"
	}
	if err := r.SetFocus(list + "/" + row.ID); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMCPPageStories(t *testing.T) {
	arguments := map[string]any{"nested": map[string]any{"text": "request-value"}}
	response := `{"nested":{"text":"response-value","n":9007199254740993}}`
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, row := range []ListRow{
		exampleRow(Example{ID: "fixture", Harness: "claude", Server: "fake", Tool: "echo", At: at,
			Session: "session-fixture", Cwd: "/project", Transcript: "~/sessions/fixture.jsonl",
			Arguments: arguments, Result: response}),
		callRow(client.Call{ID: "fixture", Server: "fake", Tool: "echo", At: at, Arguments: arguments, Preview: response}),
	} {
		for _, width := range []int{150, 78} {
			t.Run(row.ID+"/"+strconv.Itoa(width), func(t *testing.T) {
				m := mountedMCPPage(t, row, width, 65)
				r := m.Runtime
				frame := ansi.Strip(m.View().Content)
				for _, want := range []string{"Metadata", "Request"} {
					if !strings.Contains(frame, want) {
						t.Fatalf("detail lacks %q:\n%s", want, frame)
					}
				}
				if strings.HasPrefix(row.ID, exampleNoun+"/") {
					for _, want := range []string{"session-fixture", "/project", "~/sessions/fixture.jsonl", "false"} {
						if !strings.Contains(frame, want) {
							t.Fatalf("example metadata lacks %q:\n%s", want, frame)
						}
					}
				}
				if strings.Index(frame, "Metadata") > strings.Index(frame, "Request") {
					t.Fatal("metadata must precede the request/response")
				}
				for _, section := range []struct{ title, value string }{{"Request", "request-value"}, {"Response", "response-value"}} {
					// Focus the tree and select its nested object using shared bindings.
					for _, key := range []string{"ctrl+w", "l", "j"} {
						m.Update(keytest.Msg(key, false, false))
					}
					frame = ansi.Strip(m.View().Content)
					for _, want := range []string{section.title, "Structure", "Data · .nested", section.value} {
						if !strings.Contains(frame, want) {
							t.Fatalf("JSON detail lacks %q:\n%s", want, frame)
						}
					}
					sideBySide := false
					for _, line := range strings.Split(frame, "\n") {
						if strings.Contains(line, "Structure") && strings.Contains(line, "Data ·") {
							sideBySide = true
						}
					}
					if sideBySide != (width == 150) {
						t.Fatalf("structure/data layout at width %d:\n%s", width, frame)
					}
					if section.title == "Response" && !strings.Contains(frame, "9007199254740993") {
						t.Fatalf("response number changed:\n%s", frame)
					}
				}
				if _, err := r.Settings().SetSession("detail.sections", false); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"9", "9", "9", "K"} {
					m.Update(keytest.Msg(key, false, false))
				}
				frame = ansi.Strip(m.View().Content)
				if !strings.Contains(frame, "arguments") || strings.Contains(frame, "Structure") || strings.Contains(frame, "Metadata") {
					t.Fatalf("legacy opt-out:\n%s", frame)
				}
			})
		}
	}
}

func TestMCPPageResponseFallback(t *testing.T) {
	for _, response := range []string{"prose first line\n```\n## untrusted heading", `{"unfinished":`, ""} {
		for _, row := range []ListRow{
			exampleRow(Example{ID: "fallback", Arguments: map[string]any{}, Result: response}),
			callRow(client.Call{ID: "fallback", Arguments: map[string]any{}, Preview: response}),
		} {
			m := mountedMCPPage(t, row, 150, 150)
			frame := ansi.Strip(m.View().Content)
			if !strings.Contains(frame, "Response") {
				t.Fatalf("missing response section:\n%s", frame)
			}
			for _, line := range strings.Split(response, "\n") {
				if !strings.Contains(frame, line) {
					t.Fatalf("fallback lacks %q:\n%s", line, frame)
				}
			}
		}
	}
}

func TestMCPPageBindings(t *testing.T) {
	e := newEnv(t)
	var doc ocli.CatalogDoc
	json.Unmarshal([]byte(e.ok("catalog")), &doc)
	want := map[string]string{serverNoun + ":g d": "mcp-server.discover", serverNoun + ":g r": "mcp-server.refresh", exampleNoun + ":g a": "mcp-example.again"}
	got := map[string]string{}
	r := ochazuke.NewRuntime()
	r.Config().Disable()
	m := ochazuke.NewModel(r)
	for _, binding := range doc.Bindings {
		key := binding.Noun + ":" + binding.Keys
		if _, ok := want[key]; !ok {
			continue
		}
		got[key] = binding.Request.Key()
		for _, other := range doc.Bindings {
			if other.Noun != "ochazuke.list" && other.Noun != "ochazuke.page" && other.Noun != "ochazuke.app" {
				continue
			}
			if other.Mode != "normal" && other.Mode != "" || strings.HasSuffix(other.Keys, " esc") || other.Keys == "" {
				continue
			}
			if strings.HasPrefix(binding.Keys+" ", other.Keys+" ") || strings.HasPrefix(other.Keys+" ", binding.Keys+" ") {
				t.Errorf("%s clashes with ancestor %s %s", key, other.Noun, other.Keys)
			}
		}
		ran := ""
		r.Define(binding.Request.Noun).Verb(binding.Request.Verb, func(c ochazuke.Ctx) ochazuke.Result {
			ran = c.Instance.ID
			return ochazuke.Done(nil)
		}, "fixture").Fetch()
		if err := r.Bind(binding); err != nil {
			t.Fatal(err)
		}
		id := "fixture/" + key
		list, err := r.AddList(id, []ochazuke.ListItem{{ID: id + "/row", Noun: binding.Noun}})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SetFocus(list.FocusTarget()); err != nil {
			t.Fatal(err)
		}
		for _, key := range strings.Fields(binding.Keys) {
			m.Update(keytest.Msg(key, false, false))
		}
		if ran != id+"/row" {
			t.Fatalf("%s dispatch target = %q", key, ran)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog bindings = %v, want %v", got, want)
	}
}
