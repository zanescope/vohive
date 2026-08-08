package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type updaterRoundTripFunc func(*http.Request) (*http.Response, error)

func (f updaterRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestValidateDownloadURLAllowsOnlyBuiltInProxiesToGitHub(t *testing.T) {
	valid := []string{
		"https://github.com/zanescope/vohive/releases/download/v1.6.5/release-manifest.json",
		"https://gh-proxy.net/https://api.github.com/repos/zanescope/vohive/releases/latest",
		"https://gh-proxy.com/https://github.com/zanescope/vohive/releases/latest/download/release-manifest.json",
	}
	for _, endpoint := range valid {
		if err := validateDownloadURL(endpoint, GitHubAPIBase); err != nil {
			t.Errorf("valid endpoint %q rejected: %v", endpoint, err)
		}
	}
	invalid := []string{
		"http://gh-proxy.net/https://github.com/zanescope/vohive/releases/latest",
		"https://unknown-proxy.example/https://github.com/zanescope/vohive/releases/latest",
		"https://gh-proxy.net/https://evil.example/payload",
		"https://gh-proxy.net/https://github.com.evil.example/payload",
		"https://gh-proxy.net/",
		"https://user:secret@gh-proxy.net/https://github.com/zanescope/vohive/releases/latest",
	}
	for _, endpoint := range invalid {
		if err := validateDownloadURL(endpoint, GitHubAPIBase); err == nil {
			t.Errorf("untrusted endpoint %q was accepted", endpoint)
		}
	}
}

func TestGitHubResolverFallsBackToAPIProxy(t *testing.T) {
	manifest := validTestManifest()
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	release := GitHubRelease{
		TagName: "v1.6.0",
		Assets: []ReleaseAsset{
			{
				Name:               "release-manifest.json",
				BrowserDownloadURL: GitHubReleaseURL + "/download/v1.6.0/release-manifest.json",
			},
			{
				Name:               "release-manifest.json.minisig",
				BrowserDownloadURL: GitHubReleaseURL + "/download/v1.6.0/release-manifest.json.minisig",
			},
			{
				Name:               manifest.Artifacts[0].Name,
				BrowserDownloadURL: GitHubReleaseURL + "/download/v1.6.0/" + manifest.Artifacts[0].Name,
				Size:               manifest.Artifacts[0].Size,
			},
		},
	}
	releaseBytes, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	var requestedHosts []string
	client := &http.Client{Transport: updaterRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestedHosts = append(requestedHosts, request.URL.Hostname())
		switch request.URL.Hostname() {
		case "github.com", "api.github.com":
			return nil, errors.New("direct GitHub unavailable")
		case "gh-proxy.net":
			switch {
			case strings.Contains(request.URL.Path, "/api.github.com/repos/"):
				return updaterTestResponse(request, http.StatusOK, releaseBytes, nil), nil
			case strings.HasSuffix(request.URL.Path, "/release-manifest.json"):
				return updaterTestResponse(request, http.StatusOK, manifestBytes, nil), nil
			case strings.HasSuffix(request.URL.Path, "/release-manifest.json.minisig"):
				return updaterTestResponse(request, http.StatusOK, []byte("signature"), nil), nil
			}
		}
		return updaterTestResponse(request, http.StatusBadGateway, nil, nil), nil
	})}
	resolver, err := NewGitHubResolver(client, allowSignature{})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := resolver.Check(context.Background(), CheckRequest{
		Channel: ChannelStable, CurrentVersion: "v1.5.0", GOOS: "linux", GOARCH: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.LatestVer != "v1.6.0" {
		t.Fatalf("latest version = %q, want v1.6.0", candidate.LatestVer)
	}
	if !containsString(requestedHosts, "gh-proxy.net") {
		t.Fatalf("API proxy was not used; hosts = %v", requestedHosts)
	}
	if containsString(requestedHosts, "gh-proxy.com") {
		t.Fatalf("second proxy was used after the first proxy succeeded; hosts = %v", requestedHosts)
	}
}

func TestGitHubResolverUsesSignedLatestFallbackThroughSecondProxy(t *testing.T) {
	manifest := validTestManifest()
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: updaterRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Hostname() {
		case "api.github.com", "github.com":
			return nil, errors.New("direct GitHub unavailable")
		case "gh-proxy.net":
			return nil, errors.New("first proxy unavailable")
		case "gh-proxy.com":
			if strings.Contains(request.URL.Path, "/api.github.com/") {
				return updaterTestResponse(request, http.StatusForbidden, nil, nil), nil
			}
			switch {
			case strings.HasSuffix(request.URL.Path, "/release-manifest.json"):
				return updaterTestResponse(request, http.StatusOK, manifestBytes, nil), nil
			case strings.HasSuffix(request.URL.Path, "/release-manifest.json.minisig"):
				return updaterTestResponse(request, http.StatusOK, []byte("signature"), nil), nil
			}
		}
		return updaterTestResponse(request, http.StatusBadGateway, nil, nil), nil
	})}
	resolver, err := NewGitHubResolver(client, allowSignature{})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := resolver.Check(context.Background(), CheckRequest{
		Channel: ChannelStable, CurrentVersion: "v1.5.0", GOOS: "linux", GOARCH: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantURL := GitHubReleaseURL + "/download/v1.6.0/" + manifest.Artifacts[0].Name
	if candidate.ArtifactURL != wantURL {
		t.Fatalf("artifact URL = %q, want %q", candidate.ArtifactURL, wantURL)
	}
}

func TestDownloadArtifactResumesAcrossSources(t *testing.T) {
	content := []byte("signed-release-artifact")
	sum := sha256.Sum256(content)
	candidate := Candidate{
		Artifact: Artifact{
			Name:   "vohive_v1.6.0_linux_amd64.tar.gz",
			Size:   int64(len(content)),
			SHA256: hex.EncodeToString(sum[:]),
		},
		ArtifactURL: GitHubReleaseURL + "/download/v1.6.0/vohive_v1.6.0_linux_amd64.tar.gz",
	}
	const firstChunk = 7
	var requestedHosts []string
	client := &http.Client{Transport: updaterRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestedHosts = append(requestedHosts, request.URL.Hostname())
		switch request.URL.Hostname() {
		case "github.com":
			return &http.Response{
				StatusCode:    http.StatusOK,
				Body:          &failAfterDataBody{data: content[:firstChunk]},
				ContentLength: int64(len(content)),
				Header:        make(http.Header),
				Request:       request,
			}, nil
		case "gh-proxy.net":
			if got := request.Header.Get("Range"); got != "bytes=7-" {
				t.Fatalf("Range = %q, want bytes=7-", got)
			}
			headers := make(http.Header)
			headers.Set("Content-Range", "bytes 7-22/23")
			return updaterTestResponse(request, http.StatusPartialContent, content[firstChunk:], headers), nil
		default:
			return updaterTestResponse(request, http.StatusBadGateway, nil, nil), nil
		}
	})}
	destination := filepath.Join(t.TempDir(), candidate.Artifact.Name)
	source, err := downloadArtifactWithProgress(context.Background(), client, candidate, destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source != "gh-proxy.net" {
		t.Fatalf("source = %q, want gh-proxy.net", source)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("downloaded content = %q, want %q", got, content)
	}
	if strings.Join(requestedHosts, ",") != "github.com,gh-proxy.net" {
		t.Fatalf("source order = %v", requestedHosts)
	}
}

