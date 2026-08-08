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

const (
	releaseMetadataSourceTimeout = 5 * time.Second
	artifactSourceTimeout        = 3 * time.Minute
	artifactStallTimeout         = 20 * time.Second
)

var githubProxyBases = []string{
	"https://gh-proxy.net/",
	"https://gh-proxy.com/",
}

type downloadSource struct {
	Name string
	URL  string
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
		if r.apiBase != GitHubAPIBase {
			return Candidate{}, err
		}
		candidate, fallbackErr := r.checkWithoutAPI(ctx, request, subscriptionChannel, lookupChannel)
		if fallbackErr == nil {
			return candidate, nil
		}
		return Candidate{}, fmt.Errorf(
			"resolve release through GitHub API and signed release fallback: %w",
			errors.Join(err, fallbackErr),
		)
	}
	return r.candidateFromRelease(ctx, request, subscriptionChannel, release)
}

func (r *GitHubResolver) candidateFromRelease(
	ctx context.Context,
	request CheckRequest,
	subscriptionChannel Channel,
	release GitHubRelease,
) (Candidate, error) {
	manifestAsset, ok := findReleaseAsset(release.Assets, "release-manifest.json")
	if !ok {
		return Candidate{}, errors.New("release is missing release-manifest.json")
	}
	signatureAsset, ok := findReleaseAsset(release.Assets, "release-manifest.json.minisig")
	if !ok {
		return Candidate{}, errors.New("release is missing release-manifest.json.minisig")
	}
	manifestBytes, err := r.fetchVerifiedManifest(
		ctx,
		manifestAsset.BrowserDownloadURL,
		signatureAsset.BrowserDownloadURL,
	)
	if err != nil {
		return Candidate{}, err
	}
	var manifest ReleaseManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Candidate{}, fmt.Errorf("decode release manifest: %w", err)
	}
	prerelease := release.Prerelease
	return candidateFromManifest(
		request,
		subscriptionChannel,
		manifest,
		release.TagName,
		release.Body,
		&prerelease,
		release.Assets,
	)
}

func (r *GitHubResolver) checkWithoutAPI(
	ctx context.Context,
	request CheckRequest,
	subscriptionChannel Channel,
	lookupChannel Channel,
) (Candidate, error) {
	var releaseBase string
	switch lookupChannel {
	case ChannelStable:
		releaseBase = GitHubReleaseURL + "/latest/download"
	case ChannelPinned:
		if !validVersion(request.Version) {
			return Candidate{}, fmt.Errorf("invalid pinned version %q", request.Version)
		}
		releaseBase = GitHubReleaseURL + "/download/" + url.PathEscape(normalizeVersion(request.Version))
	default:
		return Candidate{}, errors.New("automatic beta discovery requires the GitHub API")
	}
	manifestBytes, err := r.fetchVerifiedManifest(
		ctx,
		releaseBase+"/release-manifest.json",
		releaseBase+"/release-manifest.json.minisig",
	)
	if err != nil {
		return Candidate{}, fmt.Errorf("download signed release fallback: %w", err)
	}
	var manifest ReleaseManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Candidate{}, fmt.Errorf("decode fallback release manifest: %w", err)
	}
	releaseTag := manifest.Version
	if lookupChannel == ChannelPinned {
		releaseTag = request.Version
	}
	return candidateFromManifest(
		request,
		subscriptionChannel,
		manifest,
		releaseTag,
		"",
		nil,
		nil,
	)
}

