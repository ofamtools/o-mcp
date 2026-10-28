// Package fake is a small MCP server for o-mcp's tests: three tools on
// pages of two (echo, fail, prose), served in memory, over HTTP or over
// stdio by the test binary itself (Main).
package fake

import (
	"context"
	"encoding/json"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// EnvStdio, set to 1, makes a test binary that calls Main serve the fake
// over stdio instead of running its tests.
const EnvStdio = "O_MCP_FAKE_STDIO"

// Server is a fresh fake server.
func Server() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1.0.0"}, &mcp.ServerOptions{PageSize: 2})
	object := map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}
	s.AddTool(&mcp.Tool{Name: "echo", Description: "echo the arguments and the FAKE_ENV variable", InputSchema: object}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "echoed"}},
			StructuredContent: map[string]any{"args": args, "env": os.Getenv("FAKE_ENV")},
		}, nil
	})
	s.AddTool(&mcp.Tool{Name: "fail", Description: "always a tool error", InputSchema: object}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "it failed"}}}, nil
	})
	s.AddTool(&mcp.Tool{Name: "json", Description: "one JSON text block", InputSchema: object}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"n":1}`}}}, nil
	})
	return s
}

// Main serves the fake over stdio and exits when EnvStdio is 1; else it
// returns. A test package's TestMain calls it first.
func Main() {
	if os.Getenv(EnvStdio) != "1" {
		return
	}
	if err := Server().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// HTTP is a Streamable HTTP handler for the fake that answers 401 unless
// the request carries Authorization: want (any request when want is "").
func HTTP(want string) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return Server() }, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want != "" && r.Header.Get("Authorization") != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
