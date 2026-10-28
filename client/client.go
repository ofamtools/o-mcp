// Package client is o-mcp's in-process API: the MCP servers the AI
// harnesses configure (read through o-ai's harness package), a connection
// to one of them over stdio, Streamable HTTP or legacy SSE, its tools
// (every page of tools/list) and a tool call. Store keeps discovered
// schemas and the call history so they are usable offline.
//
// Credentials come only from the server's own configuration: its headers,
// a bearer-token environment variable, a stdio server's env. They are
// resolved at connect time and never stored: a Server carries the names
// of variables and headers, not their values. A remote server that
// answers 401 or 403 (an OAuth-only connector) is Unreachable, not a
// failure of the inventory.
//
//	servers, _ := client.Configured(client.Options{})
//	srv, _ := client.Find(servers, "slack")
//	s, _ := client.Connect(ctx, srv, client.Options{})
//	defer s.Close()
//	tools, _ := s.Tools(ctx)
//	res, _ := s.Call(ctx, "search_messages", map[string]any{"query": "deploy"})
package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/unixsuperhero/o-ai/harness"
)

// Options are where servers are read from and how a connection runs. The
// zero value reads the real home, the current directory as the project and
// the process environment.
type Options struct {
	Home    string              // default os.UserHomeDir
	Project string              // default the working directory
	Getenv  func(string) string // credential and ${VAR} lookups; default os.Getenv
	Environ []string            // a stdio server's base environment; default os.Environ
	Timeout time.Duration       // connect and per-request timeout; default 30s
	Stderr  io.Writer           // a stdio server's stderr; default discarded
}

func (o Options) withDefaults() Options {
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	if o.Project == "" {
		o.Project, _ = os.Getwd()
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Environ == nil {
		o.Environ = os.Environ()
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	return o
}

// Server is one configured server: o-ai's public record (ID is
// HARNESS:SCOPE:NAME) plus the credential references o-mcp follows. It
// holds no secret value; the raw entry it connects with stays unexported
// and is never marshalled.
type Server struct {
	harness.MCPServer
	// BearerTokenEnv names the variable whose value is sent as
	// Authorization: Bearer (Codex's bearer_token_env_var).
	BearerTokenEnv string `json:"bearerTokenEnv,omitempty"`
	// HeaderEnv names the header variables (Codex's env_http_headers:
	// header name, then =, then the variable's name).
	HeaderEnv []string `json:"headerEnv,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`

	raw map[string]any
}

// Configured is every server the harnesses configure for o.Project,
// disabled ones included, sorted by ID, and the warnings of files that
// could not be read. It reads files only; it never connects.
func Configured(o Options) ([]Server, []string) {
	o = o.withDefaults()
	entries, warnings := harness.Configured(o.Home, o.Project)
	out := make([]Server, 0, len(entries))
	for _, e := range entries {
		out = append(out, FromEntry(e))
	}
	return out, warnings
}

// FromEntry is the Server of a harness entry, with its raw configuration
// kept for Connect.
func FromEntry(e harness.MCPEntry) Server {
	s := Server{MCPServer: e.Server, raw: e.Raw}
	s.BearerTokenEnv = text(e.Raw, "bearer_token_env_var")
	for _, k := range sortedKeys(object(e.Raw["env_http_headers"])) {
		s.HeaderEnv = append(s.HeaderEnv, k+"="+text(object(e.Raw["env_http_headers"]), k))
	}
	s.Cwd = text(e.Raw, "cwd")
	return s
}

// NewServer is a server from a raw configuration object in any harness's
// spelling, under the given name (an explicit --url, a test's fake). Its
// ID is "explicit:NAME".
func NewServer(name string, raw map[string]any) Server {
	s := FromEntry(harness.MCPEntry{Server: harness.ParseMCPServer(raw), Raw: raw})
	s.ID, s.Name, s.Harness, s.Scope = "explicit:"+name, name, "explicit", "explicit"
	return s
}

// Fingerprint identifies the server's public definition: what it runs or
// where it connects and which variables and headers it names. A saved
// snapshot whose fingerprint differs from the configured server's is
// stale. It hashes no secret value.
func (s Server) Fingerprint() string {
	b, _ := json.Marshal([]any{s.Config, s.Name, s.Transport, s.Command, s.Args, s.URL, s.Env, s.Headers, s.BearerTokenEnv, s.HeaderEnv, s.Cwd})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// definition is the fingerprint without the source: two harnesses that
// configure the same server agree on it.
func (s Server) definition() string {
	b, _ := json.Marshal([]any{s.Transport, s.Command, s.Args, s.URL, s.Env, s.Headers, s.BearerTokenEnv, s.HeaderEnv, s.Cwd})
	return string(b)
}

// Address is where the server is, safe to print: its URL, else its
// command line.
func (s Server) Address() string {
	if s.URL != "" {
		return s.URL
	}
	return strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
}

// Errors.
var (
	// ErrNotFound: no configured server has the name or ID.
	ErrNotFound = errors.New("no configured MCP server")
	// ErrAmbiguous: the name is configured differently in several places.
	ErrAmbiguous = errors.New("ambiguous MCP server name")
	// ErrDisabled: the server's configuration disables it.
	ErrDisabled = errors.New("MCP server is disabled in its config")
)

// UnreachableError is a server o-mcp cannot reach with the credentials its
// configuration gives: a remote server that answered 401 or 403, which is
// how an OAuth-only connector answers.
type UnreachableError struct {
	Server string
	Reason string
}

func (e *UnreachableError) Error() string { return e.Server + " is unreachable: " + e.Reason }

// IsUnreachable reports whether err is an UnreachableError.
func IsUnreachable(err error) bool {
	var u *UnreachableError
	return errors.As(err, &u)
}

// Find is the server ref names: an ID (HARNESS:SCOPE:NAME) exactly, else
// a name. A name several harnesses configure the same way is the first
// enabled of them; configured differently it is ErrAmbiguous, and the
// error lists the IDs to choose from.
func Find(servers []Server, ref string) (Server, error) {
	var named []Server
	for _, s := range servers {
		if s.ID == ref {
			return s, nil
		}
		if s.Name == ref {
			named = append(named, s)
		}
	}
	if len(named) == 0 {
		return Server{}, fmt.Errorf("%w named %q", ErrNotFound, ref)
	}
	sort.SliceStable(named, func(i, j int) bool { return !named[i].Disabled && named[j].Disabled })
	for _, s := range named[1:] {
		if s.definition() != named[0].definition() {
			ids := make([]string, len(named))
			for i, n := range named {
				ids[i] = n.ID
			}
			return Server{}, fmt.Errorf("%w %q: configured differently as %s; name one by ID", ErrAmbiguous, ref, strings.Join(ids, ", "))
		}
	}
	return named[0], nil
}

func text(m map[string]any, key string) string { s, _ := m[key].(string); return s }
func object(v any) map[string]any              { m, _ := v.(map[string]any); return m }

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
