package client

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// OAuth sign-in (the MCP authorization flow, OAuth 2.1). A remote server
// whose configuration gives no Authorization credential may answer 401
// with a WWW-Authenticate challenge; `o-mcp login SERVER` (Login) then
// discovers the protected-resource and authorization-server metadata,
// registers a client (the config's oauth.clientId, else dynamic client
// registration), runs the authorization-code flow with PKCE (S256) and a
// loopback redirect on 127.0.0.1, and keeps the tokens in the Keychain.
// Connect then sends them and refreshes them before they expire and when
// the server answers 401. Nothing but Login opens a browser: a connection
// that needs sign-in fails with a NeedsLoginError naming the command.
//
// Two records per signed-in server:
//   - the Keychain item (service "o-mcp", account "SERVER_ID|ISSUER"): the
//     access and refresh tokens, the client id and secret and the token
//     and revocation endpoints, as JSON. Only the Keychain holds them.
//   - auth/ in o-mcp's data directory, one file per server: the state
//     (signed-in, needs-sign-in), issuer, scopes, expiry and the Keychain
//     account. No token, no secret.

// Sign-in states.
const (
	SignedIn    = "signed-in"
	NeedsSignIn = "needs-sign-in"
	SignedOut   = "signed-out"
)

// refreshMargin is how long before expiry a token is refreshed.
const refreshMargin = time.Minute

// NeedsLoginError is a server that needs an OAuth sign-in: it answered
// 401 and no stored token works (none stored, or refreshing failed). It
// is not an UnreachableError. Its message names the command that signs in.
type NeedsLoginError struct {
	Server string // the server ID
	Name   string // what `o-mcp login` takes
	Reason string
	Again  bool // a stored sign-in stopped working
}

func (e *NeedsLoginError) Error() string {
	verb := "sign in"
	if e.Again {
		verb = "sign in again"
	}
	return fmt.Sprintf("%s needs sign-in (%s); %s: %s", e.Server, e.Reason, verb, e.Command())
}

// Command is the command line that signs in.
func (e *NeedsLoginError) Command() string { return LoginCommand(e.Name) }

// LoginCommand is `o-mcp login NAME`, NAME quoted for a shell when needed.
func LoginCommand(name string) string {
	if name == "" || strings.IndexFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r))
	}) >= 0 {
		name = "'" + strings.ReplaceAll(name, "'", `'\''`) + "'"
	}
	return "o-mcp login " + name
}

// IsNeedsLogin reports whether err is a NeedsLoginError.
func IsNeedsLogin(err error) bool {
	var n *NeedsLoginError
	return errors.As(err, &n)
}

func needsLogin(s Server, reason string, again bool) *NeedsLoginError {
	name := s.Name
	if name == "" {
		name = s.ID
	}
	return &NeedsLoginError{Server: s.ID, Name: name, Reason: reason, Again: again}
}

// OAuth reports whether sign-in applies to the server: a remote server
// (Streamable HTTP or SSE) whose configuration gives no Authorization
// header and no bearer-token variable.
func (s Server) OAuth() bool {
	if s.Transport != "http" && s.Transport != "sse" {
		return false
	}
	if s.BearerTokenEnv != "" {
		return false
	}
	for _, h := range s.Headers {
		if strings.EqualFold(h, "Authorization") {
			return false
		}
	}
	for _, h := range s.HeaderEnv {
		if name, _, _ := strings.Cut(h, "="); strings.EqualFold(name, "Authorization") {
			return false
		}
	}
	return true
}

// oauthSettings are the optional "oauth" object of a server's config
// entry (Claude Code's spelling): clientId (or client_id), a client
// registered in advance for an authorization server without dynamic
// registration; callbackPort, a fixed loopback port for a client
// registered with one redirect URI; scopes, a list or a string.
type oauthSettings struct {
	clientID     string
	callbackPort int
	scopes       []string
}

