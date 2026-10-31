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
	for _, tc := range []struct {
		name, response string
		want           any
	}{
		{"object", `{"values":[true,null,9007199254740993]}`, map[string]any{"values": []any{true, nil, json.Number("9007199254740993")}}},
		{"prose", "message\n```\n## untrusted heading", "message\n```\n## untrusted heading"},
		{"truncated", `{"unfinished":`, `{"unfinished":`},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arguments := map[string]any{"request": []any{"complete", true}}
			for _, row := range []ListRow{
				exampleRow(Example{Arguments: arguments, Result: tc.response}),
				callRow(client.Call{Arguments: arguments, Preview: tc.response}),
			} {
				b, err := json.Marshal(row.Data)
				if err != nil {
					t.Fatal(err)
				}
				var data map[string]any
				json.Unmarshal(b, &data)
				payload, _ := data["payload"].(string)
				blocks := strings.Split(payload, "```json\n")
				if len(blocks) != 3 {
					t.Fatalf("need two fenced JSON blocks: %q", payload)
				}
				for i, want := range []any{arguments, tc.want} {
					body, rest, ok := strings.Cut(blocks[i+1], "\n```")
					if !ok || strings.Contains(rest, "```") {
						t.Fatalf("invalid or escaped fence: %q", payload)
					}
					var got any
					decoder := json.NewDecoder(strings.NewReader(body))
					decoder.UseNumber()
					if err := decoder.Decode(&got); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("block %d = %#v, want %#v", i, got, want)
					}
				}
			}
		})
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

func TestMCPPageStories(t *testing.T) {
	e := newEnv(t)
	var doc ocli.CatalogDoc
	if err := json.Unmarshal([]byte(e.ok("catalog")), &doc); err != nil {
		t.Fatal(err)
	}
	for _, width := range []int{150, 78} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			r := ochazuke.NewRuntime()
			r.Config().Disable()
			m := ochazuke.NewModel(r)
			m.Width, m.Height = width, 65
			m.Appearance.Theme = "mono"
			views, err := ocli.MountPages(m, doc, ocli.PageConfig{})
			if err != nil || len(views) != 1 {
				t.Fatalf("mount: %v, %v", views, err)
			}
			v := views[0]
			row := exampleRow(Example{
				ID: "fixture", Harness: "claude", Server: "fake", Tool: "echo",
				At: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), Session: "session-fixture",
				Cwd: "/project", Transcript: "~/sessions/fixture.jsonl",
				Arguments: map[string]any{"nested": map[string]any{"text": "request-value"}},
				Result:    `{"nested":{"text":"response-value","n":9007199254740993}}`,
			})
			// A host sees the JSON wire shape, rather than Go structs.
			wire, _ := json.Marshal(row)
			var selected ocli.Row
			json.Unmarshal(wire, &selected)
			selected.Noun = exampleNoun
			if err := v.SetState([]ocli.Row{selected}); err != nil {
				t.Fatal(err)
			}
			if err := r.OpenPage(v.Page()); err != nil {
				t.Fatal(err)
			}
			if err := r.SetFocus("mcp-examples/" + row.ID); err != nil {
				t.Fatal(err)
			}
			frame := ansi.Strip(m.View().Content)
			for _, want := range []string{"Metadata", "session-fixture", "/project", "~/sessions/fixture.jsonl", "false", "Arguments"} {
				if !strings.Contains(frame, want) {
					t.Errorf("detail lacks %q:\n%s", want, frame)
				}
			}
			if strings.Index(frame, "Metadata") > strings.Index(frame, "Arguments") {
				t.Error("metadata must precede the request/response")
			}
			// Result is below the fold: the existing detail scroll must reach it.
			for _, key := range []string{"9", "9", "9", "J"} {
				m.Update(keytest.Msg(key, false, false))
			}
			frame = ansi.Strip(m.View().Content)
			for _, want := range []string{"request-value", "Result", "response-value", "9007199254740993"} {
				if !strings.Contains(frame, want) {
					t.Errorf("scrolled detail lacks %q:\n%s", want, frame)
				}
			}
			if _, err := r.Settings().SetSession("detail.sections", false); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"9", "9", "9", "K"} {
				m.Update(keytest.Msg(key, false, false))
			}
			if frame := ansi.Strip(m.View().Content); !strings.Contains(frame, "arguments") || strings.Contains(frame, "Metadata") {
				t.Fatalf("legacy opt-out:\n%s", frame)
			}
		})
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
