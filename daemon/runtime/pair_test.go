package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPairExchangesTheCodeForAToken(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != RedeemPath {
			t.Errorf("path = %q, want %q", r.URL.Path, RedeemPath)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"runtimeId":"9f6d1d1c-8b0a-4c3d-9e2f-1a2b3c4d5e6f",
			"runtimeToken":"qart_secret_value",
			"runtimeName":"macbook-pro",
			"orgId":"3a2b1c0d-4e5f-6a7b-8c9d-0e1f2a3b4c5d"
		}`))
	}))
	defer srv.Close()

	cfg, err := Pair(t.Context(), PairInput{ServerURL: srv.URL, Code: "k7q2-9fmr-3xt8", RuntimeName: "macbook-pro"})
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}

	if cfg.RuntimeID != "9f6d1d1c-8b0a-4c3d-9e2f-1a2b3c4d5e6f" || cfg.Token != "qart_secret_value" {
		t.Fatalf("config = %+v", cfg.LogValue())
	}
	if cfg.OrgID != "3a2b1c0d-4e5f-6a7b-8c9d-0e1f2a3b4c5d" {
		t.Fatalf("orgId = %q; the organization comes from the code, and the daemon records what it was told", cfg.OrgID)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a freshly paired config should be usable: %v", err)
	}

	// The daemon never asserts which organization it belongs to.
	if _, ok := got["orgId"]; ok {
		t.Fatalf("the pairing request claimed an organization: %v", got)
	}
	if got["pairingCode"] != "k7q2-9fmr-3xt8" {
		t.Fatalf("pairingCode = %v", got["pairingCode"])
	}
	hostInfo, ok := got["hostInfo"].(map[string]any)
	if !ok || hostInfo["os"] == "" || hostInfo["version"] != Version {
		t.Fatalf("hostInfo = %v", got["hostInfo"])
	}
}

func TestPairSurfacesTheBackendsMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"that pairing code is not valid, has expired, or has already been used"}}`))
	}))
	defer srv.Close()

	_, err := Pair(t.Context(), PairInput{ServerURL: srv.URL, Code: "k7q2-9fmr-3xt8"})
	if err == nil {
		t.Fatal("expected an error")
	}
	// The backend writes that message for a human; repeating it beats
	// inventing a worse one.
	if !strings.Contains(err.Error(), "already been used") {
		t.Fatalf("error = %v", err)
	}
}

func TestPairRejectsAnUnusableResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no token", `{"runtimeId":"9f6d1d1c-8b0a-4c3d-9e2f-1a2b3c4d5e6f"}`},
		{"no id", `{"runtimeToken":"qart_x"}`},
		{"not json", `<html>proxy error</html>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			if _, err := Pair(t.Context(), PairInput{ServerURL: srv.URL, Code: "k7q2-9fmr-3xt8"}); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestPairValidatesItsInput(t *testing.T) {
	if _, err := Pair(t.Context(), PairInput{ServerURL: "https://qa.test"}); err == nil {
		t.Fatal("a pairing without a code must fail")
	}
	if _, err := Pair(t.Context(), PairInput{ServerURL: "ws://qa.test", Code: "abcd-efgh-ijkl"}); err == nil {
		t.Fatal("pairing is plain HTTP; a websocket URL must be rejected")
	}
}