func (s Server) oauthSettings() oauthSettings {
	m := object(s.raw["oauth"])
	var o oauthSettings
	o.clientID = text(m, "clientId")
	if o.clientID == "" {
		o.clientID = text(m, "client_id")
	}
	switch p := m["callbackPort"].(type) {
	case float64:
		o.callbackPort = int(p)
	case int64:
		o.callbackPort = int(p)
	case string:
		o.callbackPort, _ = strconv.Atoi(p)
	}
	switch sc := m["scopes"].(type) {
	case string:
		o.scopes = strings.Fields(strings.ReplaceAll(sc, ",", " "))
	default:
		o.scopes = stringList(sc)
	}
	return o
}

// AuthRecord is the non-secret record of a server's sign-in, a file under
// auth/ in o-mcp's data directory.
type AuthRecord struct {
	Server       string     `json:"server"`
	Name         string     `json:"name"`
	State        string     `json:"state"` // signed-in, needs-sign-in
	Reason       string     `json:"reason,omitempty"`
	Resource     string     `json:"resource,omitempty"`
	Issuer       string     `json:"issuer,omitempty"`
	Account      string     `json:"account,omitempty"` // the Keychain account, service o-mcp
	Scopes       []string   `json:"scopes,omitempty"`
	Expiry       *time.Time `json:"expiry,omitempty"`
	Registration string     `json:"registration,omitempty"` // dynamic or configured (oauth.clientId)
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// credential is the Keychain item: everything a refresh needs.
type credential struct {
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	TokenType     string    `json:"token_type,omitempty"`
	Expiry        time.Time `json:"expiry,omitzero"`
	Scopes        []string  `json:"scopes,omitempty"`
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret,omitempty"`
	AuthStyle     int       `json:"auth_style,omitempty"`
	TokenURL      string    `json:"token_url"`
	RevocationURL string    `json:"revocation_url,omitempty"`
	Resource      string    `json:"resource,omitempty"`
}

func (c *credential) secrets() []string {
	return []string{c.AccessToken, c.RefreshToken, c.ClientSecret}
}

// keychainAccount is the Keychain account of a server and issuer: the
// server ID, |, the issuer, with blanks, quotes and % escaped.
func keychainAccount(id, issuer string) string {
	esc := func(s string) string {
		var b strings.Builder
		for _, c := range []byte(s) {
			if c <= ' ' || c >= 0x7f || strings.IndexByte("\"'\\%|#", c) >= 0 {
				fmt.Fprintf(&b, "%%%02X", c)
			} else {
				b.WriteByte(c)
			}
		}
		return b.String()
	}
	return esc(id) + "|" + esc(issuer)
}

func (o Options) authDir() string {
	if o.AuthDir != "" {
		return o.AuthDir
	}
	return filepath.Join(DefaultStore(o.Home, o.Getenv).Dir, "auth")
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func recordPath(o Options, id string) string { return filepath.Join(o.authDir(), safeName(id)+".json") }

func loadRecord(o Options, id string) (AuthRecord, bool) {
	b, err := os.ReadFile(recordPath(o, id))
	if err != nil {
		return AuthRecord{}, false
	}
	var r AuthRecord
	if json.Unmarshal(b, &r) != nil || r.Server != id {
		return AuthRecord{}, false
	}
	return r, true
}

func saveRecord(o Options, r AuthRecord) error {
	r.UpdatedAt = o.now().UTC()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(recordPath(o, r.Server), append(b, '\n'))
}

// AuthRecords are every server's sign-in record, sorted by server ID. They
// read files only, never the Keychain.
func AuthRecords(o Options) []AuthRecord {
	o = o.withDefaults()
	files, _ := filepath.Glob(filepath.Join(o.authDir(), "*.json"))
	var out []AuthRecord
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var r AuthRecord
		if json.Unmarshal(b, &r) == nil && r.Server != "" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Server < out[j].Server })
	return out
}

// AuthState is the server's sign-in state from its record alone (no
// Keychain read): signed-in, needs-sign-in, signed-out, or "" when
// sign-in does not apply.
func AuthState(s Server, o Options) string {
	if !s.OAuth() {
		return ""
	}
	o = o.withDefaults()
	if r, ok := loadRecord(o, s.ID); ok {
		return r.State
	}
	return SignedOut
}

