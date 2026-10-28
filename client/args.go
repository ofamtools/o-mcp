package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ParseArgs is a tool call's arguments: a JSON object (empty input is {}).
func ParseArgs(b []byte) (map[string]any, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("arguments are not JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("arguments: more than one JSON value")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("arguments must be a JSON object")
	}
	return m, nil
}

// ReadArgs reads ParseArgs's input from r, at most 16 MiB.
func ReadArgs(r io.Reader) (map[string]any, error) {
	b, err := io.ReadAll(io.LimitReader(r, 16<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 16<<20 {
		return nil, errors.New("arguments exceed 16 MiB")
	}
	return ParseArgs(b)
}

// SetArg applies one KEY=VALUE override to args: a value that parses as
// JSON is that value (a number, boolean, object, array, null), anything
// else the string. KEY may be a dotted path into nested objects.
func SetArg(args map[string]any, kv string) error {
	key, value, ok := strings.Cut(kv, "=")
	if !ok || key == "" {
		return fmt.Errorf("%q is not KEY=VALUE", kv)
	}
	var v any = value
	dec := json.NewDecoder(strings.NewReader(value))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err == nil && !dec.More() {
		v = parsed
	}
	parts := strings.Split(key, ".")
	m := args
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
	return nil
}

// ErrNoData: a result has no structured content and is not exactly one
// JSON text block.
var ErrNoData = errors.New("result has no structured content and is not one JSON text block")

// Data is a result's data: its structuredContent when present, else its
// one text block parsed as JSON. Prose, several blocks and other content
// are ErrNoData.
func Data(res *mcp.CallToolResult) (any, error) {
	if res.StructuredContent != nil {
		return res.StructuredContent, nil
	}
	if len(res.Content) != 1 {
		return nil, ErrNoData
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return nil, ErrNoData
	}
	var v any
	if err := json.Unmarshal([]byte(tc.Text), &v); err != nil {
		return nil, ErrNoData
	}
	return v, nil
}
