package client

import "ofam.dev/o-ai/harness"

// RedactValues is v with every string's secret-looking parts (tokens,
// bearer credentials, JWTs: o-ai's harness.SecretValue) replaced by
// [REDACTED]. Keys are kept: a tool's arguments named "command" or "args"
// are its data, not configuration.
func RedactValues(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			out[k] = RedactValues(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = RedactValues(value)
		}
		return out
	case string:
		return harness.SecretValue.ReplaceAllString(x, "[REDACTED]")
	}
	return v
}
