package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/unixsuperhero/o-mcp/client"
	ocli "github.com/unixsuperhero/ochazuke/cli"
	"github.com/unixsuperhero/oden"
)

// commands are the top-level operations. call is its own command (stdin,
// the exit status of a tool error); the rest are the runtime's verbs under
// h-mcp's names.
func (t *Tool) commands(sh *ocli.Shell) []oden.Command {
	forward := func(name, summary string, words ...string) oden.Command {
		return oden.Command{Name: name, Summary: summary, Run: func(ctx context.Context, rest []string) error {
			return sh.Run(ctx, append(append([]string{}, words...), rest...))
		}}
	}
	return []oden.Command{
		{Name: "call", Summary: "call SERVER TOOL [KEY=VALUE...] [--data JSON | --args-file FILE|-] [--extract] [--timeout SECONDS]: print the MCP result as JSON; a tool error prints it and exits 1", Run: t.callCommandRun},
		forward("configured", "the servers every harness configures, disabled ones too (mcp-server list; --harness H, --state enabled|disabled|saved|stale|unsaved)", serverNoun, "list"),
		forward("discover", "discover SERVER | --all: connect, list every tool, save the schemas (mcp-server discover)", serverNoun, "discover"),
		forward("refresh", "refresh [SERVER]: discover the saved servers again (mcp-server refresh)", serverNoun, "refresh"),
		forward("test", "test SERVER: initialize only (mcp-server test)", serverNoun, "test"),
		forward("tools", "tools [SERVER]: the saved tools, offline (mcp-tool list)", toolNoun, "list"),
		forward("tool", "tool SERVER TOOL: one saved tool, its schema and call command (mcp-tool show)", toolNoun, "show"),
		forward("examples", "examples [SERVER [TOOL]]: real calls from Claude and Codex session logs (mcp-example list)", exampleNoun, "list"),
		forward("harvest", "harvest [--rescan]: read the session logs into the examples cache (mcp-example harvest)", exampleNoun, "harvest"),
		forward("calls", "calls [SERVER [TOOL]]: o-mcp's own call history (mcp-call list)", callNoun, "list"),
		forward("state", "every list's rows as JSON Lines (status state)", "status", "state"),
	}
}

// callArgs are call's words: SERVER TOOL, KEY=VALUE overrides and the
// options.
type callArgs struct {
	server, tool string
	args         map[string]any
	extract      bool
	timeout      time.Duration
}

// parseCall reads call's words; stdin is read only for --args-file -.
func (t *Tool) parseCall(words []string) (callArgs, error) {
	var c callArgs
	var pos, sets []string
	var data, file string
	var haveData, haveFile bool
	for i := 0; i < len(words); i++ {
		w := words[i]
		name, value, inline := strings.Cut(strings.TrimPrefix(w, "--"), "=")
		if !strings.HasPrefix(w, "--") {
			if len(pos) < 2 {
				pos = append(pos, w)
			} else {
				sets = append(sets, w)
			}
			continue
		}
		take := func() (string, error) {
			if inline {
				return value, nil
			}
			if i+1 >= len(words) {
				return "", fmt.Errorf("--%s needs a value", name)
			}
			i++
			return words[i], nil
		}
		var err error
		switch name {
		case "data", "args":
			data, err = take()
			haveData = true
		case "args-file":
			file, err = take()
			haveFile = true
		case "extract":
			c.extract = true
		case "timeout":
			var v string
			if v, err = take(); err == nil {
				var secs float64
				if secs, err = strconv.ParseFloat(v, 64); err == nil && secs <= 0 {
					err = errors.New("--timeout must be positive seconds")
				}
				c.timeout = time.Duration(secs * float64(time.Second))
			}
		default:
			return c, fmt.Errorf("call: unknown option %q (--data JSON, --args-file FILE|-, --extract, --timeout SECONDS)", w)
		}
		if err != nil {
			return c, err
		}
	}
	if len(pos) < 2 {
		return c, errors.New("usage: " + t.Prog + " call SERVER TOOL [KEY=VALUE...] [--data JSON | --args-file FILE|-] [--extract] [--timeout SECONDS]")
	}
	if haveData && haveFile {
		return c, errors.New("call: --data and --args-file both give the arguments; pass one")
	}
	c.server, c.tool = pos[0], pos[1]
	var err error
	switch {
	case haveData:
		c.args, err = client.ParseArgs([]byte(data))
	case haveFile && file == "-":
		c.args, err = client.ReadArgs(t.Stdin)
	case haveFile:
		var f *os.File
		if f, err = os.Open(file); err == nil {
			c.args, err = client.ReadArgs(f)
			f.Close()
		}
	default:
		c.args = map[string]any{}
	}
	if err != nil {
		return c, err
	}
	for _, kv := range sets {
		if err := client.SetArg(c.args, kv); err != nil {
			return c, err
		}
	}
	return c, nil
}

func (t *Tool) callCommandRun(ctx context.Context, words []string) error {
	c, err := t.parseCall(words)
	if err != nil {
		return err
	}
	if c.timeout > 0 {
		t.Timeout = c.timeout
	}
	res, _, err := t.call(ctx, c.server, c.tool, c.args)
	if err != nil {
		return err
	}
	var out any = res
	if c.extract && !res.IsError {
		if out, err = client.Data(res); err != nil {
			return fmt.Errorf("--extract: %w (omit --extract for the full result)", err)
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(t.Stdout, string(b))
	if res.IsError {
		fmt.Fprintln(t.Stderr, t.Prog+": the tool answered an error (isError)")
		return &oden.ExitError{Code: 1}
	}
	return nil
}

func compactJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
