package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/unixsuperhero/o-mcp/internal/fake"
	"github.com/unixsuperhero/oden"
)

func TestMain(m *testing.M) {
	fake.Main() // the test binary is the stdio fake server when asked
	os.Exit(m.Run())
}

// env is a test's tool: a temporary home with testdata/home copied in,
// a .claude.json whose fake server is this test binary, and every data and
// cache path inside the home. Nothing reads or writes the real home.
type env struct {
	t    *testing.T
	tool *Tool
	home string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_CONFIG_HOME", "PI_CODING_AGENT_DIR", "XDG_DATA_HOME", "XDG_CACHE_HOME", "O_MCP_DATA"} {
		t.Setenv(k, "")
	}
	home := t.TempDir()
	err := filepath.Walk("testdata/home", func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("testdata/home", p)
		dst := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(dst), 0o755)
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, home: home}
	e.writeClaude(true)
	e.tool = &Tool{Home: home, Cwd: t.TempDir(), Prog: "o-mcp", Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "O_MCP_CLIPBOARD=true"}}
	return e
}

// writeClaude writes .claude.json: the fake stdio server (when fakeOn),
// and an OAuth-only remote connector.
func (e *env) writeClaude(fakeOn bool) {
	servers := map[string]any{"oauth-only": map[string]any{"type": "http", "url": "https://connector.example.test/mcp"}}
	if fakeOn {
		servers["fake"] = map[string]any{"command": os.Args[0], "env": map[string]any{fake.EnvStdio: "1", "FAKE_ENV": "${FAKE_SOURCE:-default}"}}
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": servers})
	if err := os.WriteFile(filepath.Join(e.home, ".claude.json"), b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// run runs one command; it returns stdout, stderr and the exit code.
func (e *env) run(stdin string, args ...string) (string, string, int) {
	var out, errOut bytes.Buffer
	e.tool.Stdin, e.tool.Stdout, e.tool.Stderr = strings.NewReader(stdin), &out, &errOut
	err := e.tool.Main(context.Background(), args)
	code := oden.Report(&errOut, "o-mcp", err)
	return out.String(), errOut.String(), code
}

func (e *env) ok(args ...string) string {
	e.t.Helper()
	out, errOut, code := e.run("", args...)
	if code != 0 {
		e.t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
	}
	return out
}

func rows(t *testing.T, out string) []ListRow {
	t.Helper()
	var rs []ListRow
	if err := json.Unmarshal([]byte(out), &rs); err != nil {
		t.Fatalf("not rows: %v\n%s", err, out)
	}
	return rs
}

func ids(rs []ListRow) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func TestConfiguredListsEveryHarnessDisabledToo(t *testing.T) {
	e := newEnv(t)
	got := ids(rows(t, e.ok("configured")))
	want := []string{"mcp-server/claude:user:fake", "mcp-server/claude:user:oauth-only", "mcp-server/codex:user:off", "mcp-server/codex:user:remote"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("configured = %v, want %v", got, want)
	}
	if got := ids(rows(t, e.ok("configured", "--state", "disabled"))); !reflect.DeepEqual(got, []string{"mcp-server/codex:user:off"}) {
		t.Fatalf("disabled view = %v", got)
	}
	if got := ids(rows(t, e.ok("mcp-server", "list", "--harness", "codex", "--state", "enabled"))); !reflect.DeepEqual(got, []string{"mcp-server/codex:user:remote"}) {
		t.Fatalf("codex enabled = %v", got)
	}
	out := e.ok("mcp-server", "show", "remote")
	if !strings.Contains(out, `"bearerTokenEnv": "REMOTE_TOKEN"`) {
		t.Fatalf("show remote = %s", out)
	}
}

func TestDiscoverSavesSchemasUsableOffline(t *testing.T) {
	e := newEnv(t)
	var res DiscoverResult
	json.Unmarshal([]byte(e.ok("discover", "fake")), &res)
	if len(res.Servers) != 1 || res.Servers[0].Tools != 3 {
		t.Fatalf("discover = %+v", res)
	}
	if got := ids(rows(t, e.ok("tools", "fake"))); !reflect.DeepEqual(got, []string{"mcp-tool/claude:user:fake/echo", "mcp-tool/claude:user:fake/fail", "mcp-tool/claude:user:fake/json"}) {
		t.Fatalf("tools = %v", got)
	}
	// The source config loses the server: the saved schemas still answer.
	e.writeClaude(false)
	out := e.ok("tool", "fake", "echo")
	if !strings.Contains(out, `"inputSchema"`) || !strings.Contains(out, `o-mcp call fake echo --data`) {
		t.Fatalf("tool offline = %s", out)
	}
	if out := e.ok("mcp-tool", "schema", "--target", "mcp-tool/claude:user:fake/echo"); !strings.Contains(out, `"properties"`) {
		t.Fatalf("schema by target = %s", out)
	}
	srv := rows(t, e.ok("configured", "--state", "saved"))
	if len(srv) != 1 || !strings.Contains(srv[0].Label, "not configured") {
		t.Fatalf("saved, unconfigured = %+v", srv)
	}
	if out := e.ok("mcp-tool", "copy-schema", "fake", "echo"); !strings.Contains(out, `"copied": true`) {
		t.Fatalf("copy-schema = %s", out)
	}
}

func TestDiscoverAllContinuesAndMarksUnreachable(t *testing.T) {
	e := newEnv(t)
	var res DiscoverResult
	json.Unmarshal([]byte(e.ok("discover", "--all")), &res)
	if len(res.Servers) != 1 || res.Servers[0].ID != "claude:user:fake" {
		t.Fatalf("servers = %+v", res.Servers)
	}
	errs := map[string]DiscoverError{}
	for _, er := range res.Errors {
		errs[er.ID] = er
	}
	if _, ok := errs["codex:user:off"]; ok {
		t.Fatal("a disabled server is skipped in a batch, not an error")
	}
	if er, ok := errs["codex:user:remote"]; !ok || !strings.Contains(er.Error, "REMOTE_TOKEN") {
		t.Fatalf("remote = %+v", er)
	}
	if _, ok := errs["claude:user:oauth-only"]; !ok {
		t.Fatalf("oauth-only missing from errors: %+v", res.Errors)
	}
	if _, errOut, code := e.run("", "discover", "off"); code == 0 || !strings.Contains(errOut, "disabled") {
		t.Fatalf("discover a disabled server: %d %s", code, errOut)
	}
}

func TestCallArgumentsAndResults(t *testing.T) {
	e := newEnv(t)
	data := func(out string) map[string]any {
		var v map[string]any
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return v
	}
	got := data(e.ok("call", "fake", "echo", "--data", `{"text":"inline"}`, "--extract"))
	if !reflect.DeepEqual(got["args"], map[string]any{"text": "inline"}) || got["env"] != "default" {
		t.Fatalf("--data = %v", got)
	}
	out, errOut, code := e.run(`{"text":"stdin","n":1}`, "call", "fake", "echo", "--args-file", "-", "n=2", "extra=x", "--extract")
	if code != 0 {
		t.Fatalf("stdin: %d %s", code, errOut)
	}
	if got := data(out); !reflect.DeepEqual(got["args"], map[string]any{"text": "stdin", "n": 2.0, "extra": "x"}) {
		t.Fatalf("--args-file - = %v", got)
	}
	full := data(e.ok("call", "fake", "echo"))
	if _, ok := full["structuredContent"]; !ok {
		t.Fatalf("full result = %v", full)
	}
	out, errOut, code = e.run("", "call", "fake", "fail", "--extract")
	if code != 1 || !strings.Contains(out, `"isError": true`) || !strings.Contains(errOut, "isError") {
		t.Fatalf("tool error: %d %s %s", code, out, errOut)
	}
	if _, errOut, code := e.run("", "call", "fake", "echo", "--data", "[1]"); code == 0 || !strings.Contains(errOut, "JSON object") {
		t.Fatalf("array args: %d %s", code, errOut)
	}
	if _, errOut, code := e.run("", "call", "fake", "echo", "--data", "{}", "--args-file", "-"); code == 0 || !strings.Contains(errOut, "pass one") {
		t.Fatalf("both: %d %s", code, errOut)
	}
	if _, _, code := e.run("", "call", "fake"); code == 0 {
		t.Fatal("call without TOOL succeeded")
	}
	calls := rows(t, e.ok("calls", "fake"))
	if len(calls) != 4 || !strings.HasPrefix(calls[0].Label, "fake fail") || !strings.Contains(calls[0].Label, "error") {
		t.Fatalf("calls = %+v", calls)
	}
	if got := rows(t, e.ok("mcp-tool", "calls", "fake", "fail")); len(got) != 1 {
		t.Fatalf("tool calls = %v", ids(got))
	}
}

func TestParseCallKeyValues(t *testing.T) {
	tool := &Tool{Prog: "o-mcp", Stdin: strings.NewReader("")}
	c, err := tool.parseCall([]string{"srv", "tl", "a=1", "b.c=true", "--timeout", "2.5", "--extract"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c.args)
	if c.server != "srv" || c.tool != "tl" || string(b) != `{"a":1,"b":{"c":true}}` || !c.extract || c.timeout.Seconds() != 2.5 {
		t.Fatalf("parsed = %+v %s", c, b)
	}
	for _, bad := range [][]string{{"s", "t", "--nope"}, {"s", "t", "--data"}, {"s", "t", "novalue"}, {"s", "t", "--timeout", "0"}} {
		if _, err := tool.parseCall(bad); err == nil {
			t.Errorf("parseCall(%v) accepted", bad)
		}
	}
}

func TestExamplesFromSessionLogs(t *testing.T) {
	e := newEnv(t)
	all := rows(t, e.ok("examples"))
	if got := ids(all); !reflect.DeepEqual(got, []string{"mcp-example/codex:call_2", "mcp-example/codex:call_1", "mcp-example/claude:toolu_3", "mcp-example/claude:toolu_1"}) {
		t.Fatalf("examples = %v", got)
	}
	ex := rows(t, e.ok("mcp-tool", "examples", "fake", "echo"))
	if len(ex) != 2 {
		t.Fatalf("echo examples = %v", ids(ex))
	}
	b, _ := json.Marshal(ex)
	if strings.Contains(string(b), "ghp_") || !strings.Contains(string(b), "from claude") {
		t.Fatalf("redaction or content wrong: %s", b)
	}
	if got := rows(t, e.ok("mcp-example", "list", "--harness", "claude", "--tool", "fail")); len(got) != 1 || !strings.Contains(got[0].Label, "error") {
		t.Fatalf("claude fail = %+v", got)
	}
	// Cached: a second harvest reads no file.
	var rep HarvestReport
	json.Unmarshal([]byte(e.ok("harvest")), &rep)
	if rep.Files != 2 || rep.Read != 0 || rep.Examples != 4 {
		t.Fatalf("harvest = %+v", rep)
	}
	json.Unmarshal([]byte(e.ok("harvest", "--rescan")), &rep)
	if rep.Read != 2 {
		t.Fatalf("rescan = %+v", rep)
	}
	// The example's call, made again through o-mcp, is recorded.
	out := e.ok("mcp-example", "again", "--target", "mcp-example/codex:call_1")
	if !strings.Contains(out, "from codex") {
		t.Fatalf("again = %s", out)
	}
	if _, errOut, code := e.run("", "mcp-example", "again", "claude:toolu_1"); code == 0 || !strings.Contains(errOut, "redacted") {
		t.Fatalf("redacted args replayed: %d %s", code, errOut)
	}
}

func TestStateCarriesEveryNoun(t *testing.T) {
	e := newEnv(t)
	e.ok("discover", "fake")
	out := e.ok("state")
	seen := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var r struct {
			Noun  string   `json:"noun"`
			Cells []string `json:"cells"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		if len(r.Cells) != len(tables[r.Noun].columns) {
			t.Fatalf("%s row has %d cells for %d columns", r.Noun, len(r.Cells), len(tables[r.Noun].columns))
		}
		seen[r.Noun]++
	}
	if seen[serverNoun] != 4 || seen[toolNoun] != 3 || seen[exampleNoun] != 4 {
		t.Fatalf("state nouns = %v", seen)
	}
}
