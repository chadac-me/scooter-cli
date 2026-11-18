package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeJWT is an unsigned token; only exp is read.
func fakeJWT(exp time.Time) string {
	claims, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

// fakeIdP is an OIDC provider that approves every login.
type fakeIdP struct {
	mu       sync.Mutex
	srv      *httptest.Server
	refreshs int
}

func newIdP(t *testing.T) *fakeIdP {
	idp := &fakeIdP{}
	var challenge string
	idp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"authorization_endpoint": idp.srv.URL + "/authorize", "token_endpoint": idp.srv.URL + "/token",
			})
		case "/authorize":
			q := r.URL.Query()
			idp.mu.Lock()
			challenge = q.Get("code_challenge")
			idp.mu.Unlock()
			http.Redirect(w, r, q.Get("redirect_uri")+"?code=c0de&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
		case "/token":
			_ = r.ParseForm()
			idp.mu.Lock()
			defer idp.mu.Unlock()
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				if r.Form.Get("code") != "c0de" || pkceChallenge(r.Form.Get("code_verifier")) != challenge {
					http.Error(w, `{"error":"invalid_grant"}`, 400)
					return
				}
			case "refresh_token":
				if r.Form.Get("refresh_token") != "r1" {
					http.Error(w, `{"error":"invalid_grant"}`, 400)
					return
				}
				idp.refreshs++
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id_token": fakeJWT(time.Now().Add(time.Hour)), "refresh_token": "r1"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.srv.Close)
	return idp
}

// scooterRequiring serves /whoami only when header k starts with v.
func scooterRequiring(t *testing.T, k, v string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get(k), v) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chad"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCookieLoginStoresPrivately(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := scooterRequiring(t, "Cookie", "_oauth2_proxy=abc")
	out, err := login(context.Background(), loginOptions{
		name: "home", url: srv.URL + "/", auth: serverAuth{Kind: "cookie"},
		secretInput: strings.NewReader("_oauth2_proxy=abc\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(out["user"].(json.RawMessage)) != `{"id":"chad"}` {
		t.Fatalf("got %v", out)
	}
	cfg, _ := loadConfig()
	if cfg.Default != "home" || cfg.Servers["home"].URL != srv.URL {
		t.Fatalf("config %+v", cfg)
	}
	st, err := os.Stat(filepath.Join(configDir(), "credentials.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials: %v %v", st, err)
	}
	// Later commands use the default server and its cookie.
	_, s, err := cfg.resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if err := newClient(s.URL, loginHeaders("home", s)).call(context.Background(), "GET", "/whoami", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRefusedCookieAsksForLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := scooterRequiring(t, "Cookie", "_oauth2_proxy=new")
	s := serverConfig{URL: srv.URL, Auth: serverAuth{Kind: "cookie"}}
	if err := saveCredential("home", credential{Cookie: "stale"}); err != nil {
		t.Fatal(err)
	}
	err := newClient(s.URL, loginHeaders("home", s)).call(context.Background(), "GET", "/whoami", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "scooter login --server home") {
		t.Fatalf("got %v", err)
	}
}

func TestOIDCLoginWithPKCE(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	idp := newIdP(t)
	srv := scooterRequiring(t, "Authorization", "Bearer e30.")
	old := browse
	t.Cleanup(func() { browse = old })
	browse = func(u string) {
		go func() {
			resp, err := http.Get(u) // follows the redirect to the loopback callback
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	_, err := login(context.Background(), loginOptions{
		name: "work", url: srv.URL, timeout: 5 * time.Second,
		auth: serverAuth{Kind: "oidc", Issuer: idp.srv.URL, ClientID: "scooter"},
	})
	if err != nil {
		t.Fatal(err)
	}
	creds, _ := loadCredentials()
	if creds["work"].RefreshToken != "r1" || jwtExpiry(creds["work"].IDToken).Before(time.Now()) {
		t.Fatalf("credentials %+v", creds["work"])
	}
}

func TestExpiredIDTokenIsRefreshed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	idp := newIdP(t)
	s := serverConfig{URL: "http://unused", Auth: serverAuth{Kind: "oidc", Issuer: idp.srv.URL, ClientID: "scooter"}}
	if err := saveCredential("work", credential{IDToken: fakeJWT(time.Now().Add(-time.Minute)), RefreshToken: "r1"}); err != nil {
		t.Fatal(err)
	}
	h, err := storedHeaders(context.Background(), "work", s, false)
	if err != nil {
		t.Fatal(err)
	}
	if idp.refreshs != 1 || !strings.HasPrefix(h["Authorization"], "Bearer e30.") {
		t.Fatalf("refreshes %d, headers %v", idp.refreshs, h)
	}
	// The fresh token was saved, so no second refresh.
	if _, err := storedHeaders(context.Background(), "work", s, false); err != nil || idp.refreshs != 1 {
		t.Fatalf("refreshes %d, %v", idp.refreshs, err)
	}
}

func TestResolveServer(t *testing.T) {
	t.Setenv("SCOOTER_SERVER", "")
	cfg := config{Default: "home", Servers: map[string]serverConfig{
		"home": {URL: "https://home.example"}, "work": {URL: "https://work.example"},
	}}
	for in, want := range map[string]string{"": "home", "work": "work", "https://work.example/": "work", "https://other.example": "https://other.example"} {
		if name, _, err := cfg.resolve(in); err != nil || name != want {
			t.Errorf("resolve(%q) = %q, %v; want %q", in, name, err, want)
		}
	}
	if _, _, err := cfg.resolve("nope"); err == nil {
		t.Error("an unknown name resolved")
	}
}

func TestAPIKeyLoginUsesTheAPIKeyEndpoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer sct_k" || r.Header.Get("Cookie") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"chad"}`)
	}))
	t.Cleanup(srv.Close)
	if _, err := login(context.Background(), loginOptions{
		name: "home", url: srv.URL, auth: serverAuth{Kind: "apikey"},
		secretInput: strings.NewReader("sct_k\n"),
	}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := loadConfig()
	_, s, _ := cfg.resolve("")
	if err := newClient(s.base(), loginHeaders("home", s)).call(context.Background(), "GET", "/conversations", nil, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{apiKeyPath + "/whoami", apiKeyPath + "/conversations"}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
}

func TestAPIKeyLoginRefusesAnythingElse(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := login(context.Background(), loginOptions{
		name: "home", url: "http://unused", auth: serverAuth{Kind: "apikey"},
		secretInput: strings.NewReader("_oauth2_proxy=abc\n"),
	})
	if err == nil || !strings.Contains(err.Error(), "sct_") {
		t.Fatalf("got %v", err)
	}
}

func TestRefusedAPIKeyAsksForLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := scooterRequiring(t, "Authorization", "Bearer sct_new")
	s := serverConfig{URL: srv.URL, Auth: serverAuth{Kind: "apikey"}}
	if err := saveCredential("home", credential{APIKey: "sct_revoked"}); err != nil {
		t.Fatal(err)
	}
	err := newClient(s.URL, loginHeaders("home", s)).call(context.Background(), "GET", "/whoami", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "scooter login --server home") {
		t.Fatalf("got %v", err)
	}
}
