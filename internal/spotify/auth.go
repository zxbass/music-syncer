package spotify

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Scopes required by music-syncer.
var Scopes = []string{
	"user-read-private",
	"user-read-email",
	"playlist-read-private",
	"playlist-read-collaborative",
	"playlist-modify-private",
	"playlist-modify-public",
	"user-library-read",
	"user-library-modify",
	"user-follow-read",
	"user-follow-modify",
	"user-read-recently-played",
}

// DefaultRedirectURI is the loopback callback registered in the Spotify app.
const DefaultRedirectURI = "http://127.0.0.1:8085/callback"

// AuthConfig controls the interactive Spotify login.
type AuthConfig struct {
	ClientID    string
	RedirectURI string
	// Manual prints the URL and asks the user to paste the callback URL
	// instead of starting a local server. Useful over SSH and in containers.
	Manual bool
	// NoBrowser suppresses opening the browser automatically.
	NoBrowser bool
	In        io.Reader
	Out       io.Writer
	// HTTPClient is used for the token exchange. Tests may override it.
	HTTPClient *http.Client
	// Timeout limits how long Login waits for the user. Default 5 minutes.
	Timeout time.Duration
}

// Login performs the OAuth 2.0 Authorization Code flow with PKCE.
func Login(ctx context.Context, cfg AuthConfig) (Token, error) {
	if strings.TrimSpace(cfg.ClientID) == "" {
		return Token{}, errors.New("spotify: client id is required; create an app at https://developer.spotify.com/dashboard")
	}
	if cfg.RedirectURI == "" {
		cfg.RedirectURI = DefaultRedirectURI
	}
	if cfg.In == nil {
		cfg.In = strings.NewReader("")
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	redirect, err := url.Parse(cfg.RedirectURI)
	if err != nil {
		return Token{}, fmt.Errorf("spotify: invalid redirect URI: %w", err)
	}

	verifier, err := randomURLSafe(64)
	if err != nil {
		return Token{}, err
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	state, err := randomURLSafe(24)
	if err != nil {
		return Token{}, err
	}

	authURL := url.URL{
		Scheme: "https",
		Host:   "accounts.spotify.com",
		Path:   "/authorize",
	}
	q := authURL.Query()
	q.Set("client_id", cfg.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", cfg.RedirectURI)
	q.Set("scope", strings.Join(Scopes, " "))
	q.Set("state", state)
	q.Set("code_challenge_method", "S256")
	q.Set("code_challenge", challenge)
	authURL.RawQuery = q.Encode()

	fmt.Fprintln(cfg.Out, "Open this URL in a browser and approve access:")
	fmt.Fprintln(cfg.Out)
	fmt.Fprintln(cfg.Out, authURL.String())
	fmt.Fprintln(cfg.Out)
	fmt.Fprintln(cfg.Out, "If Spotify reports a redirect URI mismatch, register this exact URI in the app settings:")
	fmt.Fprintln(cfg.Out, "  "+cfg.RedirectURI)
	fmt.Fprintln(cfg.Out)

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	var code string
	if cfg.Manual {
		code, err = readManualCode(cfg.In, cfg.Out, redirect.Path, state)
		if err != nil {
			return Token{}, err
		}
	} else {
		if !isLoopback(redirect.Hostname()) {
			return Token{}, fmt.Errorf("spotify: redirect URI host %q is not loopback; use --manual", redirect.Hostname())
		}
		if !cfg.NoBrowser {
			_ = openBrowser(authURL.String())
		}
		code, err = waitForCode(ctx, redirect, state, cfg.Out)
		if err != nil {
			return Token{}, err
		}
	}

	return exchangeCode(ctx, cfg.HTTPClient, cfg.ClientID, cfg.RedirectURI, verifier, code)
}

func readManualCode(in io.Reader, out io.Writer, callbackPath, state string) (string, error) {
	fmt.Fprintln(out, "After approving access, your browser will be redirected to a localhost URL that may fail to load.")
	fmt.Fprintln(out, "Paste the full redirected URL (or just the code parameter) here:")
	fmt.Fprint(out, "> ")

	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("spotify: read callback URL: %w", err)
		}
		return "", errors.New("spotify: no callback URL provided")
	}
	value := strings.TrimSpace(scanner.Text())
	if value == "" {
		return "", errors.New("spotify: empty callback URL")
	}
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil {
			return "", fmt.Errorf("spotify: parse callback URL: %w", err)
		}
		if u.Path != "" && callbackPath != "" && u.Path != callbackPath {
			return "", fmt.Errorf("spotify: callback path %q does not match redirect path %q", u.Path, callbackPath)
		}
		if e := u.Query().Get("error"); e != "" {
			return "", fmt.Errorf("spotify: authorization denied: %s %s", e, u.Query().Get("error_description"))
		}
		if got := u.Query().Get("state"); got != "" && got != state {
			return "", errors.New("spotify: state mismatch; possible CSRF, please retry")
		}
		code := u.Query().Get("code")
		if code == "" {
			return "", errors.New("spotify: callback URL has no code parameter")
		}
		return code, nil
	}
	return value, nil
}

