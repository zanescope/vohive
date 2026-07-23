package updater

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type allowSignature struct{}

func (allowSignature) Verify(_, _ []byte) error { return nil }

func TestGitHubResolverBetaChoosesHighestSemver(t *testing.T) {
	server := newReleaseServer(t, []GitHubRelease{
		testRelease("v1.6.0-beta.1", true),
		testRelease("v1.7.0-beta.2", true),
		testRelease("v1.6.5-beta.9", true),
	})
	defer server.Close()
	resolver, _ := NewGitHubResolver(server.Client(), allowSignature{})
	resolver.apiBase = server.URL
	candidate, err := resolver.Check(context.Background(), CheckRequest{
		Channel: ChannelBeta, CurrentVersion: "v1.5.0", GOOS: "linux", GOARCH: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.LatestVer != "v1.7.0-beta.2" {
		t.Fatalf("got %s, want highest beta semver", candidate.LatestVer)
	}
}

func TestGitHubResolverExactTargetKeepsChannelPolicy(t *testing.T) {
	server := newReleaseServer(t, []GitHubRelease{testRelease("v1.7.0-beta.2", true)})
	defer server.Close()
	resolver, _ := NewGitHubResolver(server.Client(), allowSignature{})
	resolver.apiBase = server.URL
	_, err := resolver.Check(context.Background(), CheckRequest{
		Channel: ChannelStable, Version: "v1.7.0-beta.2", CurrentVersion: "v1.5.0",
		GOOS: "linux", GOARCH: "amd64",
	})
	if err == nil {
		t.Fatal("stable subscription accepted an exact beta target")
	}
	candidate, err := resolver.Check(context.Background(), CheckRequest{
		Channel: ChannelBeta, Version: "v1.7.0-beta.2", CurrentVersion: "v1.5.0",
		GOOS: "linux", GOARCH: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.LatestVer != "v1.7.0-beta.2" {
		t.Fatalf("unexpected target %s", candidate.LatestVer)
	}
}

func TestGitHubResolverNonReleaseBuildCannotAutoUpdate(t *testing.T) {
	server := newReleaseServer(t, []GitHubRelease{testRelease("v1.6.0", false)})
	defer server.Close()
	resolver, _ := NewGitHubResolver(server.Client(), allowSignature{})
	resolver.apiBase = server.URL
	candidate, err := resolver.Check(context.Background(), CheckRequest{
		Channel: ChannelStable, CurrentVersion: "Unknown", GOOS: "linux", GOARCH: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.HasUpdate {
		t.Fatal("non-release build was offered an automatic update")
	}
}

func newReleaseServer(t *testing.T, releases []GitHubRelease) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/releases/latest":
			for _, release := range releases {
				if !release.Prerelease {
					_ = json.NewEncoder(w).Encode(withAssetURLs(release, server.URL))
					return
				}
			}
			http.NotFound(w, r)
		case r.URL.Path == "/releases":
			result := make([]GitHubRelease, 0, len(releases))
			for _, release := range releases {
				result = append(result, withAssetURLs(release, server.URL))
			}
			_ = json.NewEncoder(w).Encode(result)
		case strings.HasPrefix(r.URL.Path, "/releases/tags/"):
			tag := strings.TrimPrefix(r.URL.Path, "/releases/tags/")
			for _, release := range releases {
				if release.TagName == tag {
					_ = json.NewEncoder(w).Encode(withAssetURLs(release, server.URL))
					return
				}
			}
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/manifest/"):
			tag := strings.TrimPrefix(r.URL.Path, "/manifest/")
			channel := ChannelStable
			if strings.Contains(tag, "-") {
				channel = ChannelBeta
			}
			manifest := validTestManifest()
			manifest.Version = tag
			manifest.Channel = channel
			manifest.Artifacts[0].Name = "vohive_" + tag + "_linux_amd64.tar.gz"
			_ = json.NewEncoder(w).Encode(manifest)
		case strings.HasPrefix(r.URL.Path, "/signature/"):
			_, _ = w.Write([]byte("test signature"))
		default:
			http.NotFound(w, r)
		}
	}))
	return server
}

func testRelease(tag string, prerelease bool) GitHubRelease {
	return GitHubRelease{TagName: tag, Name: tag, Prerelease: prerelease}
}

func withAssetURLs(release GitHubRelease, base string) GitHubRelease {
	release.Assets = []ReleaseAsset{
		{Name: "release-manifest.json", BrowserDownloadURL: base + "/manifest/" + release.TagName},
		{Name: "release-manifest.json.minisig", BrowserDownloadURL: base + "/signature/" + release.TagName},
		{Name: "vohive_" + release.TagName + "_linux_amd64.tar.gz", BrowserDownloadURL: base + "/artifact/" + release.TagName, Size: 42},
	}
	return release
}
