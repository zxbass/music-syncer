package yandex

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	oauthBaseURL = "https://oauth.yandex.ru"
	// Credentials of the official Android app. They are public and are used
	// by the Yandex Music API community for device-flow login. This is not an
	// officially supported integration.
	deviceClientID     = "23cabbbdc6cd418abb4b39c32c41195d"
	deviceClientSecret = "REDACTED"
	deviceName         = "music-syncer"
)

// DeviceAuthConfig controls the interactive Yandex device login.
type DeviceAuthConfig struct {
	ClientID     string
	ClientSecret string
	DeviceName   string
	In           io.Reader
	Out          io.Writer
	HTTPClient   *http.Client
	// PollInterval overrides the server-provided polling interval. It is
	// mainly useful in tests.
	PollInterval time.Duration
	// BaseURL overrides the OAuth endpoint (used by tests).
	BaseURL string
}

// LoginDevice performs the OAuth device flow and returns a Yandex Music token.
func LoginDevice(ctx context.Context, cfg DeviceAuthConfig) (string, error) {
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = oauthBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.ClientID == "" {
		cfg.ClientID = deviceClientID
	}
	if cfg.ClientSecret == "" {
		cfg.ClientSecret = deviceClientSecret
	}
	if cfg.DeviceName == "" {
		cfg.DeviceName = deviceName
	}

	code, err := requestDeviceCode(ctx, cfg)
	if err != nil {
		return "", err
	}
	verificationURL := code.VerificationURL
	if verificationURL == "" {
		verificationURL = code.VerificationURI
	}
	if verificationURL == "" {
		verificationURL = "https://ya.ru/device"
	}
	fmt.Fprintln(cfg.Out, "Open this URL on any device:")
	fmt.Fprintln(cfg.Out, "  "+verificationURL)
	fmt.Fprintln(cfg.Out)
	fmt.Fprintln(cfg.Out, "Enter the code:")
	fmt.Fprintln(cfg.Out, "  "+code.UserCode)
	fmt.Fprintln(cfg.Out)
	fmt.Fprintln(cfg.Out, "Waiting for confirmation... (Ctrl+C to cancel)")

	interval := cfg.PollInterval
	if interval <= 0 {
		interval = time.Duration(code.Interval) * time.Second
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(code.ExpiresIn) * time.Second)
	if code.ExpiresIn <= 0 {
		deadline = time.Now().Add(5 * time.Minute)
	}

	for {
		if err := sleepCtx(ctx, interval); err != nil {
			return "", err
		}
		token, pending, slowDown, err := pollDeviceToken(ctx, cfg, code.DeviceCode)
		if err != nil {
			return "", err
		}
		if token != "" {
			fmt.Fprintln(cfg.Out, "Authorized.")
			return token, nil
		}
		if slowDown {
			interval += 5 * time.Second
		}
		if pending {
			if time.Now().After(deadline) {
				return "", errors.New("yandex: device code expired; run `music-syncer auth yandex` again")
			}
			continue
		}
		return "", errors.New("yandex: unexpected device authorization state")
	}
}

type deviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func requestDeviceCode(ctx context.Context, cfg DeviceAuthConfig) (deviceCode, error) {
	deviceID, err := randomDeviceID(10)
	if err != nil {
		return deviceCode{}, err
	}
	form := url.Values{
		"client_id":   {cfg.ClientID},
		"device_id":   {deviceID},
		"device_name": {cfg.DeviceName},
	}
	var code deviceCode
	if err := postForm(ctx, cfg.HTTPClient, cfg.BaseURL+"/device/code", form, &code); err != nil {
		return deviceCode{}, err
	}
	if code.DeviceCode == "" || code.UserCode == "" {
		return deviceCode{}, errors.New("yandex: device code response is incomplete")
	}
	return code, nil
}

func pollDeviceToken(ctx context.Context, cfg DeviceAuthConfig, deviceCodeValue string) (token string, pending, slowDown bool, err error) {
	form := url.Values{
		"grant_type":    {"device_code"},
		"code":          {deviceCodeValue},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", false, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return "", false, false, fmt.Errorf("yandex token poll: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode/100 == 2 {
		var payload struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			return "", false, false, fmt.Errorf("yandex: decode token response: %w", err)
		}
		if payload.AccessToken == "" {
			return "", false, false, errors.New("yandex: token response has no access_token")
		}
		return payload.AccessToken, false, false, nil
	}

	var oauthErr struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(data, &oauthErr)
	code := oauthErr.Error
	if code == "" {
		code = strings.TrimSpace(string(data))
	}
	switch {
	case strings.Contains(code, "authorization_pending"):
		return "", true, false, nil
	case strings.Contains(code, "slow_down"):
		return "", true, true, nil
	case strings.Contains(code, "expired_token"):
		return "", false, false, errors.New("yandex: device code expired; run `music-syncer auth yandex` again")
	case strings.Contains(code, "access_denied"):
		return "", false, false, errors.New("yandex: access denied by the user")
	default:
		msg := oauthErr.ErrorDescription
		if msg == "" {
			msg = code
		}
		return "", false, false, fmt.Errorf("yandex: device authorization failed: HTTP %d: %s", resp.StatusCode, msg)
	}
}

func postForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		message := strings.TrimSpace(string(data))
		var payload struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if json.Unmarshal(data, &payload) == nil && payload.Error != "" {
			message = payload.Error
			if payload.ErrorDescription != "" {
				message += ": " + payload.ErrorDescription
			}
		}
		return fmt.Errorf("yandex oauth: HTTP %d: %s", resp.StatusCode, message)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("yandex oauth: decode response: %w", err)
	}
	return nil
}

func randomDeviceID(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, n)
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(buf), nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