func candidateFromManifest(
	request CheckRequest,
	subscriptionChannel Channel,
	manifest ReleaseManifest,
	releaseTag string,
	releaseNote string,
	prerelease *bool,
	releaseAssets []ReleaseAsset,
) (Candidate, error) {
	if err := manifest.Validate(releaseTag); err != nil {
		return Candidate{}, err
	}
	if prerelease != nil && *prerelease && manifest.Channel != ChannelBeta {
		return Candidate{}, errors.New("prerelease must declare the beta channel")
	}
	if prerelease != nil && !*prerelease && manifest.Channel != ChannelStable {
		return Candidate{}, errors.New("stable release must declare the stable channel")
	}
	switch subscriptionChannel {
	case ChannelStable:
		if manifest.Channel != ChannelStable {
			return Candidate{}, errors.New("stable channel cannot install a beta target")
		}
	case ChannelBeta:
		if manifest.Channel != ChannelBeta {
			return Candidate{}, errors.New("beta channel requires a beta target")
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
	artifactURL := GitHubReleaseURL + "/download/" +
		url.PathEscape(normalizeVersion(manifest.Version)) + "/" + url.PathEscape(artifact.Name)
	if releaseAssets != nil {
		releaseArtifact, ok := findReleaseAsset(releaseAssets, artifact.Name)
		if !ok {
			return Candidate{}, fmt.Errorf("signed artifact %q is not attached to the release", artifact.Name)
		}
		if releaseArtifact.Size > 0 && releaseArtifact.Size != artifact.Size {
			return Candidate{}, fmt.Errorf("artifact size differs between GitHub and signed manifest")
		}
		artifactURL = releaseArtifact.BrowserDownloadURL
	}
	current := normalizeVersion(request.CurrentVersion)
	hasUpdate := validVersion(current) && compareVersions(current, manifest.Version) < 0
	if !validVersion(current) {
		current = strings.TrimSpace(request.CurrentVersion)
	}
	return Candidate{
		HasUpdate: hasUpdate, CurrentVer: current, LatestVer: normalizeVersion(manifest.Version),
		ReleaseNote: releaseNote, Manifest: manifest, Artifact: artifact,
		ArtifactURL: artifactURL,
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
	sources, err := downloadSourcesFor(endpoint, r.apiBase)
	if err != nil {
		return nil, err
	}
	var sourceErrors []error
	for _, source := range sources {
		data, sourceErr := r.fetchBytesFrom(ctx, source, limit)
		if sourceErr == nil {
			return data, nil
		}
		sourceErrors = append(sourceErrors, fmt.Errorf("%s: %w", source.Name, sourceErr))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("%w: all release sources failed: %w", ErrReleaseUpstream, errors.Join(sourceErrors...))
}

func (r *GitHubResolver) fetchBytesFrom(
	ctx context.Context,
	source downloadSource,
	limit int64,
) ([]byte, error) {
	if err := validateDownloadURL(source.URL, r.apiBase); err != nil {
		return nil, err
	}
	attemptCtx, cancel := context.WithTimeout(ctx, releaseMetadataSourceTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, source.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "vohive-updater")
	resp, err := trustedHTTPClient(r.client, r.apiBase).Do(req)
	if err != nil {
		return nil, fmt.Errorf("request release metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL == nil {
		return nil, errors.New("release response has no final URL")
	}
	if err := validateDownloadURL(resp.Request.URL.String(), r.apiBase); err != nil {
		return nil, fmt.Errorf("untrusted release redirect: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	reader := io.LimitReader(resp.Body, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read release metadata: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

func (r *GitHubResolver) fetchVerifiedManifest(
	ctx context.Context,
	manifestURL string,
	signatureURL string,
) ([]byte, error) {
	manifestSources, err := downloadSourcesFor(manifestURL, r.apiBase)
	if err != nil {
		return nil, err
	}
	signatureSources, err := downloadSourcesFor(signatureURL, r.apiBase)
	if err != nil {
		return nil, err
	}
	if len(manifestSources) != len(signatureSources) {
		return nil, errors.New("release manifest sources are inconsistent")
	}
	var sourceErrors []error
	for index, manifestSource := range manifestSources {
		signatureSource := signatureSources[index]
		manifestBytes, manifestErr := r.fetchBytesFrom(ctx, manifestSource, 4<<20)
		if manifestErr != nil {
			sourceErrors = append(sourceErrors, fmt.Errorf("%s manifest: %w", manifestSource.Name, manifestErr))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		signatureBytes, signatureErr := r.fetchBytesFrom(ctx, signatureSource, 64<<10)
		if signatureErr != nil {
			sourceErrors = append(sourceErrors, fmt.Errorf("%s signature: %w", signatureSource.Name, signatureErr))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if verifyErr := r.verifier.Verify(manifestBytes, signatureBytes); verifyErr != nil {
			sourceErrors = append(sourceErrors, fmt.Errorf("%s signature verification: %w", manifestSource.Name, verifyErr))
			continue
		}
		return manifestBytes, nil
	}
	return nil, fmt.Errorf("download and verify release manifest: %w", errors.Join(sourceErrors...))
}

func downloadSourcesFor(endpoint, apiBase string) ([]downloadSource, error) {
	if err := validateDownloadURL(endpoint, apiBase); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if isLocalAPIBase(apiBase) || isGitHubProxyHost(parsed.Hostname()) {
		return []downloadSource{{Name: parsed.Hostname(), URL: endpoint}}, nil
	}
	sources := []downloadSource{{Name: "github.com", URL: endpoint}}
	for _, proxyBase := range githubProxyBases {
		proxyURL, parseErr := url.Parse(proxyBase)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid built-in release proxy: %w", parseErr)
		}
		sources = append(sources, downloadSource{
			Name: strings.ToLower(proxyURL.Hostname()),
			URL:  proxyBase + endpoint,
		})
	}
	return sources, nil
}

func validateDownloadURL(endpoint, apiBase string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if parsed.User != nil || parsed.Host == "" || parsed.Fragment != "" {
		return errors.New("invalid release download URL")
	}
	if isLocalAPIBase(apiBase) {
		base, baseErr := url.Parse(apiBase)
		if baseErr != nil {
			return baseErr
		}
		if parsed.Scheme != base.Scheme || !strings.EqualFold(parsed.Host, base.Host) {
			return fmt.Errorf("untrusted local release host %q", parsed.Host)
		}
		return nil
	}
	if parsed.Scheme != "https" || parsed.Port() != "" {
		return errors.New("release download must use standard HTTPS")
	}
	if isGitHubProxyHost(parsed.Hostname()) {
		inner := strings.TrimPrefix(parsed.Path, "/")
		if parsed.RawQuery != "" {
			inner += "?" + parsed.RawQuery
		}
		if inner == "" {
			return errors.New("release proxy URL has no upstream URL")
		}
		return validateCanonicalGitHubURL(inner)
	}
	return validateCanonicalGitHubURL(endpoint)
}

func validateCanonicalGitHubURL(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" ||
		parsed.Port() != "" || parsed.Fragment != "" {
		return errors.New("invalid canonical GitHub release URL")
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return nil
	default:
		return fmt.Errorf("untrusted release host %q", parsed.Hostname())
	}
}

func isLocalAPIBase(apiBase string) bool {
	parsed, err := url.Parse(apiBase)
	return err == nil && parsed.Scheme == "http" && parsed.Hostname() == "127.0.0.1"
}

func isGitHubProxyHost(host string) bool {
	switch strings.ToLower(host) {
	case "gh-proxy.net", "gh-proxy.com":
		return true
	default:
		return false
	}
}

func trustedHTTPClient(client *http.Client, apiBase string) *http.Client {
	clone := *client
	previousCheck := clone.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := validateDownloadURL(req.URL.String(), apiBase); err != nil {
			return fmt.Errorf("refuse untrusted release redirect: %w", err)
		}
		if previousCheck != nil {
			return previousCheck(req, via)
		}
		if len(via) >= 10 {
			return errors.New("too many release redirects")
		}
		return nil
	}
	return &clone
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
	_, err := downloadArtifact(ctx, client, candidate, destination, artifactDownloadOptions{
		sourceTimeout: artifactSourceTimeout,
		stallTimeout:  artifactStallTimeout,
	})
	return err
}

type DownloadProgress struct {
	Source          string
	DownloadedBytes int64
	TotalBytes      int64
}

type artifactDownloadOptions struct {
	sourceTimeout time.Duration
	stallTimeout  time.Duration
	onProgress    func(DownloadProgress) error
}

var errArtifactDownloadStalled = errors.New("artifact download stalled")

func downloadArtifactWithProgress(
	ctx context.Context,
	client *http.Client,
	candidate Candidate,
	destination string,
	onProgress func(DownloadProgress) error,
) (string, error) {
	return downloadArtifact(ctx, client, candidate, destination, artifactDownloadOptions{
		sourceTimeout: artifactSourceTimeout,
		stallTimeout:  artifactStallTimeout,
		onProgress:    onProgress,
	})
}

func downloadArtifact(
	ctx context.Context,
	client *http.Client,
	candidate Candidate,
	destination string,
	options artifactDownloadOptions,
) (string, error) {
	if client == nil {
		client = &http.Client{Timeout: artifactSourceTimeout + time.Minute}
	}
	if err := validateDownloadURL(candidate.ArtifactURL, GitHubAPIBase); err != nil {
		return "", err
	}
	if candidate.Artifact.Size <= 0 || candidate.Artifact.Size > 2<<30 {
		return "", fmt.Errorf("invalid artifact size %d", candidate.Artifact.Size)
	}
	if err := validateManagedPath(destination); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", err
	}
	tmp := destination + ".part"
	if err := preparePartialArtifact(tmp, candidate.Artifact.Size); err != nil {
		return "", err
	}
	if info, err := os.Stat(tmp); err == nil && info.Size() == candidate.Artifact.Size {
		if verifyErr := VerifyFileSHA256(tmp, candidate.Artifact.SHA256); verifyErr == nil {
			if renameErr := os.Rename(tmp, destination); renameErr != nil {
				return "", renameErr
			}
			return "verified-partial", nil
		}
		if truncateErr := os.Truncate(tmp, 0); truncateErr != nil {
			return "", truncateErr
		}
	}
	sources, err := downloadSourcesFor(candidate.ArtifactURL, GitHubAPIBase)
	if err != nil {
		return "", err
	}
	var sourceErrors []error
	for _, source := range sources {
		if ctx.Err() != nil {
			sourceErrors = append(sourceErrors, ctx.Err())
			break
		}
		sourceErr := downloadArtifactFromSource(
			ctx,
			client,
			source,
			tmp,
			candidate.Artifact.Size,
			options,
		)
		if sourceErr != nil {
			sourceErrors = append(sourceErrors, fmt.Errorf("%s: %w", source.Name, sourceErr))
			continue
		}
		if verifyErr := VerifyFileSHA256(tmp, candidate.Artifact.SHA256); verifyErr != nil {
			sourceErrors = append(sourceErrors, fmt.Errorf("%s: verify downloaded artifact: %w", source.Name, verifyErr))
			if truncateErr := os.Truncate(tmp, 0); truncateErr != nil {
				return "", errors.Join(verifyErr, truncateErr)
			}
			continue
		}
		if renameErr := os.Rename(tmp, destination); renameErr != nil {
			return "", renameErr
		}
		return source.Name, nil
	}
	return "", fmt.Errorf("download artifact from all sources: %w", errors.Join(sourceErrors...))
}

func preparePartialArtifact(path string, total int64) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			return createErr
		}
		return file.Close()
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("partial artifact is not a regular file: %s", path)
	}
	if info.Size() < 0 || info.Size() > total {
		return os.Truncate(path, 0)
	}
	return nil
}

func downloadArtifactFromSource(
	ctx context.Context,
	client *http.Client,
	source downloadSource,
	path string,
	total int64,
	options artifactDownloadOptions,
) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	offset := info.Size()
	if offset == total {
		return nil
	}
	if options.sourceTimeout <= 0 {
		options.sourceTimeout = artifactSourceTimeout
	}
	if options.stallTimeout <= 0 {
		options.stallTimeout = artifactStallTimeout
	}
	if options.onProgress != nil {
		if err := options.onProgress(DownloadProgress{
			Source: source.Name, DownloadedBytes: offset, TotalBytes: total,
		}); err != nil {
			return err
		}
	}
	attemptCtx, cancelAttempt := context.WithTimeout(ctx, options.sourceTimeout)
	defer cancelAttempt()
	requestCtx, cancelRequest := context.WithCancelCause(attemptCtx)
	defer cancelRequest(nil)
	stallTimer := time.AfterFunc(options.stallTimeout, func() {
		cancelRequest(errArtifactDownloadStalled)
	})
	defer stallTimer.Stop()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, source.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "vohive-updater")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := trustedHTTPClient(client, GitHubAPIBase).Do(req)
	if err != nil {
		if errors.Is(context.Cause(requestCtx), errArtifactDownloadStalled) {
			return errArtifactDownloadStalled
		}
		return err
	}
	defer resp.Body.Close()
	stallTimer.Reset(options.stallTimeout)
	if resp.Request == nil || resp.Request.URL == nil {
		return errors.New("artifact response has no final URL")
	}
	if err := validateDownloadURL(resp.Request.URL.String(), GitHubAPIBase); err != nil {
		return fmt.Errorf("untrusted artifact redirect: %w", err)
	}

	writeOffset, remaining, err := validateArtifactResponse(resp, offset, total)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if writeOffset == 0 {
		if err := file.Truncate(0); err != nil {
			return err
		}
	}
	if _, err := file.Seek(writeOffset, io.SeekStart); err != nil {
		return err
	}
	reader := &artifactProgressReader{
		reader:       resp.Body,
		timer:        stallTimer,
		stallTimeout: options.stallTimeout,
		source:       source.Name,
		downloaded:   writeOffset,
		total:        total,
		onProgress:   options.onProgress,
	}
	written, copyErr := io.Copy(file, io.LimitReader(reader, remaining+1))
	stallTimer.Stop()
	syncErr := file.Sync()
	if errors.Is(context.Cause(requestCtx), errArtifactDownloadStalled) {
		return errArtifactDownloadStalled
	}
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if written != remaining {
		return fmt.Errorf("artifact size mismatch: expected %d more bytes, got %d", remaining, written)
	}
	return nil
}

func validateArtifactResponse(resp *http.Response, offset, total int64) (int64, int64, error) {
	writeOffset := int64(0)
	remaining := total
	switch {
	case offset > 0 && resp.StatusCode == http.StatusPartialContent:
		start, end, responseTotal, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return 0, 0, err
		}
		if start != offset || end < start || end >= total || responseTotal != total {
			return 0, 0, errors.New("artifact Content-Range mismatch")
		}
		writeOffset = offset
		remaining = total - offset
	case resp.StatusCode == http.StatusOK:
		// A source may ignore Range. Restart safely from byte zero in that case.
	case offset == 0 && resp.StatusCode == http.StatusPartialContent:
		return 0, 0, errors.New("artifact returned an unexpected partial response")
	default:
		return 0, 0, fmt.Errorf("download artifact returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != remaining {
		return 0, 0, fmt.Errorf(
			"artifact Content-Length mismatch: expected %d, got %d",
			remaining,
			resp.ContentLength,
		)
	}
	return writeOffset, remaining, nil
}

func parseContentRange(value string) (start, end, total int64, err error) {
	if _, scanErr := fmt.Sscanf(value, "bytes %d-%d/%d", &start, &end, &total); scanErr != nil {
		return 0, 0, 0, fmt.Errorf("invalid artifact Content-Range %q", value)
	}
	return start, end, total, nil
}

type artifactProgressReader struct {
	reader       io.Reader
	timer        *time.Timer
	stallTimeout time.Duration
	source       string
	downloaded   int64
	total        int64
	onProgress   func(DownloadProgress) error
}

func (r *artifactProgressReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if n <= 0 {
		return n, err
	}
	r.timer.Reset(r.stallTimeout)
	r.downloaded += int64(n)
	if r.onProgress != nil {
		if progressErr := r.onProgress(DownloadProgress{
			Source: r.source, DownloadedBytes: r.downloaded, TotalBytes: r.total,
		}); progressErr != nil {
			return n, progressErr
		}
	}
	return n, err
}
