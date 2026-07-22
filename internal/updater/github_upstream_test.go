package updater

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubResolverClassifiesUpstreamHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	resolver, err := NewGitHubResolver(server.Client(), allowSignature{})
	if err != nil {
		t.Fatal(err)
	}
	resolver.apiBase = server.URL
	_, err = resolver.Check(context.Background(), CheckRequest{
		Channel: ChannelStable, CurrentVersion: "v1.6.0", GOOS: "linux", GOARCH: "amd64",
	})
	if !errors.Is(err, ErrReleaseUpstream) {
		t.Fatalf("resolver error = %v, want ErrReleaseUpstream", err)
	}
}
