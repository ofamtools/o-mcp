package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ofam.dev/o-ai/harness"
	"ofam.dev/o-mcp/client"
)

// Examples are real calls the harnesses made while someone worked, read
// from their session logs (h-mcp's harvest): Claude's tool_use blocks named
// mcp__SERVER__TOOL joined to their tool_result, Codex's McpToolCall items
// (and the older mcp_tool_call_end events). Reading never connects to a
// server. The logs are large (gigabytes), so each file's examples are kept
// in a cache keyed by its size and modification time and a file is read
// again only when it changed.

// Example is one observed call.
type Example struct {
	ID         string         `json:"id"` // HARNESS:CALL_ID
	Harness    string         `json:"harness"`
	Server     string         `json:"server"` // the server's name as the harness spelled it
	Tool       string         `json:"tool"`
	At         time.Time      `json:"at"`
	Session    string         `json:"session,omitempty"`
	Transcript string         `json:"transcript"`
	Cwd        string         `json:"cwd,omitempty"`
	Arguments  map[string]any `json:"arguments"`
	IsError    bool           `json:"isError"`
	Result     string         `json:"result,omitempty"` // the start of the result's text
}

// UnmarshalJSON accepts the old transcript field for one release. Only
// transcript is written: a host interprets path as the record's project.
func (e *Example) UnmarshalJSON(b []byte) error {
	type plain Example
	var record struct {
		plain
		Path string `json:"path"`
	}
	if err := json.Unmarshal(b, &record); err != nil {
		return err
	}
	*e = Example(record.plain)
	if e.Transcript == "" {
		e.Transcript = record.Path
	}
	return nil
}

const resultPreview = 2048

type harvestFile struct {
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"modTime"`
	Examples []Example `json:"examples"`
}

type harvestCache struct {
	Version int                    `json:"version"`
	Files   map[string]harvestFile `json:"files"`
}

const harvestVersion = 1

// HarvestReport says what a harvest read.
type HarvestReport struct {
	Files    int `json:"files"`
	Read     int `json:"read"`
	Examples int `json:"examples"`
}

// sessionLogs are the session files of each harness o-mcp reads.
func (t *Tool) sessionLogs() map[string][]string {
	out := map[string][]string{}
	walk := func(harness, root string) {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // a missing or unreadable directory has no logs
			}
			if !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
				out[harness] = append(out[harness], p)
			}
			return nil
		})
	}
	walk("claude", filepath.Join(t.harnessRoot("claude"), "projects"))
	codex := t.harnessRoot("codex")
	walk("codex", filepath.Join(codex, "sessions"))
	walk("codex", filepath.Join(codex, "archived_sessions"))
	return out
}

// harnessRoot is a harness's directory under the tool's home (o-ai's
// harness.Root, which follows CLAUDE_CONFIG_DIR and CODEX_HOME).
func (t *Tool) harnessRoot(id string) string { return harness.Root(id, t.Home) }

func (t *Tool) harvestPath() string { return filepath.Join(t.CacheDir(), "examples.json") }

// Harvest reads the changed session files (every file with rescan) and
// returns every example, newest first.
func (t *Tool) Harvest(rescan bool) ([]Example, HarvestReport, error) {
	cache := harvestCache{Version: harvestVersion, Files: map[string]harvestFile{}}
	if !rescan {
		if b, err := os.ReadFile(t.harvestPath()); err == nil {
			var c harvestCache
			if json.Unmarshal(b, &c) == nil && c.Version == harvestVersion && c.Files != nil {
				cache = c
			}
		}
	}
	var rep HarvestReport
	next := map[string]harvestFile{}
	changed := rescan
	for h, files := range t.sessionLogs() {
		for _, p := range files {
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			rep.Files++
			if f, ok := cache.Files[p]; ok && f.Size == info.Size() && f.ModTime.Equal(info.ModTime()) {
				next[p] = f
				continue
			}
			rep.Read++
			changed = true
			var ex []Example
			switch h {
			case "claude":
				ex = claudeExamples(p)
			case "codex":
				ex = codexExamples(p)
			}
			for i := range ex {
				ex[i].Transcript = t.tilde(p)
			}
			next[p] = harvestFile{Size: info.Size(), ModTime: info.ModTime(), Examples: ex}
		}
	}
	if len(next) != len(cache.Files) {
		changed = true // a file went away
	}
	var all []Example
	for _, f := range next {
		all = append(all, f.Examples...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].At.Equal(all[j].At) {
			return all[i].At.After(all[j].At)
		}
		return all[i].ID < all[j].ID
	})
	rep.Examples = len(all)
	if changed {
		b, err := json.Marshal(harvestCache{Version: harvestVersion, Files: next})
		if err != nil {
			return all, rep, err
		}
		if err := writeFileAtomic(t.harvestPath(), b); err != nil {
			return all, rep, err
		}
	}
	return all, rep, nil
}

// splitClaudeName is SERVER and TOOL of mcp__SERVER__TOOL.
func splitClaudeName(name string) (server, tool string, ok bool) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return "", "", false
	}
	server, tool, ok = strings.Cut(rest, "__")
	return server, tool, ok && server != "" && tool != ""
}

// eachLine calls f with every line of a file (lines up to 64 MiB).
func eachLine(path string, f func([]byte)) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		f(sc.Bytes())
	}
}

func claudeExamples(path string) []Example {
	type block struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     map[string]any  `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	type line struct {
		Timestamp time.Time `json:"timestamp"`
		SessionID string    `json:"sessionId"`
		Cwd       string    `json:"cwd"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	var out []Example
	pending := map[string]int{} // tool_use id -> index in out
	eachLine(path, func(b []byte) {
		isUse := bytes.Contains(b, []byte(`"name":"mcp__`)) && bytes.Contains(b, []byte(`"tool_use"`))
		isResult := false
		if len(pending) > 0 && bytes.Contains(b, []byte(`"tool_result"`)) {
			for id := range pending {
				if bytes.Contains(b, []byte(id)) {
					isResult = true
					break
				}
			}
		}
		if !isUse && !isResult {
			return
		}
		var l line
		if json.Unmarshal(b, &l) != nil {
			return
		}
		var blocks []block
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			return
		}
		for _, bl := range blocks {
			switch bl.Type {
			case "tool_use":
				server, tool, ok := splitClaudeName(bl.Name)
				if !ok || bl.ID == "" {
					continue
				}
				args := bl.Input
				if args == nil {
					args = map[string]any{}
				}
				pending[bl.ID] = len(out)
				out = append(out, Example{ID: "claude:" + bl.ID, Harness: "claude", Server: server, Tool: tool, At: l.Timestamp.UTC(), Session: l.SessionID, Cwd: l.Cwd, Arguments: redactArgs(args)})
			case "tool_result":
				i, ok := pending[bl.ToolUseID]
				if !ok {
					continue
				}
				delete(pending, bl.ToolUseID)
				out[i].IsError = bl.IsError
				out[i].Result = preview(contentText(bl.Content))
			}
		}
	})
	return out
}

