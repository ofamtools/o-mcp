package client

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
)

// noBrowser is an OpenURL that fails the test: nothing but an explicit
// sign-in may open a browser.
func noBrowser(t *testing.T) func(string) error {
	return func(u string) error {
		t.Errorf("a browser was opened for %s", u)
		return nil
	}
}

// signInEnv is a fake authorization server with its MCP server, a server
// entry for it and options whose browser drives the redirect.
type signInEnv struct {
	t      *testing.T
	fake   *fake.OAuth
	server Server
	o      Options
	keys   *MemoryKeyring
	prompt *bytes.Buffer
}

func newSignIn(t *testing.T, raw map[string]any) *signInEnv {
	t.Helper()
	f := fake.NewOAuth()
	t.Cleanup(f.Close)
	if raw == nil {
		raw = map[string]any{}
	}
	raw["type"], raw["url"] = "http", f.Endpoint()
	s := NewServer("gw", raw)
	e := &signInEnv{t: t, fake: f, server: s, keys: NewMemoryKeyring(), prompt: &bytes.Buffer{}}
	e.o = opts(t)
	e.o.Keyring, e.o.Prompt = e.keys, e.prompt
	return e
}

// login runs the browser flow with the fake browser.
func (e *signInEnv) login() AuthStatus {
	e.t.Helper()
	o := e.o
	o.OpenURL = fake.Browser
	st, err := Login(context.Background(), e.server, o)
	if err != nil {
		e.t.Fatalf("login: %v", err)
	}
	return st
}

func (e *signInEnv) tools() error {
	sess, err := Connect(context.Background(), e.server, e.o)
	if err != nil {
		return err
	}
	defer sess.Close()
	_, err = sess.Tools(context.Background())
	return err
}

// noTokens fails when any issued token appears in the texts or in any
// file under the home (records, snapshots, history).
func (e *signInEnv) noTokens(texts ...string) {
	e.t.Helper()
	var issued []string
	e.fake.Do(func(f *fake.OAuth) { issued = append(issued, f.Issued...) })
	if len(issued) == 0 {
		e.t.Fatal("no tokens issued")
	}
	texts = append(texts, e.prompt.String())
	filepath.Walk(e.o.Home, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			texts = append(texts, string(b))
		}
		return nil
	})
	for _, tok := range issued {
		for _, s := range texts {
			if strings.Contains(s, tok) {
				e.t.Fatalf("token %s… appears in output or a file", tok[:10])
			}
		}
	}
}

func TestLoginDiscoversRegistersAndKeepsTokensInTheKeyring(t *testing.T) {
	e := newSignIn(t, nil)
	st := e.login()
	if !st.SignedIn || st.State != SignedIn || st.Issuer != e.fake.URL || st.Registration != "dynamic" || !reflect.DeepEqual(st.Scopes, []string{"read"}) || !st.Refreshable || st.Expiry == nil {
		t.Fatalf("status = %+v", st)
	}
	account := keychainAccount(e.server.ID, e.fake.URL)
	if got := e.keys.Accounts(KeychainService); !reflect.DeepEqual(got, []string{account}) {
		t.Fatalf("keyring accounts = %v, want [%s]", got, account)
	}
	if st.Keychain != "service o-mcp, account "+account {
		t.Fatalf("keychain = %q", st.Keychain)
	}
	e.fake.Do(func(f *fake.OAuth) {
		if len(f.Registered) != 1 || f.PKCEVerified != 1 || len(f.Authorizations) != 1 {
			t.Fatalf("registered %v, pkce %d, authorizations %d", f.Registered, f.PKCEVerified, len(f.Authorizations))
		}
		q := f.Authorizations[0]
		if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("resource") != f.Endpoint() || q.Get("client_id") != f.Registered[0] {
			t.Fatalf("authorize query = %v", q)
		}
		if !strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") || strings.HasSuffix(q.Get("redirect_uri"), ":0/callback") {
			t.Fatalf("redirect = %s", q.Get("redirect_uri"))
		}
	})
	if !strings.Contains(e.prompt.String(), e.fake.URL+"/authorize?") {
		t.Fatalf("the URL is not printed as a fallback: %q", e.prompt.String())
	}
	// Connect sends the stored token: no browser, no new sign-in.
	if err := e.tools(); err != nil {
		t.Fatal(err)
	}
	e.fake.Do(func(f *fake.OAuth) {
		if f.MCPRequests["valid"] == 0 || f.Exchanges != 1 {
			t.Fatalf("mcp requests %v, exchanges %d", f.MCPRequests, f.Exchanges)
		}
	})
	e.noTokens()
	// The record is the non-secret half.
	b, _ := os.ReadFile(recordPath(e.o.withDefaults(), e.server.ID))
	var rec AuthRecord
	if json.Unmarshal(b, &rec) != nil || rec.Account != account || rec.State != SignedIn {
		t.Fatalf("record = %s", b)
	}
}

