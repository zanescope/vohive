package api

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/zanescope/vohive/internal/global"
	"github.com/zanescope/vohive/internal/updater"
)

func TestReadinessEndpointProvesManagedVersion(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "readiness.key")
	if err := updater.RotateReadinessKey(keyFile); err != nil {
		t.Fatal(err)
	}
	t.Setenv(updater.ReadinessKeyFileEnv, keyFile)

	previousVersion := global.Version
	global.Version = "v1.2.3"
	t.Cleanup(func() { global.Version = previousVersion })

	server := httptest.NewServer((&Server{}).newRouter())
	defer server.Close()

	checker := updater.HTTPReadyChecker{Client: server.Client()}
	err := checker.Ready(context.Background(), updater.ReadyExpectation{
		Endpoint:        server.URL + "/readyz",
		ExpectedVersion: "v1.2.3",
		KeyFile:         keyFile,
	})
	if err != nil {
		t.Fatalf("Ready() error = %v", err)
	}
}
