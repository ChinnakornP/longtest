package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// RedeemPath is the unauthenticated endpoint a fresh daemon posts its pairing
// code to. It is unauthenticated by necessity: the code is the only credential
// the daemon holds at that point.
const RedeemPath = "/api/v1/runtimes/redeem"

// PairInput is what `qa-daemon pair` was given.
type PairInput struct {
	ServerURL   string
	Code        string
	RuntimeName string
	// HTTPClient overrides the client, for tests and for proxied networks.
	HTTPClient *http.Client
}

// Pair exchanges a one-time pairing code for a runtime identity and token.
//
// The organization is never sent: it comes from the pairing code server-side
// and is only reported back. A daemon cannot choose which tenant it joins.
func Pair(ctx context.Context, in PairInput) (Config, error) {
	code := strings.TrimSpace(in.Code)
	if code == "" {
		return Config{}, fmt.Errorf("runtime: pairing code is required")
	}
	name := strings.TrimSpace(in.RuntimeName)
	if name == "" {
		name = defaultRuntimeName()
	}

	cfg := Config{ServerURL: strings.TrimSpace(in.ServerURL)}
	endpoint, err := cfg.APIURL(RedeemPath)
	if err != nil {
		return Config{}, err
	}

	body, err := json.Marshal(map[string]any{
		"pairingCode": code,
		"runtimeName": name,
		"hostInfo": map[string]string{
			"hostname": hostname(),
			"os":       runtime.GOOS,
			"arch":     runtime.GOARCH,
			"version":  Version,
		},
	})
	if err != nil {
		return Config{}, fmt.Errorf("runtime: encode pairing request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Config{}, fmt.Errorf("runtime: build pairing request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := in.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Config{}, fmt.Errorf("runtime: reach %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return Config{}, fmt.Errorf("runtime: read pairing response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Config{}, fmt.Errorf("runtime: pairing failed: %s", apiErrorMessage(resp.StatusCode, payload))
	}

	var redeemed struct {
		RuntimeID    string `json:"runtimeId"`
		RuntimeToken string `json:"runtimeToken"`
		RuntimeName  string `json:"runtimeName"`
		OrgID        string `json:"orgId"`
	}
	if err := json.Unmarshal(payload, &redeemed); err != nil {
		return Config{}, fmt.Errorf("runtime: parse pairing response: %w", err)
	}
	if redeemed.RuntimeID == "" || redeemed.RuntimeToken == "" {
		return Config{}, fmt.Errorf("runtime: pairing response is missing the runtime id or token")
	}

	cfg.RuntimeID = redeemed.RuntimeID
	cfg.RuntimeName = redeemed.RuntimeName
	cfg.OrgID = redeemed.OrgID
	cfg.Token = redeemed.RuntimeToken
	return cfg, nil
}

// apiErrorMessage renders the backend's error envelope. The envelope's message
// is written for a human, so it is shown; the raw body is only used when the
// response is not one (a proxy error page, say), and is truncated because it
// is not ours.
func apiErrorMessage(status int, body []byte) string {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		return fmt.Sprintf("%d %s: %s", status, envelope.Error.Code, envelope.Error.Message)
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	if text == "" {
		return fmt.Sprintf("%d %s", status, http.StatusText(status))
	}
	return fmt.Sprintf("%d: %s", status, text)
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

func defaultRuntimeName() string {
	if name := hostname(); name != "" {
		return name
	}
	return "qa-runtime"
}