// AuthStatus is a server's sign-in, safe to print: never a token.
type AuthStatus struct {
	Server       string     `json:"server"`
	Name         string     `json:"name"`
	State        string     `json:"state"` // signed-in, needs-sign-in, signed-out
	SignedIn     bool       `json:"signedIn"`
	Reason       string     `json:"reason,omitempty"`
	Issuer       string     `json:"issuer,omitempty"`
	Scopes       []string   `json:"scopes,omitempty"`
	Expiry       *time.Time `json:"expiry,omitempty"`
	Expired      bool       `json:"expired,omitempty"`
	Refreshable  bool       `json:"refreshable,omitempty"` // a refresh token is stored
	Registration string     `json:"registration,omitempty"`
	Keychain     string     `json:"keychain,omitempty"` // "service o-mcp, account ..."
	Login        string     `json:"login,omitempty"`    // the command that signs in, when not signed in
}

// Status is the server's sign-in: its record, checked against the
// Keychain item (read, never printed).
func Status(s Server, o Options) AuthStatus {
	o = o.withDefaults()
	st := AuthStatus{Server: s.ID, Name: s.Name, State: SignedOut}
	if st.Name == "" {
		st.Name = s.ID
	}
	r, ok := loadRecord(o, s.ID)
	if ok {
		st.State, st.Reason, st.Issuer, st.Scopes, st.Expiry, st.Registration = r.State, r.Reason, r.Issuer, r.Scopes, r.Expiry, r.Registration
		if r.Account != "" {
			st.Keychain = "service " + KeychainService + ", account " + r.Account
		}
	}
	if st.State == SignedIn {
		cred, err := loadCredential(o, r.Account)
		switch {
		case err != nil:
			st.State, st.Reason = NeedsSignIn, redact(err.Error(), nil)
		default:
			st.Refreshable = cred.RefreshToken != ""
			if !cred.Expiry.IsZero() && !o.now().Before(cred.Expiry) {
				st.Expired = true
				if !st.Refreshable {
					st.State, st.Reason = NeedsSignIn, "the token expired and no refresh token is stored"
				}
			}
		}
	}
	st.SignedIn = st.State == SignedIn
	if !st.SignedIn {
		st.Login = LoginCommand(st.Name)
	}
	return st
}

func loadCredential(o Options, account string) (*credential, error) {
	if account == "" {
		return nil, errors.New("no Keychain account recorded")
	}
	b, err := o.Keyring.Get(KeychainService, account)
	if errors.Is(err, ErrNoItem) {
		return nil, errors.New("its Keychain item is gone")
	}
	if err != nil {
		return nil, err
	}
	var c credential
	if err := json.Unmarshal(b, &c); err != nil || c.AccessToken == "" {
		return nil, errors.New("its Keychain item is not an o-mcp sign-in")
	}
	return &c, nil
}

func storeCredential(o Options, account string, c *credential) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := o.Keyring.Set(KeychainService, account, b); err != nil {
		return errors.New(redact(err.Error(), c.secrets()))
	}
	return nil
}

