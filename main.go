// Command o-mcp is the family's MCP tool, a Go recreation of h-mcp: the
// MCP servers every AI harness configures (read through o-ai's harness
// package, disabled entries included), their tools discovered and saved
// so they are usable offline, direct tool calls, and real calls found in
// Claude and Codex session logs. It is an oden app with an ochazuke
// runtime behind it, so `o-mcp catalog` answers a host (mtm mounts it as
// the MCP page). Package client is the same work in process.
//
//	o-mcp configured [--harness H] [--state S]      the configured servers
//	o-mcp discover SERVER | --all                   connect, list tools, save
//	o-mcp refresh [SERVER]                          discover the saved servers again
//	o-mcp test SERVER                               initialize only
//	o-mcp login SERVER [--force]                    OAuth sign-in in the browser
//	o-mcp logout SERVER                             revoke and forget the sign-in
//	o-mcp auth status [SERVER]                      signed in or not, expiry, scopes
//	o-mcp tools [SERVER]                            the saved tools
//	o-mcp tool SERVER TOOL                          one saved tool and its schema
//	o-mcp call SERVER TOOL [K=V...] [--data JSON | --args-file FILE|-] [--extract]
//	o-mcp examples SERVER [TOOL]                    real calls from session logs
//	o-mcp harvest [--rescan]                        read the session logs
//	o-mcp calls [SERVER [TOOL]]                     o-mcp's own call history
//	o-mcp NOUN VERB ...                             mcp-server, mcp-tool, mcp-call, mcp-example
//	o-mcp catalog                                   the nouns and verbs, for a host
package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/unixsuperhero/o-mcp/client"
	ocli "github.com/unixsuperhero/ochazuke/cli"
	"github.com/unixsuperhero/oden"
)

// Version is the catalog's version.
const Version = "0.1.0"

// Tool is one invocation's world: streams, environment, home and working
// directory (the project whose servers are read). Every path o-mcp reads
// or writes derives from them, so a test points it at a temporary tree.
type Tool struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Env            []string
	Home, Cwd      string
	Prog           string // "o mcp" under o, else "o-mcp"; messages only
	Now            func() time.Time
	Timeout        time.Duration // per request; 0 is client's default
	// Keyring holds sign-in tokens (nil: the macOS Keychain); OpenURL
	// opens the browser for login (nil: open). Tests set both.
	Keyring client.Keyring
	OpenURL func(string) error
}

func main() {
	home, err := os.UserHomeDir()
	if err == nil {
		home, err = filepath.Abs(home)
	}
	cwd, cwdErr := os.Getwd()
	if err == nil {
		err = cwdErr
	}
	prog := "o-mcp"
	if parent := os.Getenv(oden.EnvParent); parent != "" {
		prog = parent + " mcp"
	}
	if err != nil {
		os.Exit(oden.Report(os.Stderr, prog, err))
	}
	t := &Tool{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Env: os.Environ(), Home: home, Cwd: cwd, Prog: prog, Now: time.Now}
	os.Exit(oden.Report(os.Stderr, t.Prog, t.Main(context.Background(), os.Args[1:])))
}

// Main runs one command: an operation (call, configured, ...), a noun of
// the runtime, catalog, or an oden builtin (help, commands, which).
func (t *Tool) Main(ctx context.Context, args []string) error {
	app := oden.New("o-mcp", args, oden.WithBuiltins(), oden.WithStdio(t.Stdin, t.Stdout, t.Stderr), oden.WithEnv(t.Env))
	sh := t.shell()
	for _, c := range t.commands(sh) {
		app.Add(c)
	}
	ocli.Mount(app, sh)
	return app.Run(ctx)
}

// getenv is a variable of t.Env, "" when unset.
func (t *Tool) getenv(key string) string {
	for i := len(t.Env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(t.Env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

func (t *Tool) now() time.Time {
	if t.Now == nil {
		return time.Now()
	}
	return t.Now()
}

// options are the client's: this tool's home, project and environment.
func (t *Tool) options() client.Options {
	return client.Options{Home: t.Home, Project: t.Cwd, Getenv: t.getenv, Environ: t.Env, Timeout: t.Timeout, Keyring: t.Keyring, OpenURL: t.OpenURL, Prompt: t.Stderr}
}

// store is o-mcp's data directory (client.DefaultStore).
func (t *Tool) store() client.Store { return client.DefaultStore(t.Home, t.getenv) }

// CacheDir is $XDG_CACHE_HOME/o-mcp, else ~/.cache/o-mcp.
func (t *Tool) CacheDir() string {
	base := t.getenv("XDG_CACHE_HOME")
	if base == "" {
		base = filepath.Join(t.Home, ".cache")
	}
	return filepath.Join(base, "o-mcp")
}

// tilde is path with the home directory spelled ~.
func (t *Tool) tilde(path string) string {
	if rest, ok := strings.CutPrefix(path, t.Home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return path
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
