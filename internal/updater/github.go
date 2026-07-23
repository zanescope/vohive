package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type GitHubRelease struct {
	TagName    string         `json:"tag_name"`
	Name       string         `json:"name"`
	Body       string         `json:"body"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []ReleaseAsset `json:"assets"`
}

type CheckRequest struct {
	Channel        Channel
	Version        string
	CurrentVersion string
	GOOS           string
	GOARCH         string
}

type Candidate struct {
	HasUpdate   bool            `json:"has_update"`
	CurrentVer  string          `json:"current_version"`
	LatestVer   string          `json:"latest_version"`
	ReleaseNote string          `json:"release_note"`
	Manifest    ReleaseManifest `json:"manifest"`
	Artifact    Artifact        `json:"artifact"`
	ArtifactURL string          `json:"-"`
}

type ReleaseResolver interface {
	Check(context.Context, CheckRequest) (Candidate, error)
}

type GitHubResolver struct {
	client   *http.Client
	verifier SignatureVerifier
	apiBase  string
}

func NewGitHubResolver(client *http.Client, verifier SignatureVerifier) (*GitHubResolver, error) {
	if verifier == nil {
		return nil, ErrSignatureUnavailable
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GitHubResolver{client: client, verifier: verifier, apiBase: GitHubAPIBase}, nil
}

func (r *GitHubResolver) Check(ctx context.Context, request CheckRequest) (Candidate, error) {
	subscriptionChannel, err := ParseChannel(string(request.Channel))
	if err != nil {
		return Candidate{}, err
	}
	lookupChannel := subscriptionChannel
	if request.Version != "" {
		if !validVersion(request.Version) {
			return Candidate{}, fmt.Errorf("invalid requested version %q", request.Version)
		}
		lookupChannel = ChannelPinned
	}
	release, err := r.resolveRelease(ctx, lookupChannel, request.Version)
	if err != nil {
		return Candidate{}, err
	}
	manifestAsset, ok := findReleaseAsset(release.Assets, "release-manifest.json")
	if !ok {
		return Candidate{}, errors.New("release is missing release-manifest.json")
	}
	signatureAsset, ok := findReleaseAsset(release.Assets, "release-manifest.json.minisig")
	if !ok {
		return Candidate{}, errors.New("release is missing release-manifest.json.minisig")
	}
	manifestBytes, err := r.fetchBytes(ctx, manifestAsset.BrowserDownloadURL, 4<<20)
	if err != nil {
		return Candidate{}, fmt.Errorf("download release manifest: %w", err)
	}
	signatureBytes, err := r.fetchBytes(ctx, signatureAsset.BrowserDownloadURL, 64<<10)
	if err != nil {
		return Candidate{}, fmt.Errorf("download release manifest signature: %w", err)
	}
	if err := r.verifier.Verify(manifestBytes, signatureBytes); err != nil {
		return Candidate{}, fmt.Errorf("verify release manifest: %w", err)
	}
	var manifest ReleaseManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Candidate{}, fmt.Errorf("decode release manifest: %w", err)
	}
	if err := manifest.Validate(release.TagName); err != nil {
		return Candidate{}, err
	}
	if release.Prerelease && manifest.Channel != ChannelBeta {
		return Candidate{}, errors.New("prerelease must declare the beta channel")
	}
	if !release.Prerelease && manifest.Channel != ChannelStable {
		return Candidate{}, errors.New("stable release must declare the stable channel")
	}
	if request.Version != "" {
		switch subscriptionChannel {
		case ChannelStable:
			if release.Prerelease {
				return Candidate{}, errors.New("stable channel cannot install an exact beta target")
			}
		case ChannelBeta:
			if !release.Prerelease {
				return Candidate{}, errors.New("beta channel requires an exact beta target")
			}
		}
	}
	goos, goarch := request.GOOS, request.GOARCH
	if goos == "" || goarch == "" {
		return Candidate{}, errors.New("target operating system and architecture are required")
	}
	artifact, err := manifest.ArtifactFor(goos, goarch)
	if err != nil {
		return Candidate{}, err
	}
	releaseArtifact, ok := findReleaseAsset(release.Assets, artifact.Name)
	if !ok {
		return Candidate{}, fmt.Errorf("signed artifact %q is not attached to the release", artifact.Name)
	}
	if releaseArtifact.Size > 0 && releaseArtifact.Size != artifact.Size {
		return Candidate{}, fmt.Errorf("artifact size differs between GitHub and signed manifest")
	}
	current := normalizeVersion(request.CurrentVersion)
	hasUpdate := validVersion(current) && compareVersions(current, manifest.Version) < 0
	if !validVersion(current) {
		current = strings.TrimSpace(request.CurrentVersion)
	}
	return Candidate{
		HasUpdate: hasUpdate, CurrentVer: current, LatestVer: normalizeVersion(manifest.Version),
		ReleaseNote: release.Body, Manifest: manifest, Artifact: artifact,
		ArtifactURL: releaseArtifact.BrowserDownloadURL,
	}, nil
}

func (r *GitHubResolver) resolveRelease(ctx context.Context, channel Channel, version string) (GitHubRelease, error) {
	switch channel {
	case ChannelStable:
		var release GitHubRelease
		if err := r.fetchJSON(ctx, r.apiBase+"/releases/latest", &release, 4<<20); err != nil {
			return GitHubRelease{}, err
		}
		if release.Draft || release.Prerelease {
			return GitHubRelease{}, errors.New("GitHub latest release is not a stable release")
		}
		return release, nil
	case ChannelBeta:
		var releases []GitHubRelease
		if err := r.fetchJSON(ctx, r.apiBase+"/releases?per_page=30", &releases, 16<<20); err != nil {
			return GitHubRelease{}, err
		}
		var selected *GitHubRelease
		for index := range releases {
			release := &releases[index]
			if release.Draft || !release.Prerelease || !validVersion(release.TagName) {
				continue
			}
			if selected == nil || compareVersions(release.TagName, selected.TagName) > 0 {
				selected = release
			}
		}
		if selected == nil {
			return GitHubRelease{}, errors.New("no beta release is available")
		}
		return *selected, nil
	case ChannelPinned:
		if !validVersion(version) {
			return GitHubRelease{}, fmt.Errorf("invalid pinned version %q", version)
		}
		var release GitHubRelease
		endpoint := r.apiBase + "/releases/tags/" + url.PathEscape(normalizeVersion(version))
		if err := r.fetchJSON(ctx, endpoint, &release, 4<<20); err != nil {
			return GitHubRelease{}, err
		}
		if release.Draft {
			return GitHubRelease{}, errors.New("draft releases cannot be installed")
		}
		return release, nil
	default:
		return GitHubRelease{}, fmt.Errorf("unsupported channel %q", channel)
	}
}
func (r *GitHubResolver) fetchJSON(ctx context.Context, endpoint string, destination any, limit int64) error {
	data, err := r.fetchBytes(ctx, endpoint, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("%w: decode GitHub response: %v", ErrReleaseUpstream, err)
	}
	return nil
}

func (r *GitHubResolver) fetchBytes(ctx context.Context, endpoint string, limit int64) ([]byte, error) {
	if err := validateDownloadURL(endpoint, r.apiBase); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "vohive-updater")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: request release metadata: %v", ErrReleaseUpstream, err)
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL == nil {
		return nil, errors.New("release response has no final URL")
	}
	if err := validateDownloadURL(resp.Request.URL.String(), r.apiBase); err != nil {
		return nil, fmt.Errorf("untrusted release redirect: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: GET %s returned HTTP %d", ErrReleaseUpstream, endpoint, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	reader := io.LimitReader(resp.Body, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("%w: read release metadata: %v", ErrReleaseUpstream, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

func validateDownloadURL(endpoint, apiBase string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" && !strings.HasPrefix(apiBase, "http://127.0.0.1:") {
		return fmt.Errorf("release download must use HTTPS")
	}
	if parsed.User != nil || parsed.Host == "" {
		return errors.New("invalid release download URL")
	}
	if strings.HasPrefix(apiBase, "http://127.0.0.1:") {
		return nil
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return nil
	default:
		return fmt.Errorf("untrusted release host %q", parsed.Hostname())
	}
}

func findReleaseAsset(assets []ReleaseAsset, name string) (ReleaseAsset, bool) {
	for _, asset := range assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return ReleaseAsset{}, false
}

func DownloadArtifact(ctx context.Context, client *http.Client, candidate Candidate, destination string) error {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	if err := validateDownloadURL(candidate.ArtifactURL, GitHubAPIBase); err != nil {
		return err
	}
	if candidate.Artifact.Size <= 0 || candidate.Artifact.Size > 2<<30 {
		return fmt.Errorf("invalid artifact size %d", candidate.Artifact.Size)
	}
	if err := validateManagedPath(destination); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	tmp := destination + ".part"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		if removeErr := os.Remove(tmp); removeErr != nil {
			return removeErr
		}
		file, err = os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, candidate.ArtifactURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "vohive-updater")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL == nil {
		return errors.New("artifact response has no final URL")
	}
	if err := validateDownloadURL(resp.Request.URL.String(), GitHubAPIBase); err != nil {
		return fmt.Errorf("untrusted artifact redirect: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download artifact returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != candidate.Artifact.Size {
		return fmt.Errorf("artifact Content-Length mismatch")
	}
	written, err := io.Copy(file, io.LimitReader(resp.Body, candidate.Artifact.Size+1))
	if err != nil {
		return err
	}
	if written != candidate.Artifact.Size {
		return fmt.Errorf("artifact size mismatch: expected %d, got %d", candidate.Artifact.Size, written)
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := VerifyFileSHA256(tmp, candidate.Artifact.SHA256); err != nil {
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		return err
	}
	ok = true
	return nil
}
