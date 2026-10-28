package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// Version is the client version sent in initialize.
const Version = "0.1.0"

// Session is one open connection to a server.
type Session struct {
	Server  Server
	cs      *mcp.ClientSession
	timeout time.Duration
}

// Connect opens a session with the server: it resolves the server's
// configuration (credentials from the current environment), starts or
// reaches it and initializes. A disabled server is ErrDisabled. A remote
// server that signs in (Server.OAuth) gets its stored token, refreshed
// before it expires and once on a 401; one that needs a sign-in is a
// NeedsLoginError, unless o.Interactive, when Connect runs Login and
// tries again. Any other 401 or 403 is an UnreachableError.
func Connect(ctx context.Context, s Server, o Options) (*Session, error) {
	sess, err := connect(ctx, s, o)
	if err != nil && o.Interactive && IsNeedsLogin(err) {
		if _, lerr := Login(ctx, s, o); lerr != nil {
			return nil, lerr
		}
		o.Interactive = false
		return connect(ctx, s, o)
	}
	return sess, err
}

func connect(ctx context.Context, s Server, o Options) (*Session, error) {
	if s.Disabled {
		return nil, fmt.Errorf("%s: %w", s.ID, ErrDisabled)
	}
	o = o.withDefaults()
	var h *oauthHandler
	if s.OAuth() {
		h = newOAuthHandler(s, o)
	}
	t, status, err := transport(s, o, h)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.ID, err)
	}
	sess, err := ConnectTransport(ctx, s, t, o)
	if err != nil && status != nil {
		code := status.get()
		if h != nil {
			if n := h.result(code); n != nil {
				markNeedsLogin(o, s, n)
				return nil, n
			}
		}
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			reason := fmt.Sprintf("the server answered %d", code)
			if h != nil {
				reason += " and its config gives no Authorization header or bearer token"
			} else {
				reason += " to the credential its config gives"
			}
			return nil, &UnreachableError{Server: s.ID, Reason: reason}
		}
	}
	return sess, err
}

// ConnectTransport opens a session over a transport the caller made (an
// in-memory transport in a test, a server in the same process).
func ConnectTransport(ctx context.Context, s Server, t mcp.Transport, o Options) (*Session, error) {
	o = o.withDefaults()
	c := mcp.NewClient(&mcp.Implementation{Name: "o-mcp", Version: Version}, nil)
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	cs, err := c.Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: connect: %w", s.ID, err)
	}
	return &Session{Server: s, cs: cs, timeout: o.Timeout}, nil
}

// Initialize is the server's initialize answer.
func (s *Session) Initialize() *mcp.InitializeResult { return s.cs.InitializeResult() }

// Close ends the session (and a stdio server's process).
func (s *Session) Close() error { return s.cs.Close() }

// Tools is every tool the server lists, page by page of tools/list.
func (s *Session) Tools(ctx context.Context) ([]*mcp.Tool, error) {
	var out []*mcp.Tool
	params := &mcp.ListToolsParams{}
	for page := 0; ; page++ {
		if page > 1000 {
			return nil, errors.New("tools/list: more than 1000 pages")
		}
		rctx, cancel := context.WithTimeout(ctx, s.timeout)
		res, err := s.cs.ListTools(rctx, params)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%s: tools/list: %w", s.Server.ID, err)
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" {
			return out, nil
		}
		params = &mcp.ListToolsParams{Cursor: res.NextCursor}
	}
}

// Call calls one tool with arguments (nil is {}). A tool error is a result
// with IsError set, not an error; an error is the transport's or the
// protocol's.
func (s *Session) Call(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("%s: tools/call %s: %w", s.Server.ID, tool, err)
	}
	return res, nil
}

// statusRecorder remembers the last HTTP status a remote server answered.
type statusRecorder struct {
	mu   sync.Mutex
	code int
}

func (r *statusRecorder) set(code int) { r.mu.Lock(); r.code = code; r.mu.Unlock() }
func (r *statusRecorder) get() int     { r.mu.Lock(); defer r.mu.Unlock(); return r.code }

// headerTransport adds the configured headers to every request and records
// the answer's status. For an SSE server that signs in (the go-sdk's SSE
// transport takes no OAuth handler) it also sends the stored token.
type headerTransport struct {
	headers map[string]string
	status  *statusRecorder
	base    http.RoundTripper
	token   oauth2.TokenSource
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if t.token != nil {
		tok, err := t.token.Token()
		if err != nil {
			return nil, err
		}
		if tok != nil {
			req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}
	}
	res, err := t.base.RoundTrip(req)
	if err == nil {
		t.status.set(res.StatusCode)
	}
	return res, err
}