func TestDownloadArtifactStopsStalledSources(t *testing.T) {
	content := []byte("artifact")
	sum := sha256.Sum256(content)
	candidate := Candidate{
		Artifact: Artifact{
			Name: "artifact.tar.gz", Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]),
		},
		ArtifactURL: GitHubReleaseURL + "/download/v1.6.0/artifact.tar.gz",
	}
	client := &http.Client{Transport: updaterRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Body:          &contextBlockingBody{ctx: request.Context()},
			ContentLength: int64(len(content)),
			Header:        make(http.Header),
			Request:       request,
		}, nil
	})}
	destination := filepath.Join(t.TempDir(), candidate.Artifact.Name)
	started := time.Now()
	_, err := downloadArtifact(context.Background(), client, candidate, destination, artifactDownloadOptions{
		sourceTimeout: time.Second,
		stallTimeout:  20 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), errArtifactDownloadStalled.Error()) {
		t.Fatalf("error = %v, want stalled download error", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled sources took too long to stop: %s", elapsed)
	}
}

type failAfterDataBody struct {
	data []byte
	sent bool
}

func (b *failAfterDataBody) Read(buffer []byte) (int, error) {
	if b.sent {
		return 0, errors.New("connection interrupted")
	}
	b.sent = true
	return copy(buffer, b.data), nil
}

func (*failAfterDataBody) Close() error { return nil }

type contextBlockingBody struct {
	ctx  context.Context
	once sync.Once
}

func (b *contextBlockingBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *contextBlockingBody) Close() error {
	b.once.Do(func() {})
	return nil
}

func updaterTestResponse(
	request *http.Request,
	status int,
	body []byte,
	headers http.Header,
) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{
		StatusCode:    status,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Header:        headers,
		Request:       request,
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