// redact is s with the known secrets and anything secret-looking replaced.
func redact(s string, secrets []string) string {
	for _, v := range secrets {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return RedactValues(s).(string)
}

// oauthHandler is the go-sdk's auth.OAuthHandler for a server that signs
// in: TokenSource answers the stored token (refreshed when it is about to
// expire), Authorize answers a 401 by refreshing once, else with a
// NeedsLoginError. It never opens a browser.
type oauthHandler struct {
	s Server
	o Options

	mu        sync.Mutex
	loaded    bool
	rec       AuthRecord
	cred      *credential
	retried   bool // Authorize refreshed once already
	needs     *NeedsLoginError
	authorize bool // Authorize was called (the server asked for a token)
}

var _ auth.OAuthHandler = (*oauthHandler)(nil)

func newOAuthHandler(s Server, o Options) *oauthHandler { return &oauthHandler{s: s, o: o} }

func (h *oauthHandler) loadLocked() {
	if h.loaded {
		return
	}
	h.loaded = true
	r, ok := loadRecord(h.o, h.s.ID)
	if !ok || r.State != SignedIn {
		return
	}
	h.rec = r
	cred, err := loadCredential(h.o, r.Account)
	if err != nil {
		h.needs = needsLogin(h.s, redact(err.Error(), nil), true)
		return
	}
	h.cred = cred
}

func (h *oauthHandler) TokenSource(context.Context) (oauth2.TokenSource, error) { return h, nil }

// Token is the stored access token, refreshed first when it expires
// within refreshMargin; nil (no Authorization header) when none is stored.
func (h *oauthHandler) Token() (*oauth2.Token, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadLocked()
	if h.cred == nil {
		return nil, nil
	}
	if h.cred.Expiry.IsZero() || h.o.now().Add(refreshMargin).Before(h.cred.Expiry) {
		return h.token(), nil
	}
	if h.cred.RefreshToken == "" {
		h.needs = needsLogin(h.s, "its token expired and the server gave no refresh token", true)
		h.cred = nil
		return nil, h.needs
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.o.Timeout)
	defer cancel()
	if err := h.refreshLocked(ctx); err != nil {
		h.needs = needsLogin(h.s, "refreshing its token failed: "+err.Error(), true)
		h.cred = nil
		return nil, h.needs
	}
	return h.token(), nil
}

func (h *oauthHandler) token() *oauth2.Token {
	return &oauth2.Token{AccessToken: h.cred.AccessToken, TokenType: "Bearer", Expiry: h.cred.Expiry}
}

// Authorize answers a 401 or 403. A 403 that is not insufficient_scope
// is retried as is (the go-sdk's rule; a second 403 is unreachable).
func (h *oauthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	defer resp.Body.Close()
	defer io.Copy(io.Discard, resp.Body)
	challenges, _ := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	if resp.StatusCode == http.StatusForbidden && bearerParam(challenges, "error") != "insufficient_scope" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authorize = true
	h.loadLocked()
	if h.needs != nil {
		return h.needs
	}
	var reason string
	switch {
	case resp.StatusCode == http.StatusForbidden:
		reason = "the server asks for more scopes (insufficient_scope)"
	case h.cred != nil && h.cred.RefreshToken != "" && !h.retried:
		h.retried = true
		err := h.refreshLocked(ctx)
		if err == nil {
			return nil
		}
		reason = "the server rejected its token and refreshing failed: " + err.Error()
	case h.cred != nil:
		reason = "the server rejected its stored token"
	default:
		reason = "the server asks for OAuth sign-in (401)"
	}
	h.needs = needsLogin(h.s, reason, h.cred != nil)
	h.cred = nil
	return h.needs
}

// result is the handler's verdict after a failed connect: the
// NeedsLoginError, else one when the server answered 401 at all.
func (h *oauthHandler) result(code int) *NeedsLoginError {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.needs != nil {
		return h.needs
	}
	if code == http.StatusUnauthorized {
		return needsLogin(h.s, "the server answered 401", h.rec.State == SignedIn)
	}
	return nil
}

// refreshLocked trades the refresh token for a new access token and
// stores it (the Keychain item and the record's expiry).
func (h *oauthHandler) refreshLocked(ctx context.Context) error {
	next, err := refreshCredential(ctx, &http.Client{Timeout: h.o.Timeout}, h.cred, h.o.now())
	if err != nil {
		return err
	}
	if err := storeCredential(h.o, h.rec.Account, next); err != nil {
		return err
	}
	h.cred = next
	h.rec.Expiry = expiryOf(next)
	if len(next.Scopes) > 0 {
		h.rec.Scopes = next.Scopes
	}
	return saveRecord(h.o, h.rec)
}

// markNeedsLogin records that the server needs sign-in, keeping the
// Keychain account (logout still deletes the item).
func markNeedsLogin(o Options, s Server, n *NeedsLoginError) {
	r, ok := loadRecord(o, s.ID)
	if !ok {
		r = AuthRecord{Server: s.ID, Name: s.Name}
	}
	r.State, r.Reason = NeedsSignIn, n.Reason
	_ = saveRecord(o, r)
}

func expiryOf(c *credential) *time.Time {
	if c.Expiry.IsZero() {
		return nil
	}
	e := c.Expiry.UTC()
	return &e
}

func bearerParam(cs []oauthex.Challenge, key string) string {
	for _, c := range cs {
		if c.Scheme == "bearer" && c.Params[key] != "" {
			return c.Params[key]
		}
	}
	return ""
}

// tokenResponse is a token endpoint's answer (RFC 6749 5.1, 5.2).
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// refreshCredential is the refresh-token grant, with the resource
// parameter (RFC 8707) the MCP authorization spec asks for. Its errors
// name the endpoint's error code, never a token.
func refreshCredential(ctx context.Context, hc *http.Client, c *credential, now time.Time) (*credential, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.RefreshToken}}
	if c.Resource != "" {
		form.Set("resource", c.Resource)
	}
	req, err := clientRequest(ctx, c, c.TokenURL, form)
	if err != nil {
		return nil, err
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, errors.New(redact(err.Error(), c.secrets()))
	}
	defer res.Body.Close()
	var tr tokenResponse
	_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&tr)
	if res.StatusCode != http.StatusOK || tr.AccessToken == "" {
		msg := fmt.Sprintf("the token endpoint answered %d", res.StatusCode)
		if tr.Error != "" {
			msg += " " + tr.Error
			if tr.ErrorDescription != "" {
				msg += ": " + tr.ErrorDescription
			}
		}
		return nil, errors.New(redact(msg, c.secrets()))
	}
	next := *c
	next.AccessToken, next.TokenType = tr.AccessToken, tr.TokenType
	if tr.RefreshToken != "" {
		next.RefreshToken = tr.RefreshToken
	}
	next.Expiry = time.Time{}
	if tr.ExpiresIn > 0 {
		next.Expiry = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	if tr.Scope != "" {
		next.Scopes = strings.Fields(tr.Scope)
	}
	return &next, nil
}