func waitForCode(ctx context.Context, redirect *url.URL, state string, out io.Writer) (string, error) {
	handler := http.NewServeMux()
	result := make(chan struct {
		code string
		err  error
	}, 1)

	handler.HandleFunc(redirect.Path, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-result:
			http.Error(w, "already handled", http.StatusConflict)
			return
		default:
		}
		if e := r.URL.Query().Get("error"); e != "" {
			desc := r.URL.Query().Get("error_description")
			http.Error(w, "Authorization failed: "+e+" "+desc, http.StatusBadRequest)
			result <- struct {
				code string
				err  error
			}{err: fmt.Errorf("spotify: authorization denied: %s %s", e, desc)}
			return
		}
		if got := r.URL.Query().Get("state"); got != state {
			http.Error(w, "State mismatch", http.StatusBadRequest)
			result <- struct {
				code string
				err  error
			}{err: errors.New("spotify: state mismatch; possible CSRF, please retry")}
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "Missing code", http.StatusBadRequest)
			result <- struct {
				code string
				err  error
			}{err: errors.New("spotify: callback has no code")}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, successPage)
		result <- struct {
			code string
			err  error
		}{code: code}
	})

	addr := redirect.Host
	if !strings.Contains(addr, ":") {
		addr += ":80"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("spotify: listen on %s: %w (use --manual if the port is busy)", addr, err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case result <- struct {
				code string
				err  error
			}{err: err}:
			default:
			}
		}
	}()

	fmt.Fprintln(out, "Waiting for the browser callback... (Ctrl+C to cancel)")
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("spotify: login timed out: %w", ctx.Err())
	case res := <-result:
		if res.err != nil {
			return "", res.err
		}
		return res.code, nil
	}
}

func exchangeCode(ctx context.Context, hc *http.Client, clientID, redirectURI, verifier, code string) (Token, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://accounts.spotify.com/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("spotify: token exchange: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return Token{}, tokenError(resp.StatusCode, data)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return Token{}, fmt.Errorf("spotify: decode token response: %w", err)
	}
	if payload.AccessToken == "" {
		return Token{}, errors.New("spotify: token response has no access_token")
	}
	return Token{
		AccessToken:  payload.AccessToken,
		TokenType:    payload.TokenType,
		RefreshToken: payload.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}, nil
}

func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func openBrowser(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

const successPage = `<!doctype html><html><head><meta charset="utf-8"><title>music-syncer</title>
<style>body{font-family:system-ui,sans-serif;background:#111;color:#eee;display:flex;min-height:100vh;align-items:center;justify-content:center}main{text-align:center;max-width:32rem;padding:2rem}code{background:#222;padding:.2rem .4rem;border-radius:.3rem}</style>
</head><body><main><h1>Spotify is connected</h1>
<p>You can close this tab and return to the terminal.</p></main></body></html>`