func TestNeedsLoginNeverOpensABrowser(t *testing.T) {
	e := newSignIn(t, nil)
	err := e.tools()
	if !IsNeedsLogin(err) || IsUnreachable(err) {
		t.Fatalf("err = %v, want needs sign-in", err)
	}
	if !strings.Contains(err.Error(), "sign in: o-mcp login gw") {
		t.Fatalf("no login hint: %v", err)
	}
	if got := AuthState(e.server, e.o); got != NeedsSignIn {
		t.Fatalf("state = %q", got)
	}
	if st := Status(e.server, e.o); st.SignedIn || st.Login != "o-mcp login gw" {
		t.Fatalf("status = %+v", st)
	}
	if len(e.keys.Accounts(KeychainService)) != 0 {
		t.Fatal("a needs-sign-in connect stored something in the keyring")
	}
}

func TestInteractiveConnectSignsInThenConnects(t *testing.T) {
	e := newSignIn(t, nil)
	e.o.Interactive, e.o.OpenURL = true, fake.Browser
	if err := e.tools(); err != nil {
		t.Fatal(err)
	}
	e.fake.Do(func(f *fake.OAuth) {
		if f.Exchanges != 1 {
			t.Fatalf("exchanges = %d", f.Exchanges)
		}
	})
}

func TestRefreshBeforeExpiry(t *testing.T) {
	e := newSignIn(t, nil)
	e.fake.Do(func(f *fake.OAuth) { f.ExpiresIn = 30 }) // inside the refresh margin
	e.login()
	before, _ := e.keys.Get(KeychainService, keychainAccount(e.server.ID, e.fake.URL))
	e.fake.Do(func(f *fake.OAuth) { f.ExpiresIn = 3600 })
	if err := e.tools(); err != nil {
		t.Fatal(err)
	}
	e.fake.Do(func(f *fake.OAuth) {
		if f.Refreshes != 1 || f.MCPRequests["invalid"] != 0 {
			t.Fatalf("refreshes %d, requests %v", f.Refreshes, f.MCPRequests)
		}
	})
	after, _ := e.keys.Get(KeychainService, keychainAccount(e.server.ID, e.fake.URL))
	if bytes.Equal(before, after) {
		t.Fatal("the refreshed token was not stored")
	}
	// The next connect uses the stored refreshed token as is.
	if err := e.tools(); err != nil {
		t.Fatal(err)
	}
	e.fake.Do(func(f *fake.OAuth) {
		if f.Refreshes != 1 {
			t.Fatalf("refreshes = %d", f.Refreshes)
		}
	})
	e.noTokens()
}

func TestRefreshOnA401(t *testing.T) {
	e := newSignIn(t, nil)
	e.login()
	e.fake.Invalidate() // the server forgets the access token
	if err := e.tools(); err != nil {
		t.Fatal(err)
	}
	e.fake.Do(func(f *fake.OAuth) {
		if f.Refreshes != 1 || f.MCPRequests["invalid"] != 1 {
			t.Fatalf("refreshes %d, requests %v", f.Refreshes, f.MCPRequests)
		}
	})
}

func TestFailedRefreshAsksToSignInAgain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *signInEnv)
	}{
		{"expiry", func(e *signInEnv) { e.fake.Do(func(f *fake.OAuth) { f.ExpiresIn = 30 }) }},
		{"401", func(e *signInEnv) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSignIn(t, nil)
			tc.setup(e)
			e.login()
			e.fake.Invalidate()
			e.fake.Do(func(f *fake.OAuth) { f.FailRefresh = true })
			err := e.tools()
			if !IsNeedsLogin(err) || !strings.Contains(err.Error(), "sign in again: o-mcp login gw") || !strings.Contains(err.Error(), "invalid_grant") {
				t.Fatalf("err = %v", err)
			}
			if st := Status(e.server, e.o); st.SignedIn || st.State != NeedsSignIn {
				t.Fatalf("status = %+v", st)
			}
			e.noTokens(err.Error())
		})
	}
}

