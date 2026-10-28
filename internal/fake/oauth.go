package fake

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// OAuth is a fake OAuth 2.1 authorization server and an MCP server behind
// it, on one httptest server: the MCP endpoint (/mcp) answers 401 with a
// WWW-Authenticate challenge until a request carries a token it issued;
// it serves protected-resource and authorization-server metadata,
// dynamic client registration, an authorize endpoint that redirects
// straight back (a test drives it by GETting the authorization URL), a
// token endpoint (authorization code with PKCE S256, refresh) and
// revocation. Tokens are random, so a test can look for them in output.
type OAuth struct {
	*httptest.Server

	mu sync.Mutex
	// Settings a test may change.
	ExpiresIn      int64 // seconds; 0 omits expires_in
	NoRegistration bool  // no registration_endpoint in the metadata
	FailRefresh    bool  // the refresh grant answers invalid_grant
	// What happened.
	Registered     []string          // client ids
	Authorizations []url.Values      // the authorize requests' queries
	Exchanges      int               // codes exchanged
	Refreshes      int               // refresh grants answered
	Revoked        []string          // tokens revoked
	PKCEVerified   int               // codes whose verifier matched their S256 challenge
	MCPRequests    map[string]int    // by Authorization: "" or "valid" or "invalid"
	Issued         []string          // every access and refresh token issued
	access         map[string]bool   // valid access tokens
	refresh        map[string]string // refresh token -> client id
	codes          map[string]grant
}

type grant struct {
	clientID, redirect, challenge, resource string
}

// NewOAuth starts the fake.
func NewOAuth() *OAuth {
	f := &OAuth{ExpiresIn: 3600, MCPRequests: map[string]int{}, access: map[string]bool{}, refresh: map[string]string{}, codes: map[string]grant{}}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return Server() }, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"resource": f.URL + "/mcp", "authorization_servers": []string{f.URL}, "scopes_supported": []string{"read", "write"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		meta := map[string]any{
			"issuer": f.URL, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token",
			"revocation_endpoint": f.URL + "/revoke", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported": []string{"S256"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
		}
		if !f.NoRegistration {
			meta["registration_endpoint"] = f.URL + "/register"
		}
		writeJSON(w, 200, meta)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var meta map[string]any
		json.NewDecoder(r.Body).Decode(&meta)
		f.mu.Lock()
		id := "client-" + rand.Text()[:8]
		f.Registered = append(f.Registered, id)
		f.mu.Unlock()
		meta["client_id"] = id
		writeJSON(w, 201, meta)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.Authorizations = append(f.Authorizations, q)
		code := "code-" + rand.Text()
		f.codes[code] = grant{clientID: q.Get("client_id"), redirect: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), resource: q.Get("resource")}
		f.mu.Unlock()
		if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "PKCE S256 required", 400)
			return
		}
		back, _ := url.Parse(q.Get("redirect_uri"))
		bq := back.Query()
		bq.Set("code", code)
		bq.Set("state", q.Get("state"))
		back.RawQuery = bq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			g, ok := f.codes[r.Form.Get("code")]
			delete(f.codes, r.Form.Get("code"))
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge || g.redirect != r.Form.Get("redirect_uri") || g.clientID != r.Form.Get("client_id") || g.resource != r.Form.Get("resource") {
				writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
			f.PKCEVerified++
			f.Exchanges++
			writeJSON(w, 200, f.issueLocked(g.clientID))
		case "refresh_token":
			id, ok := f.refresh[r.Form.Get("refresh_token")]
			if f.FailRefresh || !ok || id != r.Form.Get("client_id") {
				writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "refresh token is not valid"})
				return
			}
			delete(f.refresh, r.Form.Get("refresh_token")) // rotation
			f.Refreshes++
			writeJSON(w, 200, f.issueLocked(id))
		default:
			writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
		}
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		tok := r.Form.Get("token")
		f.Revoked = append(f.Revoked, tok)
		delete(f.access, tok)
		delete(f.refresh, tok)
		w.WriteHeader(200)
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		f.mu.Lock()
		valid := strings.HasPrefix(auth, "Bearer ") && f.access[strings.TrimPrefix(auth, "Bearer ")]
		switch {
		case auth == "":
			f.MCPRequests[""]++
		case valid:
			f.MCPRequests["valid"]++
		default:
			f.MCPRequests["invalid"]++
		}
		f.mu.Unlock()
		if !valid {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.URL+`/.well-known/oauth-protected-resource/mcp", scope="read"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
	f.Server = httptest.NewServer(mux)
	return f
}

func (f *OAuth) issueLocked(clientID string) map[string]any {
	at, rt := "access-"+rand.Text(), "refresh-"+rand.Text()
	f.access[at] = true
	f.refresh[rt] = clientID
	f.Issued = append(f.Issued, at, rt)
	out := map[string]any{"access_token": at, "refresh_token": rt, "token_type": "Bearer", "scope": "read"}
	if f.ExpiresIn > 0 {
		out["expires_in"] = f.ExpiresIn
	}
	return out
}

// Endpoint is the MCP endpoint's URL.
func (f *OAuth) Endpoint() string { return f.URL + "/mcp" }

// Invalidate makes every access token issued so far invalid (the server
// forgot them), so the next request answers 401.
func (f *OAuth) Invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.access = map[string]bool{}
}

// Do runs fn under the fake's lock, to read or change its fields.
func (f *OAuth) Do(fn func(f *OAuth)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// Browser is a test's stand-in for the browser: it GETs the authorization
// URL and follows the redirect to the loopback callback.
func Browser(u string) error {
	res, err := http.Get(u)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