// clientRequest is a form POST to an authorization server endpoint with
// the client's authentication: HTTP Basic for client_secret_basic, else
// client_id (and client_secret) in the form.
func clientRequest(ctx context.Context, c *credential, endpoint string, form url.Values) (*http.Request, error) {
	basic := c.ClientSecret != "" && oauth2.AuthStyle(c.AuthStyle) == oauth2.AuthStyleInHeader
	if !basic {
		form.Set("client_id", c.ClientID)
		if c.ClientSecret != "" {
			form.Set("client_secret", c.ClientSecret)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.ClientSecret))
	}
	return req, nil
}

// Login runs the browser sign-in for the server and stores the tokens in
// the Keychain: it asks the server (which must answer 401), discovers its
// authorization server, registers a client, prints the authorization URL
// to o.Prompt and opens it with o.OpenURL, waits for the loopback redirect
// (o.SignInTimeout), and exchanges the code. It always runs the flow; a
// caller that wants "already signed in" checks Status first. It is the
// only function that opens a browser.
func Login(ctx context.Context, s Server, o Options) (AuthStatus, error) {
	if s.Disabled {
		return AuthStatus{}, fmt.Errorf("%s: %w", s.ID, ErrDisabled)
	}
	if !s.OAuth() {
		return AuthStatus{}, fmt.Errorf("%s: sign-in does not apply: it is not a remote server, or its config gives its own credential (an Authorization header or a bearer-token variable)", s.ID)
	}
	o = o.withDefaults()
	endpoint, headers, err := remote(s, o)
	if err != nil {
		return AuthStatus{}, fmt.Errorf("%s: %w", s.ID, err)
	}
	hc := &http.Client{Timeout: o.Timeout}
	challenges, err := probe(ctx, hc, endpoint, headers)
	if err != nil {
		return AuthStatus{}, fmt.Errorf("%s: %w", s.ID, err)
	}
	cred, rec, err := signIn(ctx, hc, s, o, endpoint, challenges)
	if err != nil {
		return AuthStatus{}, fmt.Errorf("%s: sign-in: %w", s.ID, err)
	}
	if old, ok := loadRecord(o, s.ID); ok && old.Account != "" && old.Account != rec.Account {
		_ = o.Keyring.Delete(KeychainService, old.Account) // a different issuer now
	}
	if err := storeCredential(o, rec.Account, cred); err != nil {
		return AuthStatus{}, fmt.Errorf("%s: %w", s.ID, err)
	}
	if err := saveRecord(o, rec); err != nil {
		return AuthStatus{}, fmt.Errorf("%s: %w", s.ID, err)
	}
	return Status(s, o), nil
}

