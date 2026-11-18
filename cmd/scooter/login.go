package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type loginOptions struct {
	name, url    string
	auth         serverAuth
	makeDefault  bool
	noBrowser    bool
	timeout      time.Duration
	secretInput  io.Reader
	secretPrompt bool
}

func loginMain(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: scooter login [--server NAME] [--url URL --api-key | --url URL --cookie | --url URL --issuer URL --client-id ID]

Signs in and stores the login under ~/.config/scooter. A new server needs
--url and an auth kind: --api-key pastes a key from Settings → API keys;
--cookie pastes oauth2-proxy's _oauth2_proxy cookie (either is read from
stdin when it isn't a terminal); --issuer and --client-id log in with OIDC
(PKCE) in a browser.`)
		fs.PrintDefaults()
	}
	server := fs.String("server", "", "server name (default: the default server)")
	rawURL := fs.String("url", "", "server base URL, to add or change a server")
	apiKey := fs.Bool("api-key", false, "sign in with a pasted API key")
	cookie := fs.Bool("cookie", false, "sign in with a pasted _oauth2_proxy cookie")
	issuer := fs.String("issuer", "", "OIDC issuer URL")
	clientID := fs.String("client-id", "", "OIDC client id")
	scopes := fs.String("scopes", "", "OIDC scopes, space-separated (default: openid email profile offline_access)")
	makeDefault := fs.Bool("default", false, "make this the default server")
	noBrowser := fs.Bool("no-browser", false, "print the login URL instead of opening a browser")
	_ = fs.Parse(args) // ExitOnError: it exits rather than returning one.

	opts := loginOptions{
		name: *server, url: *rawURL, makeDefault: *makeDefault, noBrowser: *noBrowser,
		timeout: 5 * time.Minute, secretInput: os.Stdin, secretPrompt: isTerminal(os.Stdin),
	}
	switch {
	case *apiKey:
		opts.auth = serverAuth{Kind: "apikey"}
	case *cookie:
		opts.auth = serverAuth{Kind: "cookie"}
	case *issuer != "" || *clientID != "":
		opts.auth = serverAuth{Kind: "oidc", Issuer: *issuer, ClientID: *clientID, Scopes: strings.Fields(*scopes)}
	}
	ctx, stop := commandContext()
	defer stop()
	out, err := login(ctx, opts)
	if err != nil {
		fatal(err)
	}
	printJSON(out)
}

// login saves the server, signs in, and checks the login.
func login(ctx context.Context, o loginOptions) (map[string]any, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	name := o.name
	if name == "" {
		if name, _, err = cfg.resolve(""); err != nil {
			return nil, errors.New("pass --server NAME (and --url, for a new server)")
		}
	}
	s, known := cfg.Servers[name]
	if o.url != "" {
		s.URL = strings.TrimRight(o.url, "/")
	}
	if o.auth.Kind != "" {
		s.Auth = o.auth
	}
	switch {
	case s.URL == "":
		return nil, fmt.Errorf("server %s is new: pass --url", name)
	case s.Auth.Kind == "":
		return nil, fmt.Errorf("server %s: pass --api-key, --cookie, or --issuer and --client-id", name)
	case s.Auth.Kind == "oidc" && (s.Auth.Issuer == "" || s.Auth.ClientID == ""):
		return nil, errors.New("OIDC needs both --issuer and --client-id")
	}

	var cred credential
	switch s.Auth.Kind {
	case "apikey":
		cred, err = readAPIKey(o, s.URL)
	case "cookie":
		cred, err = readCookie(o, s.URL)
	default:
		cred, err = oidcLogin(ctx, s.Auth, o)
	}
	if err != nil {
		return nil, err
	}

	// Save the server only once a login worked.
	if !known || o.url != "" || o.auth.Kind != "" || o.makeDefault || cfg.Default == "" {
		cfg.Servers[name] = s
		if o.makeDefault || cfg.Default == "" {
			cfg.Default = name
		}
		if err := writeJSONFile("config.json", cfg); err != nil {
			return nil, err
		}
	}
	if err := saveCredential(name, cred); err != nil {
		return nil, err
	}
	var who json.RawMessage
	if err := newClient(s.base(), loginHeaders(name, s)).call(ctx, "GET", "/whoami", nil, &who); err != nil {
		return nil, fmt.Errorf("signed in, but the server refused the login: %w", err)
	}
	return map[string]any{"server": name, "url": s.URL, "user": who}, nil
}

func readCookie(o loginOptions, base string) (credential, error) {
	v, err := readSecret(o, fmt.Sprintf("Sign in at %s/oauth2/sign_in, then copy the _oauth2_proxy cookie from the browser.\nCookie value: ", base))
	v = strings.TrimPrefix(v, "_oauth2_proxy=")
	if err == nil && v == "" {
		err = errors.New("no cookie given")
	}
	return credential{Cookie: v}, err
}

func readAPIKey(o loginOptions, base string) (credential, error) {
	v, err := readSecret(o, fmt.Sprintf("Create a key at %s/settings/apikeys, then paste it.\nAPI key: ", base))
	if err == nil && !strings.HasPrefix(v, "sct_") {
		err = errors.New("an API key starts with sct_")
	}
	return credential{APIKey: v}, err
}

// readSecret reads one line, prompting without echo on a terminal.
func readSecret(o loginOptions, prompt string) (string, error) {
	if o.secretPrompt {
		fmt.Fprint(os.Stderr, prompt)
		restore := noEcho()
		defer func() { restore(); fmt.Fprintln(os.Stderr) }()
	}
	line, err := bufio.NewReader(o.secretInput).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

type oidcEndpoints struct {
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
}

func discover(ctx context.Context, issuer string) (oidcEndpoints, error) {
	var d oidcEndpoints
	err := getJSON(ctx, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", &d)
	if err == nil && (d.Authorization == "" || d.Token == "") {
		err = errors.New("discovery document lacks endpoints")
	}
	if err != nil {
		return d, fmt.Errorf("OIDC discovery for %s: %w", issuer, err)
	}
	return d, nil
}

func getJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func tokenRequest(ctx context.Context, endpoint string, form url.Values) (credential, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return credential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return credential{}, err
	}
	defer resp.Body.Close()
	var r struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r)
	switch {
	case resp.StatusCode != http.StatusOK:
		return credential{}, fmt.Errorf("token request: %s %s %s", resp.Status, r.Error, r.Description)
	case r.IDToken == "":
		return credential{}, errors.New("token request: the IdP returned no id_token")
	}
	return credential{IDToken: r.IDToken, RefreshToken: r.RefreshToken}, nil
}

// refreshIDToken gets a new ID token, keeping the refresh token.
func refreshIDToken(ctx context.Context, a serverAuth, refresh string) (credential, error) {
	d, err := discover(ctx, a.Issuer)
	if err != nil {
		return credential{}, err
	}
	cred, err := tokenRequest(ctx, d.Token, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {a.ClientID},
	})
	if cred.RefreshToken == "" {
		cred.RefreshToken = refresh
	}
	return cred, err
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms.
	return base64.RawURLEncoding.EncodeToString(b)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// oidcLogin runs Authorization Code + PKCE with a loopback redirect.
func oidcLogin(ctx context.Context, a serverAuth, o loginOptions) (credential, error) {
	d, err := discover(ctx, a.Issuer)
	if err != nil {
		return credential{}, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return credential{}, err
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	verifier, state := randomString(48), randomString(16)
	scopes := a.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile", "offline_access"}
	}
	authURL := d.Authorization + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {a.ClientID},
		"redirect_uri":          {redirect},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}.Encode()

	type result struct{ code, err string }
	done := make(chan result, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		res := result{code: q.Get("code"), err: q.Get("error")}
		if q.Get("state") != state {
			res = result{err: "state mismatch"}
		} else if res.code == "" && res.err == "" {
			res.err = "no code in the callback"
		}
		_, _ = fmt.Fprintln(w, "Scooter: you can close this tab.")
		select {
		case done <- res:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }() // also closes ln

	fmt.Fprintf(os.Stderr, "Finish signing in in your browser:\n  %s\n", authURL)
	if !o.noBrowser {
		browse(authURL)
	}
	var res result
	select {
	case res = <-done:
	case <-time.After(o.timeout):
		return credential{}, errors.New("login timed out")
	case <-ctx.Done():
		return credential{}, ctx.Err()
	}
	if res.err != "" {
		return credential{}, fmt.Errorf("login failed: %s", res.err)
	}
	return tokenRequest(ctx, d.Token, url.Values{
		"grant_type": {"authorization_code"}, "code": {res.code}, "redirect_uri": {redirect},
		"code_verifier": {verifier}, "client_id": {a.ClientID},
	})
}

// browse opens a URL in a browser; tests replace it.
var browse = openBrowser

func openBrowser(u string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	_ = exec.Command(cmd, u).Start() // The URL is printed too, so a failure is fine.
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// noEcho hides typed input; call the result to restore.
func noEcho() func() {
	run := func(arg string) error {
		cmd := exec.Command("stty", arg)
		cmd.Stdin = os.Stdin
		return cmd.Run()
	}
	if run("-echo") != nil {
		return func() {}
	}
	return func() { _ = run("echo") }
}