func TestConfiguredClientIDWithoutRegistration(t *testing.T) {
	e := newSignIn(t, nil)
	e.fake.Do(func(f *fake.OAuth) { f.NoRegistration = true })
	o := e.o
	o.OpenURL = fake.Browser
	if _, err := Login(context.Background(), e.server, o); err == nil || !strings.Contains(err.Error(), "oauth.clientId") {
		t.Fatalf("no registration, no client id = %v", err)
	}
	e2 := newSignIn(t, map[string]any{"oauth": map[string]any{"clientId": "pre-registered", "scopes": "read write"}})
	e2.fake.Do(func(f *fake.OAuth) { f.NoRegistration = true })
	st := e2.login()
	if st.Registration != "configured" {
		t.Fatalf("status = %+v", st)
	}
	e2.fake.Do(func(f *fake.OAuth) {
		q := f.Authorizations[0]
		if len(f.Registered) != 0 || q.Get("client_id") != "pre-registered" || q.Get("scope") != "read write" {
			t.Fatalf("registered %v, query %v", f.Registered, q)
		}
	})
	if err := e2.tools(); err != nil {
		t.Fatal(err)
	}
}

func TestLogoutRevokesAndDeletes(t *testing.T) {
	e := newSignIn(t, nil)
	e.login()
	res, err := Logout(context.Background(), e.server, e.o)
	if err != nil || !res.Deleted || !res.Revoked {
		t.Fatalf("logout = %+v, %v", res, err)
	}
	e.fake.Do(func(f *fake.OAuth) {
		if len(f.Revoked) != 2 {
			t.Fatalf("revoked %d tokens", len(f.Revoked))
		}
	})
	if len(e.keys.Accounts(KeychainService)) != 0 {
		t.Fatal("the keyring item is still there")
	}
	if st := Status(e.server, e.o); st.State != SignedOut || st.SignedIn {
		t.Fatalf("status = %+v", st)
	}
	out, _ := json.Marshal(res)
	e.noTokens(string(out))
}

func TestOAuthAppliesOnlyWithoutAConfiguredCredential(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"url":"https://x.test/mcp"}`:                                           true,
		`{"type":"sse","url":"https://x.test/sse"}`:                              true,
		`{"url":"https://x.test/mcp","headers":{"X-Org":"a"}}`:                   true,
		`{"url":"https://x.test/mcp","headers":{"authorization":"Bearer ${T}"}}`: false,
		`{"url":"https://x.test/mcp","bearer_token_env_var":"T"}`:                false,
		`{"url":"https://x.test/mcp","env_http_headers":{"Authorization":"T"}}`:  false,
		`{"command":"x"}`: false,
	} {
		var m map[string]any
		json.Unmarshal([]byte(raw), &m)
		if got := NewServer("x", m).OAuth(); got != want {
			t.Errorf("%s: OAuth = %v, want %v", raw, got, want)
		}
	}
}

func TestKeychainAccountAndArgs(t *testing.T) {
	if got := keychainAccount("claude:user:runlayer", "https://auth.example.com"); got != "claude:user:runlayer|https://auth.example.com" {
		t.Fatalf("account = %s", got)
	}
	if got := keychainAccount("a b|c", `x"y`); got != "a%20b%7Cc|x%22y" {
		t.Fatalf("escaped = %s", got)
	}
	if _, err := keychainArg("has space"); err == nil {
		t.Fatal("a blank was accepted")
	}
}

func TestMemoryKeyring(t *testing.T) {
	k := NewMemoryKeyring()
	if _, err := k.Get("s", "a"); err != ErrNoItem {
		t.Fatalf("empty get = %v", err)
	}
	k.Set("s", "a", []byte("one"))
	k.Set("s", "a", []byte("two"))
	if b, err := k.Get("s", "a"); err != nil || string(b) != "two" {
		t.Fatalf("get = %s %v", b, err)
	}
	if k.Delete("s", "a") != nil || k.Delete("s", "a") != ErrNoItem {
		t.Fatal("delete")
	}
}

// TestRealKeychain writes, reads and deletes one item (service
// o-mcp-test) in the real login keychain, with a secret long enough to
// need several chunks. It runs only with O_MCP_KEYCHAIN_TEST=1, on macOS.
func TestRealKeychain(t *testing.T) {
	if os.Getenv("O_MCP_KEYCHAIN_TEST") != "1" {
		t.Skip("set O_MCP_KEYCHAIN_TEST=1 to use the real Keychain")
	}
	k := Keychain()
	const svc, acct = "o-mcp-test", "test|https://issuer.example"
	secret := bytes.Repeat([]byte(`{"access_token":"xyz"}`), 500)
	defer k.Delete(svc, acct)
	if err := k.Set(svc, acct, secret); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Get(svc, acct); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("get = %d bytes, %v", len(got), err)
	}
	if err := k.Set(svc, acct, []byte("short")); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Get(svc, acct); err != nil || string(got) != "short" {
		t.Fatalf("after a shorter set = %q, %v", got, err)
	}
	if err := k.Delete(svc, acct); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get(svc, acct); err != ErrNoItem {
		t.Fatalf("after delete = %v", err)
	}
}