// probe sends the server an initialize request without a token and
// answers the challenges of its 401 (or of a 403 insufficient_scope).
func probe(ctx context.Context, hc *http.Client, endpoint string, headers map[string]string) ([]oauthex.Challenge, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"o-mcp","version":"` + Version + `"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	challenges, _ := oauthex.ParseWWWAuthenticate(res.Header.Values("WWW-Authenticate"))
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return challenges, nil
	case res.StatusCode == http.StatusForbidden && bearerParam(challenges, "error") == "insufficient_scope":
		return challenges, nil
	case res.StatusCode < 300:
		return nil, fmt.Errorf("the server answered %d without a token: it needs no sign-in", res.StatusCode)
	}
	return nil, fmt.Errorf("the server answered %d, not 401: it does not ask for OAuth sign-in", res.StatusCode)
}

// signIn is the flow after the challenge: metadata, registration, the
// browser, the code exchange. It answers the Keychain item and the
// record, stored by the caller.
func signIn(ctx context.Context, hc *http.Client, s Server, o Options, endpoint string, challenges []oauthex.Challenge) (*credential, AuthRecord, error) {
	prm, err := protectedResource(ctx, hc, endpoint, challenges)
	if err != nil {
		return nil, AuthRecord{}, err
	}
	issuer := prm.AuthorizationServers[0]
	asm, err := auth.GetAuthServerMetadata(ctx, issuer, hc)
	if err != nil {
		return nil, AuthRecord{}, fmt.Errorf("authorization server metadata: %w", err)
	}
	if asm == nil {
		// The 2025-03-26 spec's fallback: endpoints under the issuer.
		asm = &oauthex.AuthServerMeta{Issuer: issuer, AuthorizationEndpoint: issuer + "/authorize", TokenEndpoint: issuer + "/token", RegistrationEndpoint: issuer + "/register"}
	}
	settings := s.oauthSettings()
	scopes := settings.scopes
	if len(scopes) == 0 {
		scopes = strings.Fields(bearerParam(challenges, "scope"))
	}
	if len(scopes) == 0 {
		scopes = prm.ScopesSupported
	}

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(settings.callbackPort)))
	if err != nil {
		return nil, AuthRecord{}, fmt.Errorf("loopback redirect: %w", err)
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	cred := &credential{TokenURL: asm.TokenEndpoint, RevocationURL: asm.RevocationEndpoint, Resource: prm.Resource}
	registration := "configured"
	switch {
	case settings.clientID != "":
		cred.ClientID, cred.AuthStyle = settings.clientID, int(oauth2.AuthStyleInParams)
	case asm.RegistrationEndpoint != "":
		registration = "dynamic"
		reg, err := oauthex.RegisterClient(ctx, asm.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
			RedirectURIs:            []string{redirect},
			TokenEndpointAuthMethod: "none",
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			ClientName:              "o-mcp",
			Scope:                   strings.Join(scopes, " "),
			ApplicationType:         "native",
		}, hc)
		if err != nil {
			return nil, AuthRecord{}, fmt.Errorf("client registration at %s: %s", asm.RegistrationEndpoint, redact(err.Error(), nil))
		}
		cred.ClientID, cred.ClientSecret = reg.ClientID, reg.ClientSecret
		cred.AuthStyle = int(authStyle(reg.TokenEndpointAuthMethod, reg.ClientSecret))
	default:
		return nil, AuthRecord{}, fmt.Errorf("the authorization server %s offers no dynamic client registration: set oauth.clientId in %s's entry in %s", issuer, s.Name, s.Config)
	}

	cfg := &oauth2.Config{
		ClientID:     cred.ClientID,
		ClientSecret: cred.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: asm.AuthorizationEndpoint, TokenURL: asm.TokenEndpoint, AuthStyle: oauth2.AuthStyle(cred.AuthStyle)},
		RedirectURL:  redirect,
		Scopes:       scopes,
	}
	verifier := oauth2.GenerateVerifier()
	state := rand.Text()
	authURL := cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("resource", prm.Resource))

	code, err := awaitCode(ctx, ln, state, o, func() {
		fmt.Fprintf(o.Prompt, "Sign in to %s in your browser. If it does not open, open this URL:\n\n  %s\n\n", s.Name, authURL)
		if err := o.OpenURL(authURL); err != nil {
			fmt.Fprintf(o.Prompt, "(could not open a browser: %v)\n", err)
		}
	})
	if err != nil {
		return nil, AuthRecord{}, err
	}
	tok, err := cfg.Exchange(context.WithValue(ctx, oauth2.HTTPClient, hc), code, oauth2.VerifierOption(verifier), oauth2.SetAuthURLParam("resource", prm.Resource))
	if err != nil {
		return nil, AuthRecord{}, fmt.Errorf("token exchange: %s", exchangeError(err, cred))
	}
	cred.AccessToken, cred.RefreshToken, cred.TokenType, cred.Expiry = tok.AccessToken, tok.RefreshToken, tok.TokenType, tok.Expiry
	cred.Scopes = scopes
	if sc, _ := tok.Extra("scope").(string); sc != "" {
		cred.Scopes = strings.Fields(sc)
	}
	rec := AuthRecord{
		Server: s.ID, Name: s.Name, State: SignedIn, Resource: prm.Resource, Issuer: asm.Issuer,
		Account: keychainAccount(s.ID, asm.Issuer), Scopes: cred.Scopes, Expiry: expiryOf(cred), Registration: registration,
	}
	return cred, rec, nil
}

