package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// serverAuth says how to sign in to a server.
type serverAuth struct {
	Kind     string   `json:"kind"` // "cookie", "oidc", "apikey" or "" for none
	Issuer   string   `json:"issuer,omitempty"`
	ClientID string   `json:"clientId,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
}

type serverConfig struct {
	URL  string     `json:"url"`
	Auth serverAuth `json:"auth"`
}

// config is ~/.config/scooter/config.json.
type config struct {
	Default string                  `json:"default,omitempty"`
	Servers map[string]serverConfig `json:"servers"`
}

// credential is one server's stored login.
type credential struct {
	Cookie       string `json:"cookie,omitempty"`
	APIKey       string `json:"apiKey,omitempty"`
	IDToken      string `json:"idToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
}

func configDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "scooter")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "scooter")
}

func readJSONFile(name string, v any) error {
	b, err := os.ReadFile(filepath.Join(configDir(), name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// writeJSONFile replaces the file atomically; both files stay 0600.
func writeJSONFile(name string, v any) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone already after the rename
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

func loadConfig() (config, error) {
	c := config{Servers: map[string]serverConfig{}}
	err := readJSONFile("config.json", &c)
	if c.Servers == nil {
		c.Servers = map[string]serverConfig{}
	}
	return c, err
}

func loadCredentials() (map[string]credential, error) {
	m := map[string]credential{}
	err := readJSONFile("credentials.json", &m)
	return m, err
}

func saveCredential(name string, cred credential) error {
	m, err := loadCredentials()
	if err != nil {
		return err
	}
	m[name] = cred
	return writeJSONFile("credentials.json", m)
}

// serverNames lists the configured servers, sorted.
func (c config) serverNames() []string {
	names := make([]string, 0, len(c.Servers))
	for n := range c.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// resolve picks the server: flag, $SCOOTER_SERVER, then default.
func (c config) resolve(flagValue string) (string, serverConfig, error) {
	want := flagValue
	if want == "" {
		want = os.Getenv("SCOOTER_SERVER")
	}
	if want == "" {
		want = c.Default
	}
	if want == "" && len(c.Servers) == 1 {
		want = c.serverNames()[0]
	}
	if want == "" {
		return "", serverConfig{}, errors.New("no server: pass --server, set SCOOTER_SERVER, or run scooter login")
	}
	if s, ok := c.Servers[want]; ok {
		return want, s, nil
	}
	if strings.Contains(want, "://") {
		for _, n := range c.serverNames() {
			if strings.TrimRight(c.Servers[n].URL, "/") == strings.TrimRight(want, "/") {
				return n, c.Servers[n], nil
			}
		}
		// An unconfigured URL is a server without auth.
		return want, serverConfig{URL: want}, nil
	}
	return "", serverConfig{}, fmt.Errorf("unknown server %q; run scooter login --server %s --url URL", want, want)
}

// apiKeyPath is where Scooter serves API-key callers.
const apiKeyPath = "/webhooks/agent-api/v1"

// base is the URL a server's commands go to.
func (s serverConfig) base() string {
	if s.Auth.Kind == "apikey" {
		return s.URL + apiKeyPath
	}
	return s.URL
}

// loginHeaders returns a header source backed by the stored login.
func loginHeaders(name string, s serverConfig) headerSource {
	return headerSource{fn: func(ctx context.Context, retry bool) (map[string]string, error) {
		return storedHeaders(ctx, name, s, retry)
	}}
}

func storedHeaders(ctx context.Context, name string, s serverConfig, retry bool) (map[string]string, error) {
	if s.Auth.Kind == "" {
		return map[string]string{}, nil
	}
	creds, err := loadCredentials()
	if err != nil {
		return nil, err
	}
	cred := creds[name]
	relogin := fmt.Errorf("%w: run scooter login --server %s", errAuthWall, name)
	switch s.Auth.Kind {
	case "cookie":
		if cred.Cookie == "" || retry {
			return nil, relogin
		}
		return map[string]string{"Cookie": "_oauth2_proxy=" + cred.Cookie}, nil
	case "apikey":
		if cred.APIKey == "" || retry {
			return nil, relogin
		}
		return map[string]string{"Authorization": "Bearer " + cred.APIKey}, nil
	case "oidc":
		if cred.IDToken == "" || retry || jwtExpiry(cred.IDToken).Before(time.Now().Add(time.Minute)) {
			if cred.RefreshToken == "" {
				return nil, relogin
			}
			cred, err = refreshIDToken(ctx, s.Auth, cred.RefreshToken)
			if err != nil {
				return nil, fmt.Errorf("%w (refresh failed: %v)", relogin, err)
			}
			if err := saveCredential(name, cred); err != nil {
				return nil, err
			}
		}
		return map[string]string{"Authorization": "Bearer " + cred.IDToken}, nil
	}
	return nil, fmt.Errorf("server %s: unknown auth kind %q", name, s.Auth.Kind)
}

// jwtExpiry reads a JWT's exp claim, or zero.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &claims) != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}