// transport is the server's transport, its configuration resolved now.
func transport(s Server, o Options, h *oauthHandler) (mcp.Transport, *statusRecorder, error) {
	raw := s.raw
	if raw == nil {
		return nil, nil, errors.New("no configuration to connect with (a saved record, not a configured server)")
	}
	expand := expander(o, s)
	switch s.Transport {
	case "stdio":
		var command string
		args := stringList(raw["args"])
		if c, ok := raw["command"].(string); ok {
			command = c
		} else if parts := stringList(raw["command"]); len(parts) > 0 {
			command, args = parts[0], append(parts[1:], args...)
		}
		if command == "" {
			return nil, nil, errors.New("stdio server has no command")
		}
		cmd := exec.Command(expand(command))
		for _, a := range args {
			cmd.Args = append(cmd.Args, expand(a))
		}
		env := append([]string{}, o.Environ...)
		for _, key := range []string{"env", "environment"} {
			for _, k := range sortedKeys(object(raw[key])) {
				switch v := object(raw[key])[k].(type) {
				case nil:
					env = unset(env, k)
				case string:
					env = append(unset(env, k), k+"="+expand(v))
				case float64, int64, int, bool:
					env = append(unset(env, k), k+"="+fmt.Sprint(v))
				}
			}
		}
		cmd.Env = env
		if cwd := expand(text(raw, "cwd")); cwd != "" {
			if !filepath.IsAbs(cwd) {
				cwd = filepath.Join(filepath.Dir(s.Config), cwd)
			}
			cmd.Dir = cwd
		}
		cmd.Stderr = o.Stderr
		return &mcp.CommandTransport{Command: cmd}, nil, nil
	case "http", "sse":
		endpoint, headers, err := remote(s, o)
		if err != nil {
			return nil, nil, err
		}
		status := &statusRecorder{}
		ht := &headerTransport{headers: headers, status: status, base: http.DefaultTransport}
		hc := &http.Client{Transport: ht}
		if s.Transport == "sse" {
			if h != nil {
				ht.token = h
			}
			return &mcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: hc}, status, nil
		}
		st := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true}
		if h != nil {
			st.OAuthHandler = h
		}
		return st, status, nil
	}
	return nil, nil, fmt.Errorf("unsupported transport %q", s.Transport)
}

// remote is a remote server's endpoint and configured headers, resolved
// now.
func remote(s Server, o Options) (string, map[string]string, error) {
	raw := s.raw
	if raw == nil {
		return "", nil, errors.New("no configuration to connect with (a saved record, not a configured server)")
	}
	expand := expander(o, s)
	endpoint := ""
	for _, k := range []string{"url", "httpUrl", "serverUrl"} {
		if endpoint = expand(text(raw, k)); endpoint != "" {
			break
		}
	}
	if endpoint == "" {
		return "", nil, errors.New("remote server has no url")
	}
	headers := map[string]string{}
	for _, key := range []string{"headers", "http_headers"} {
		for k, v := range object(raw[key]) {
			if sv, ok := v.(string); ok {
				headers[k] = expand(sv)
			}
		}
	}
	for k, v := range object(raw["env_http_headers"]) {
		if name, ok := v.(string); ok && o.Getenv(name) != "" {
			headers[k] = o.Getenv(name)
		}
	}
	if name := s.BearerTokenEnv; name != "" {
		if tok := o.Getenv(name); tok != "" {
			headers["Authorization"] = "Bearer " + tok
		} else {
			return "", nil, fmt.Errorf("bearer token variable %s is not set", name)
		}
	}
	return endpoint, headers, nil
}

// reference is a variable reference in a configured string: ${VAR},
// ${env:VAR}, {env:VAR} (OpenCode), ${VAR:-default}.
var reference = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}|\{env:([A-Za-z_][A-Za-z0-9_]*)\}|\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expander resolves variable references against o's environment, and
// ${workspaceFolder} to the project.
func expander(o Options, s Server) func(string) string {
	return func(v string) string {
		return reference.ReplaceAllStringFunc(v, func(m string) string {
			g := reference.FindStringSubmatch(m)
			name := g[1] + g[2] + g[3]
			if name == "workspaceFolder" {
				return o.Project
			}
			if val := o.Getenv(name); val != "" {
				return val
			}
			return g[5]
		})
	}
}

func unset(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func stringList(v any) []string {
	a, _ := v.([]any)
	var out []string
	for _, x := range a {
		switch s := x.(type) {
		case string:
			out = append(out, s)
		case float64:
			out = append(out, strconv.FormatFloat(s, 'f', -1, 64))
		}
	}
	return out
}