func codexExamples(path string) []Example {
	type result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	type line struct {
		Timestamp time.Time `json:"timestamp"`
		Payload   struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     struct {
				Type      string         `json:"type"`
				ID        string         `json:"id"`
				Server    string         `json:"server"`
				Tool      string         `json:"tool"`
				Arguments map[string]any `json:"arguments"`
				Status    string         `json:"status"`
				Result    *result        `json:"result"`
				Error     any            `json:"error"`
			} `json:"item"`
			// mcp_tool_call_end (older rollouts).
			CallID     string `json:"call_id"`
			Invocation struct {
				Server    string         `json:"server"`
				Tool      string         `json:"tool"`
				Arguments map[string]any `json:"arguments"`
			} `json:"invocation"`
			Result struct {
				Ok  *result `json:"Ok"`
				Err any     `json:"Err"`
			} `json:"result"`
		} `json:"payload"`
	}
	text := func(r *result) string {
		if r == nil {
			return ""
		}
		var parts []string
		for _, c := range r.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	var out []Example
	eachLine(path, func(b []byte) {
		if !bytes.Contains(b, []byte(`"McpToolCall"`)) && !bytes.Contains(b, []byte(`"mcp_tool_call_end"`)) {
			return
		}
		var l line
		if json.Unmarshal(b, &l) != nil {
			return
		}
		p := l.Payload
		switch {
		case p.Type == "item_completed" && p.Item.Type == "McpToolCall" && p.Item.Server != "":
			args := p.Item.Arguments
			if args == nil {
				args = map[string]any{}
			}
			isErr := p.Item.Status == "failed" || p.Item.Error != nil || (p.Item.Result != nil && p.Item.Result.IsError)
			sess := p.ThreadID
			if sess == "" {
				sess = session
			}
			out = append(out, Example{ID: "codex:" + p.Item.ID, Harness: "codex", Server: p.Item.Server, Tool: p.Item.Tool, At: l.Timestamp.UTC(), Session: sess, Arguments: redactArgs(args), IsError: isErr, Result: preview(text(p.Item.Result))})
		case p.Type == "mcp_tool_call_end" && p.Invocation.Server != "":
			args := p.Invocation.Arguments
			if args == nil {
				args = map[string]any{}
			}
			res := p.Result.Ok
			isErr := p.Result.Err != nil || (res != nil && res.IsError)
			out = append(out, Example{ID: "codex:" + p.CallID, Harness: "codex", Server: p.Invocation.Server, Tool: p.Invocation.Tool, At: l.Timestamp.UTC(), Session: session, Arguments: redactArgs(args), IsError: isErr, Result: preview(text(res))})
		}
	})
	return out
}

func redactArgs(args map[string]any) map[string]any {
	m, _ := client.RedactValues(args).(map[string]any)
	return m
}

// contentText is a tool_result's content as text: a string, or its text
// blocks joined.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func preview(s string) string {
	s = client.RedactValues(s).(string)
	if len(s) <= resultPreview {
		return s
	}
	cut := resultPreview
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // not inside a UTF-8 sequence
		cut--
	}
	return s[:cut] + "…"
}