// exchangeError is the token exchange's failure without a body that could
// carry a secret: the endpoint's error code and description.
func exchangeError(err error, c *credential) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		msg := "the token endpoint answered"
		if re.Response != nil {
			msg += " " + strconv.Itoa(re.Response.StatusCode)
		}
		if re.ErrorCode != "" {
			msg += " " + re.ErrorCode
		}
		if re.ErrorDescription != "" {
			msg += ": " + re.ErrorDescription
		}
		return redact(msg, c.secrets())
	}
	return redact(err.Error(), c.secrets())
}

func authStyle(method, secret string) oauth2.AuthStyle {
	switch {
	case method == "client_secret_basic":
		return oauth2.AuthStyleInHeader
	case method == "client_secret_post", method == "none", secret == "":
		return oauth2.AuthStyleInParams
	}
	return oauth2.AuthStyleInHeader // RFC 7591's default
}

// protectedResource is the server's protected-resource metadata (RFC
// 9728), in the MCP spec's order: the challenge's resource_metadata, the
// well-known URL at the endpoint's path, then at the root. Without any,
// the server's origin is its authorization server (the 2025-03-26 spec).
func protectedResource(ctx context.Context, hc *http.Client, endpoint string, challenges []oauthex.Challenge) (*oauthex.ProtectedResourceMetadata, error) {
	type candidate struct{ url, resource string }
	var cands []candidate
	for _, c := range challenges {
		if u := c.Params["resource_metadata"]; u != "" {
			cands = append(cands, candidate{u, endpoint})
			break
		}
	}
	ru, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	mu := *ru
	mu.RawQuery, mu.Fragment = "", ""
	mu.Path = "/.well-known/oauth-protected-resource/" + strings.TrimLeft(ru.Path, "/")
	cands = append(cands, candidate{mu.String(), endpoint})
	mu.Path = "/.well-known/oauth-protected-resource"
	root := *ru
	root.Path, root.RawQuery, root.Fragment = "", "", ""
	cands = append(cands, candidate{mu.String(), root.String()})
	for _, c := range cands {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, c.url, c.resource, hc)
		if err != nil || prm == nil {
			continue
		}
		if len(prm.AuthorizationServers) == 0 {
			return nil, fmt.Errorf("protected resource metadata at %s names no authorization server", c.url)
		}
		return prm, nil
	}
	return &oauthex.ProtectedResourceMetadata{Resource: endpoint, AuthorizationServers: []string{root.String()}}, nil
}

