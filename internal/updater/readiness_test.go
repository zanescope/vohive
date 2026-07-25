package updater

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveProbeURLUsesConfiguredPort(t *testing.T) {
	t.Setenv("VOHIVE_SERVER_PORT", "")
	t.Setenv("PROXY_SERVER_PORT", "")

	tests := []struct {
		name    string
		port    string
		want    string
		wantErr bool
	}{
		{name: "integer", port: "9012", want: "http://127.0.0.1:9012/readyz"},
		{name: "listen address", port: `":9013"`, want: "http://127.0.0.1:9013/readyz"},
		{name: "wildcard ipv4", port: `"0.0.0.0:9014"`, want: "http://127.0.0.1:9014/readyz"},
		{name: "remote only", port: `"192.0.2.10:9015"`, wantErr: true},
		{name: "invalid", port: `"not-a-port"`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte("server:\n  port: "+tt.port+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ResolveProbeURL(configPath, "/readyz")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveProbeURL() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveProbeURL() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ResolveProbeURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveProbeURLUsesRuntimeEnvironmentOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  port: 7575\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOHIVE_SERVER_PORT", "9123")
	t.Setenv("PROXY_SERVER_PORT", "9124")

	got, err := ResolveProbeURL(configPath, "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:9123/healthz" {
		t.Fatalf("ResolveProbeURL() = %q", got)
	}
}

func TestReadinessProofRoundTrip(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "readiness.key")
	if err := RotateReadinessKey(keyFile); err != nil {
		t.Fatal(err)
	}
	challenge, err := NewReadinessChallenge()
	if err != nil {
		t.Fatal(err)
	}
	version, proof, err := SignReadinessChallenge(keyFile, "1.2.3", challenge)
	if err != nil {
		t.Fatal(err)
	}
	if version != "v1.2.3" {
		t.Fatalf("version = %q", version)
	}
	if err := VerifyReadinessProof(keyFile, version, challenge, proof); err != nil {
		t.Fatalf("VerifyReadinessProof() error = %v", err)
	}
	if err := VerifyReadinessProof(keyFile, version, challenge, proof+"00"); err == nil {
		t.Fatal("VerifyReadinessProof() accepted a modified proof")
	}
}

func TestHTTPReadyCheckerRequiresManagedVersionAndProof(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "readiness.key")
	if err := RotateReadinessKey(keyFile); err != nil {
		t.Fatal(err)
	}

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version, proof, err := SignReadinessChallenge(keyFile, "v1.2.3", r.Header.Get(ReadinessChallengeHeader))
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(ReadinessResponse{Status: "ready", Version: version, Proof: proof})
	}))
	defer good.Close()

	expectation := ReadyExpectation{
		Endpoint:        good.URL,
		ExpectedVersion: "v1.2.3",
		KeyFile:         keyFile,
	}
	checker := HTTPReadyChecker{Client: good.Client()}
	if err := checker.Ready(context.Background(), expectation); err != nil {
		t.Fatalf("Ready() error = %v", err)
	}

	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "unrelated 2xx",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"ready"}`))
			},
		},
		{
			name: "wrong version",
			handler: func(w http.ResponseWriter, r *http.Request) {
				version, proof, _ := SignReadinessChallenge(keyFile, "v1.2.4", r.Header.Get(ReadinessChallengeHeader))
				_ = json.NewEncoder(w).Encode(ReadinessResponse{Status: "ready", Version: version, Proof: proof})
			},
		},
		{
			name: "forged proof",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(ReadinessResponse{Status: "ready", Version: "v1.2.3", Proof: "00"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()
			expectation.Endpoint = server.URL
			if err := (HTTPReadyChecker{Client: server.Client()}).Ready(context.Background(), expectation); err == nil {
				t.Fatal("Ready() accepted an unverified endpoint")
			}
		})
	}
}

func TestHTTPReadyCheckerAllowsExplicitLegacyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}))
	defer server.Close()

	err := (HTTPReadyChecker{Client: server.Client()}).Ready(context.Background(), ReadyExpectation{
		Endpoint:        server.URL,
		ExpectedVersion: "v0.0.0-legacy.20260101010101",
		AllowLegacy:     true,
	})
	if err != nil {
		t.Fatalf("Ready() legacy error = %v", err)
	}
}

type readinessTestService struct {
	active bool
	err    error
}

func (s readinessTestService) Stop(context.Context) error  { return nil }
func (s readinessTestService) Start(context.Context) error { return nil }
func (s readinessTestService) Active(context.Context) (bool, error) {
	return s.active, s.err
}

type readinessTestChecker struct {
	called bool
}

func (c *readinessTestChecker) Ready(context.Context, ReadyExpectation) error {
	c.called = true
	return nil
}

func TestManagedReadyCheckerRequiresActiveService(t *testing.T) {
	next := &readinessTestChecker{}
	checker := managedReadyChecker{service: readinessTestService{active: false}, next: next}
	if err := checker.Ready(context.Background(), ReadyExpectation{}); err == nil {
		t.Fatal("Ready() accepted an inactive managed service")
	}
	if next.called {
		t.Fatal("downstream readiness ran while service was inactive")
	}

	checker.service = readinessTestService{err: errors.New("status unavailable")}
	if err := checker.Ready(context.Background(), ReadyExpectation{}); err == nil {
		t.Fatal("Ready() ignored service status error")
	}

	checker.service = readinessTestService{active: true}
	if err := checker.Ready(context.Background(), ReadyExpectation{}); err != nil {
		t.Fatalf("Ready() error = %v", err)
	}
	if !next.called {
		t.Fatal("downstream readiness was not called")
	}
}
