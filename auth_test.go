package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unixsuperhero/o-mcp/client"
	"github.com/unixsuperhero/o-mcp/internal/fake"
)

// TestSignInCommands walks an OAuth server through the CLI: needs sign-in
// (test, discover, call, configured; no browser), login, auth status,
// discover and call with the stored token, logout. No token appears in
// any output or file.
func TestSignInCommands(t *testing.T) {
	f := fake.NewOAuth()
	defer f.Close()
	e := newEnv(t)
	e.extra = map[string]any{"gw": map[string]any{"type": "http", "url": f.Endpoint()}}
	e.writeClaude(true)
	var outputs []string
	run := func(args ...string) (string, string, int) {
		out, errOut, code := e.run("", args...)
		outputs = append(outputs, out, errOut)
		return out, errOut, code
	}

	// Not signed in: every non-interactive command fails with the hint.
	out, errOut, code := run("test", "gw")
	if code == 0 || !strings.Contains(errOut, "needs sign-in") || !strings.Contains(errOut, "o-mcp login gw") || strings.Contains(errOut, "unreachable") {
		t.Fatalf("test: %d\n%s%s", code, out, errOut)
	}
	if _, errOut, code := run("discover", "gw"); code == 0 || !strings.Contains(errOut, "sign in: o-mcp login gw") {
		t.Fatalf("discover: %d %s", code, errOut)
	}
	if _, errOut, code := run("call", "gw", "echo"); code == 0 || !strings.Contains(errOut, "o-mcp login gw") {
		t.Fatalf("call: %d %s", code, errOut)
	}
	var res DiscoverResult
	out, _, _ = run("discover", "--all")
	json.Unmarshal([]byte(out), &res)
	found := false
	for _, er := range res.Errors {
		if er.ID == "claude:user:gw" {
			found = er.NeedsLogin && !er.Unreachable && er.Login == "o-mcp login gw"
		}
	}
	if !found {
		t.Fatalf("discover --all errors = %+v", res.Errors)
	}
	rs := rows(t, e.ok("configured", "--state", "needs-sign-in"))
	if len(rs) != 1 || rs[0].ID != "mcp-server/claude:user:gw" || !strings.Contains(strings.Join(rs[0].Cells, " "), "needs sign-in") {
		t.Fatalf("configured needs-sign-in = %+v", rs)
	}

	// Sign in.
	e.tool.OpenURL = fake.Browser
	out, errOut, code = run("login", "gw")
	if code != 0 || !strings.Contains(out, `"signedIn": true`) || !strings.Contains(errOut, f.URL+"/authorize?") {
		t.Fatalf("login: %d\n%s%s", code, out, errOut)
	}
	opened := 0
	e.tool.OpenURL = func(string) error { opened++; return nil }
	if out, _, _ := run("login", "gw"); !strings.Contains(out, `"already": true`) || opened != 0 {
		t.Fatalf("login when signed in = %s (opened %d)", out, opened)
	}
	var st client.AuthStatus
	out, _, _ = run("auth", "status", "gw")
	if err := json.Unmarshal([]byte(out), &st); err != nil || !st.SignedIn || st.Issuer != f.URL || st.Expiry == nil || len(st.Scopes) == 0 {
		t.Fatalf("auth status = %s", out)
	}
	out, _, _ = run("auth", "status")
	var all []client.AuthStatus
	if json.Unmarshal([]byte(out), &all); len(all) != 2 { // gw and oauth-only
		t.Fatalf("auth status (all) = %s", out)
	}
	if rs := rows(t, e.ok("configured", "--state", "signed-in")); len(rs) != 1 {
		t.Fatalf("configured signed-in = %+v", rs)
	}

	// The stored token works without a browser.
	e.tool.OpenURL = func(u string) error { t.Errorf("a browser was opened for %s", u); return nil }
	if out, _, code := run("discover", "gw"); code != 0 || !strings.Contains(out, `"tools": 3`) {
		t.Fatalf("discover signed in: %d %s", code, out)
	}
	if out, _, code := run("call", "gw", "echo", "text=hi", "--extract"); code != 0 || !strings.Contains(out, `"hi"`) {
		t.Fatalf("call signed in: %d %s", code, out)
	}

	// Sign out.
	out, _, code = run("logout", "gw")
	if code != 0 || !strings.Contains(out, `"deleted": true`) || !strings.Contains(out, `"revoked": true`) {
		t.Fatalf("logout: %d %s", code, out)
	}
	if out, _, _ := run("auth", "status", "gw"); !strings.Contains(out, `"state": "signed-out"`) {
		t.Fatalf("after logout = %s", out)
	}
	if len(e.tool.Keyring.(*client.MemoryKeyring).Accounts(client.KeychainService)) != 0 {
		t.Fatal("logout left a keyring item")
	}

	// No token in any output or file.
	var issued []string
	f.Do(func(f *fake.OAuth) { issued = f.Issued })
	filepath.Walk(e.home, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			outputs = append(outputs, string(b))
		}
		return nil
	})
	for _, tok := range issued {
		for _, s := range outputs {
			if strings.Contains(s, tok) {
				t.Fatalf("token %s… leaked:\n%s", tok[:10], s)
			}
		}
	}
}