// awaitCode serves the loopback redirect, calls start (print and open the
// URL), and answers the code once a redirect with the right state comes.
func awaitCode(ctx context.Context, ln net.Listener, state string, o Options, start func()) (string, error) {
	type answer struct {
		code string
		err  error
	}
	ch := make(chan answer, 1)
	var once sync.Once
	send := func(a answer) { once.Do(func() { ch <- a }) }
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("state") != state:
			http.Error(w, "o-mcp: this sign-in link is stale (state mismatch); start again with o-mcp login", http.StatusBadRequest)
			send(answer{err: errors.New("the redirect's state does not match (a stale or forged link)")})
		case q.Get("error") != "":
			http.Error(w, "o-mcp: sign-in failed: "+q.Get("error"), http.StatusBadRequest)
			msg := q.Get("error")
			if d := q.Get("error_description"); d != "" {
				msg += ": " + d
			}
			send(answer{err: errors.New("the authorization server answered " + redact(msg, nil))})
		case q.Get("code") == "":
			http.Error(w, "o-mcp: no code in the redirect", http.StatusBadRequest)
			send(answer{err: errors.New("the redirect carried no code")})
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<!doctype html><title>o-mcp</title><p>Signed in. You can close this tab and go back to the terminal.</p>")
			send(answer{code: q.Get("code")})
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()
	start()
	ctx, cancel := context.WithTimeout(ctx, o.SignInTimeout)
	defer cancel()
	select {
	case a := <-ch:
		return a.code, a.err
	case <-ctx.Done():
		return "", fmt.Errorf("no redirect within %s: %w", o.SignInTimeout, ctx.Err())
	}
}

// LogoutResult is what Logout removed.
type LogoutResult struct {
	Server  string `json:"server"`
	Deleted bool   `json:"deleted"`           // the Keychain item and the record
	Revoked bool   `json:"revoked,omitempty"` // the authorization server revoked the tokens
	Revoke  string `json:"revoke,omitempty"`  // why revocation was skipped or failed
}

// Logout revokes the server's tokens when its authorization server offers
// revocation (RFC 7009), then deletes the Keychain item and the record.
func Logout(ctx context.Context, s Server, o Options) (LogoutResult, error) {
	o = o.withDefaults()
	res := LogoutResult{Server: s.ID}
	r, ok := loadRecord(o, s.ID)
	if !ok {
		res.Revoke = "not signed in"
		return res, nil
	}
	if r.Account != "" {
		if cred, err := loadCredential(o, r.Account); err == nil {
			res.Revoked, res.Revoke = revoke(ctx, &http.Client{Timeout: o.Timeout}, cred)
		} else {
			res.Revoke = redact(err.Error(), nil)
		}
		if err := o.Keyring.Delete(KeychainService, r.Account); err != nil && !errors.Is(err, ErrNoItem) {
			return res, fmt.Errorf("%s: %w", s.ID, err)
		}
	}
	if err := os.Remove(recordPath(o, s.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	res.Deleted = true
	return res, nil
}

func revoke(ctx context.Context, hc *http.Client, c *credential) (bool, string) {
	if c.RevocationURL == "" {
		return false, "the authorization server offers no revocation endpoint"
	}
	tokens := [][2]string{{c.RefreshToken, "refresh_token"}, {c.AccessToken, "access_token"}}
	for _, t := range tokens {
		if t[0] == "" {
			continue
		}
		req, err := clientRequest(ctx, c, c.RevocationURL, url.Values{"token": {t[0]}, "token_type_hint": {t[1]}})
		if err != nil {
			return false, redact(err.Error(), c.secrets())
		}
		res, err := hc.Do(req)
		if err != nil {
			return false, redact(err.Error(), c.secrets())
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return false, fmt.Sprintf("the revocation endpoint answered %d", res.StatusCode)
		}
	}
	return true, ""
}

// openURL opens a URL in the default browser: open on macOS, xdg-open
// elsewhere.
func openURL(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, u).Run()
}
