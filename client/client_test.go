package client

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"ofam.dev/o-mcp/internal/fake"
)

func TestMain(m *testing.M) {
	fake.Main()
	os.Exit(m.Run())
}

func opts(t *testing.T) Options {
	return Options{Home: t.TempDir(), Project: t.TempDir(), Getenv: func(string) string { return "" }, Timeout: 10 * time.Second,
		Keyring: NewMemoryKeyring(), OpenURL: noBrowser(t), Prompt: io.Discard}
}

func names(tools []*mcp.Tool) []string {
	var out []string
	for _, tl := range tools {
		out = append(out, tl.Name)
	}
	return out
}

func TestInMemorySessionPagesToolsAndCalls(t *testing.T) {
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	go fake.Server().Run(ctx, st)
	sess, err := ConnectTransport(ctx, NewServer("fake", map[string]any{"command": "unused"}), ct, opts(t))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if sess.Initialize().ServerInfo.Name != "fake" {
		t.Fatalf("initialize = %+v", sess.Initialize())
	}
	tools, err := sess.Tools(ctx) // three tools on pages of two
	if err != nil {
		t.Fatal(err)
	}
	if got := names(tools); !reflect.DeepEqual(got, []string{"echo", "fail", "json"}) {
		t.Fatalf("tools = %v", got)
	}
	res, err := sess.Call(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := Data(res)
	if err != nil || !reflect.DeepEqual(data.(map[string]any)["args"], map[string]any{"text": "hi"}) {
		t.Fatalf("data = %v, %v", data, err)
	}
	res, err = sess.Call(ctx, "fail", nil)
	if err != nil || !res.IsError {
		t.Fatalf("fail = %+v, %v", res, err)
	}
	res, _ = sess.Call(ctx, "json", nil)
	if data, err := Data(res); err != nil || data.(map[string]any)["n"] != 1.0 {
		t.Fatalf("json text block = %v, %v", data, err)
	}
	if _, err := Data(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "prose"}}}); !errors.Is(err, ErrNoData) {
		t.Fatalf("prose = %v", err)
	}
}

// The test binary is the stdio server: its env carries FAKE_ENV from the
// server's config, resolved from a ${VAR} reference.
func TestStdioServerFromConfig(t *testing.T) {
	ctx := context.Background()
	o := opts(t)
	o.Getenv = func(k string) string {
		if k == "SECRET_SOURCE" {
			return "resolved"
		}
		return ""
	}
	s := NewServer("stdio", map[string]any{
		"command": os.Args[0],
		"env":     map[string]any{fake.EnvStdio: "1", "FAKE_ENV": "${SECRET_SOURCE}"},
	})
	snap, err := Discover(ctx, s, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(snap.Tools); len(got) != 3 || snap.Fingerprint != s.Fingerprint() {
		t.Fatalf("snapshot = %v %s", got, snap.Fingerprint)
	}
	sess, err := Connect(ctx, s, o)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	res, err := sess.Call(ctx, "echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if env := res.StructuredContent.(map[string]any)["env"]; env != "resolved" {
		t.Fatalf("FAKE_ENV = %v", env)
	}
}

func TestHTTPBearerTokenAndUnreachable(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(fake.HTTP("Bearer tok-123"))
	defer srv.Close()
	o := opts(t)
	o.Getenv = func(k string) string {
		if k == "FAKE_TOKEN" {
			return "tok-123"
		}
		return ""
	}
	withToken := NewServer("remote", map[string]any{"url": srv.URL, "bearer_token_env_var": "FAKE_TOKEN"})
	snap, err := Discover(ctx, withToken, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tools) != 3 {
		t.Fatalf("tools = %v", names(snap.Tools))
	}
	headers := NewServer("headers", map[string]any{"url": srv.URL, "headers": map[string]any{"Authorization": "Bearer ${FAKE_TOKEN}"}})
	if _, err := Discover(ctx, headers, o); err != nil {
		t.Fatalf("headers with a reference: %v", err)
	}
	// No credential in the config: a 401 asks for sign-in, not unreachable.
	oauth := NewServer("oauth", map[string]any{"url": srv.URL})
	_, err = Discover(ctx, oauth, o)
	if !IsNeedsLogin(err) || IsUnreachable(err) || !strings.Contains(err.Error(), "o-mcp login oauth") {
		t.Fatalf("no credential = %v, want needs sign-in", err)
	}
	// A configured credential the server rejects is unreachable.
	wrong := NewServer("wrong", map[string]any{"url": srv.URL, "headers": map[string]any{"Authorization": "Bearer nope"}})
	if _, err = Discover(ctx, wrong, o); !IsUnreachable(err) || IsNeedsLogin(err) {
		t.Fatalf("rejected credential = %v, want unreachable", err)
	}
}

func TestDisabledServerIsRefused(t *testing.T) {
	s := NewServer("off", map[string]any{"command": "x", "enabled": false})
	if _, err := Connect(context.Background(), s, opts(t)); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v", err)
	}
}

func TestFindByIDNameAndAmbiguity(t *testing.T) {
	a := NewServer("tuios", map[string]any{"command": "tuios"})
	a.ID = "claude:user:tuios"
	b := a
	b.ID = "codex:user:tuios"
	c := NewServer("x", map[string]any{"command": "other"})
	c.ID, c.Name = "omp:user:tuios", "tuios"
	if s, err := Find([]Server{a, b}, "tuios"); err != nil || s.ID != a.ID {
		t.Fatalf("same definition: %v %v", s.ID, err)
	}
	if s, err := Find([]Server{a, b, c}, "codex:user:tuios"); err != nil || s.ID != b.ID {
		t.Fatalf("by id: %v %v", s.ID, err)
	}
	if _, err := Find([]Server{a, c}, "tuios"); !errors.Is(err, ErrAmbiguous) || !strings.Contains(err.Error(), "omp:user:tuios") {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := Find(nil, "none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestParseArgsAndSetArg(t *testing.T) {
	for _, bad := range []string{"[1]", `"s"`, "{", `{} {}`} {
		if _, err := ParseArgs([]byte(bad)); err == nil {
			t.Errorf("ParseArgs(%q) accepted", bad)
		}
	}
	args, err := ParseArgs([]byte(" "))
	if err != nil || len(args) != 0 {
		t.Fatalf("empty = %v %v", args, err)
	}
	args, _ = ReadArgs(strings.NewReader(`{"q":"a","n":2}`))
	for _, kv := range []string{"n=3", "flag=true", "name=plain text", "nested.k=[1]", "q=\"quoted\""} {
		if err := SetArg(args, kv); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := json_(args)
	if want := `{"flag":true,"n":3,"name":"plain text","nested":{"k":[1]},"q":"quoted"}`; got != want {
		t.Fatalf("args = %s, want %s", got, want)
	}
	if SetArg(args, "novalue") == nil {
		t.Fatal("KEY without = accepted")
	}
}
