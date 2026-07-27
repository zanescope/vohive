package updater

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/zanescope/vohive/internal/schemacontract"
)

const (
	Repository       = "zanescope/vohive"
	GitHubAPIBase    = "https://api.github.com/repos/" + Repository
	GitHubReleaseURL = "https://github.com/" + Repository + "/releases"

	DefaultInstallRoot    = "/opt/vohive"
	DefaultConfigPath     = "/etc/vohive/config.yaml"
	DefaultDeploymentPath = "/etc/vohive/deployment.json"
	DefaultStateRoot      = "/var/lib/vohive/update"
	DefaultDataPath       = "/var/lib/vohive/data"
	DefaultReadinessKey   = DefaultStateRoot + "/readiness.key"

	ReadinessChallengeHeader = "X-VoHive-Readiness-Challenge"
	ReadinessKeyFileEnv      = "VOHIVE_READINESS_KEY_FILE"
)

var (
	ErrUpdateLocked           = errors.New("another update transaction is active")
	ErrSignatureUnavailable   = errors.New("release signature verification is not configured")
	ErrPortableUnsupported    = errors.New("portable installations require an external restart hook and cannot update automatically")
	ErrUpdateUnsupported      = errors.New("this deployment cannot update automatically")
	ErrNonReleaseBuild        = errors.New("automatic update is disabled for non-release builds")
	ErrTargetNotApplicable    = errors.New("the selected target is no longer applicable")
	ErrInvalidUpdateRequest   = errors.New("invalid update request")
	ErrManualRecoveryRequired = errors.New("manual recovery is required before another install or update can start")
	ErrInterruptedUpdate      = errors.New("an interrupted update must be recovered before VoHive can start")
	ErrReleaseUpstream        = errors.New("release metadata service is unavailable")
)

type Layout string

const (
	LayoutV1 Layout = "v1"
	LayoutV2 Layout = "v2"
)

type InstallType string

const (
	InstallSystemd  InstallType = "systemd"
	InstallOpenWrt  InstallType = "openwrt"
	InstallPortable InstallType = "portable"
)

type Channel string

const (
	ChannelStable Channel = "stable"
	ChannelBeta   Channel = "beta"
	ChannelPinned Channel = "pinned"
)

func ParseChannel(value string) (Channel, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "stable", "latest":
		return ChannelStable, nil
	case "beta":
		return ChannelBeta, nil
	case "pinned":
		return ChannelPinned, nil
	default:
		return "", fmt.Errorf("unsupported update channel %q", value)
	}
}

type Deployment struct {
	Schema          int         `json:"schema"`
	Product         string      `json:"product"`
	Repository      string      `json:"repository"`
	Channel         Channel     `json:"channel"`
	InstallType     InstallType `json:"install_type"`
	Layout          Layout      `json:"layout"`
	CurrentVersion  string      `json:"current_version"`
	LastGoodVersion string      `json:"last_good_version,omitempty"`
	InstallRoot     string      `json:"install_root"`
	ConfigPath      string      `json:"config_path"`
	DataPath        string      `json:"data_path"`
	StateRoot       string      `json:"state_root"`
	ReadyURL        string      `json:"ready_url,omitempty"`
}

func DefaultDeployment() Deployment {
	return Deployment{
		Schema:      1,
		Product:     "vohive",
		Repository:  Repository,
		Channel:     ChannelStable,
		InstallType: InstallSystemd,
		Layout:      LayoutV2,
		InstallRoot: DefaultInstallRoot,
		ConfigPath:  DefaultConfigPath,
		DataPath:    DefaultDataPath,
		StateRoot:   DefaultStateRoot,
	}
}

func (d Deployment) Validate() error {
	if d.Schema != 1 {
		return fmt.Errorf("unsupported deployment schema %d", d.Schema)
	}
	if d.Product != "vohive" || d.Repository != Repository {
		return fmt.Errorf("deployment identity must be vohive from %s", Repository)
	}
	if _, err := ParseChannel(string(d.Channel)); err != nil {
		return err
	}
	switch d.InstallType {
	case InstallSystemd, InstallOpenWrt, InstallPortable:
	default:
		return fmt.Errorf("unsupported install type %q", d.InstallType)
	}
	switch d.Layout {
	case LayoutV1, LayoutV2:
	default:
		return fmt.Errorf("unsupported layout %q", d.Layout)
	}
	for name, value := range map[string]string{
		"install_root": d.InstallRoot,
		"config_path":  d.ConfigPath,
		"data_path":    d.DataPath,
		"state_root":   d.StateRoot,
	} {
		if err := validateManagedPath(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func validateManagedPath(value string) error {
	if value == "" || !filepath.IsAbs(value) {
		return fmt.Errorf("path must be absolute: %q", value)
	}
	clean := filepath.Clean(value)
	volume := filepath.VolumeName(clean)
	root := string(filepath.Separator)
	if volume != "" {
		root = volume + string(filepath.Separator)
	}
	if clean == root || clean == volume {
		return fmt.Errorf("refusing filesystem root %q", value)
	}
	return nil
}

type SchemaRange struct {
	Min    int `json:"min"`
	Target int `json:"target"`
	Max    int `json:"max"`
}

type Artifact struct {
	Name       string `json:"name"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Format     string `json:"format"`
	BinaryPath string `json:"binary_path,omitempty"`
}

type ReleaseManifest struct {
	Schema            int         `json:"schema"`
	Product           string      `json:"product"`
	Repository        string      `json:"repository"`
	Version           string      `json:"version"`
	Channel           Channel     `json:"channel"`
	SourceRevision    string      `json:"source_revision"`
	MinUpdaterVersion string      `json:"min_updater_version"`
	MinDirectUpgrade  string      `json:"min_direct_upgrade,omitempty"`
	UpgradeVia        []string    `json:"upgrade_via,omitempty"`
	ConfigSchema      SchemaRange `json:"config_schema"`
	DatabaseSchema    SchemaRange `json:"database_schema"`
	Artifacts         []Artifact  `json:"artifacts"`
}

func (m ReleaseManifest) Validate(releaseTag string) error {
	if m.Schema != 1 {
		return fmt.Errorf("unsupported release manifest schema %d", m.Schema)
	}
	if m.Product != "vohive" || m.Repository != Repository {
		return fmt.Errorf("release manifest identity mismatch")
	}
	if !validVersion(releaseTag) || normalizeVersion(m.Version) != normalizeVersion(releaseTag) {
		return fmt.Errorf("release tag %q does not match manifest version %q", releaseTag, m.Version)
	}
	if !validVersion(m.Version) {
		return fmt.Errorf("invalid release version %q", m.Version)
	}
	if m.Channel != ChannelStable && m.Channel != ChannelBeta {
		return fmt.Errorf("invalid manifest channel %q", m.Channel)
	}
	if !isHex(m.SourceRevision, 40) {
		return fmt.Errorf("source revision must be a 40-character hexadecimal commit")
	}
	if !validVersion(m.MinUpdaterVersion) {
		return fmt.Errorf("invalid minimum updater version %q", m.MinUpdaterVersion)
	}
	if m.MinDirectUpgrade != "" && !validVersion(m.MinDirectUpgrade) {
		return fmt.Errorf("invalid minimum direct upgrade version %q", m.MinDirectUpgrade)
	}
	for _, bridge := range m.UpgradeVia {
		if !validVersion(bridge) {
			return fmt.Errorf("invalid bridge version %q", bridge)
		}
	}
	for name, schemas := range map[string]struct {
		declared SchemaRange
		compiled schemacontract.Range
	}{
		"config":   {declared: m.ConfigSchema, compiled: schemacontract.Config()},
		"database": {declared: m.DatabaseSchema, compiled: schemacontract.Database()},
	} {
		if err := validateManifestSchemaCompatibility(name, schemas.declared, schemas.compiled); err != nil {
			return err
		}
	}
	if len(m.Artifacts) == 0 {
		return errors.New("release manifest has no artifacts")
	}
	artifactNames := make(map[string]struct{}, len(m.Artifacts))
	platforms := make(map[string]struct{}, len(m.Artifacts))
	for _, artifact := range m.Artifacts {
		if artifact.Name == "" || artifact.Size <= 0 {
			return fmt.Errorf("artifact metadata is incomplete for %q", artifact.Name)
		}
		if strings.ContainsAny(artifact.Name, `/\`) || artifact.Name == "." || artifact.Name == ".." {
			return fmt.Errorf("unsafe artifact name %q", artifact.Name)
		}
		if !isHex(artifact.SHA256, 64) {
			return fmt.Errorf("artifact %q has an invalid SHA-256", artifact.Name)
		}
		if artifact.GOOS == "" || artifact.GOARCH == "" {
			return fmt.Errorf("artifact %q has no target platform", artifact.Name)
		}
		switch artifact.Format {
		case "tar.gz", "tgz", "binary":
		default:
			return fmt.Errorf("artifact %q has unsupported format %q", artifact.Name, artifact.Format)
		}
		if _, exists := artifactNames[artifact.Name]; exists {
			return fmt.Errorf("duplicate artifact name %q", artifact.Name)
		}
		artifactNames[artifact.Name] = struct{}{}
		platform := artifact.GOOS + "/" + artifact.GOARCH
		if _, exists := platforms[platform]; exists {
			return fmt.Errorf("duplicate artifact target %s", platform)
		}
		platforms[platform] = struct{}{}
	}
	return nil
}

func validateManifestSchemaCompatibility(name string, candidate SchemaRange, current schemacontract.Range) error {
	if candidate.Min < 0 || candidate.Target < candidate.Min || candidate.Max < candidate.Target {
		return fmt.Errorf("%s schema range must satisfy 0 <= min <= target <= max", name)
	}
	if current.Target < candidate.Min || current.Target > candidate.Max {
		return fmt.Errorf(
			"%s schema range %+v cannot consume current schema %d",
			name,
			candidate,
			current.Target,
		)
	}
	return nil
}

func (m ReleaseManifest) ArtifactFor(goos, goarch string) (Artifact, error) {
	if goarch == "arm" {
		goarch = "armv7"
	}
	for _, artifact := range m.Artifacts {
		if artifact.GOOS == goos && artifact.GOARCH == goarch {
			return artifact, nil
		}
	}
	return Artifact{}, fmt.Errorf("release %s has no artifact for %s/%s", m.Version, goos, goarch)
}

func HostArtifact(manifest ReleaseManifest) (Artifact, error) {
	return manifest.ArtifactFor(runtime.GOOS, runtime.GOARCH)
}

type Phase string

const (
	PhaseChecking         Phase = "checking"
	PhaseDownloading      Phase = "downloading"
	PhaseVerifying        Phase = "verifying"
	PhaseWaitingQuiesce   Phase = "waiting_for_quiesce"
	PhaseBackingUp        Phase = "backing_up"
	PhaseStopping         Phase = "stopping"
	PhaseSwitching        Phase = "switching"
	PhaseStarting         Phase = "starting"
	PhaseVerifyingService Phase = "verifying_service"
	PhaseCompleted        Phase = "completed"
	PhaseRollingBack      Phase = "rolling_back"
	PhaseRolledBack       Phase = "rolled_back"
	PhaseFailed           Phase = "failed"
	PhaseManualRecovery   Phase = "manual_recovery_required"
)

func (p Phase) Valid() bool {
	switch p {
	case PhaseChecking, PhaseDownloading, PhaseVerifying, PhaseWaitingQuiesce,
		PhaseBackingUp, PhaseStopping, PhaseSwitching, PhaseStarting,
		PhaseVerifyingService, PhaseCompleted, PhaseRollingBack, PhaseRolledBack,
		PhaseFailed, PhaseManualRecovery:
		return true
	default:
		return false
	}
}

func (p Phase) Terminal() bool {
	switch p {
	case PhaseCompleted, PhaseRolledBack, PhaseFailed, PhaseManualRecovery:
		return true
	default:
		return false
	}
}

type TransactionState struct {
	Schema                 int       `json:"schema"`
	ID                     string    `json:"id"`
	Operation              string    `json:"operation"`
	Phase                  Phase     `json:"phase"`
	CurrentVersion         string    `json:"current_version,omitempty"`
	TargetVersion          string    `json:"target_version,omitempty"`
	PreviousVersion        string    `json:"previous_version,omitempty"`
	PreviousTarget         string    `json:"previous_target,omitempty"`
	PreviousLastGoodTarget string    `json:"previous_last_good_target,omitempty"`
	TargetBackupPath       string    `json:"target_backup_path,omitempty"`
	ControlTouched         bool      `json:"control_touched,omitempty"`
	BackupPath             string    `json:"backup_path,omitempty"`
	ArtifactPath           string    `json:"artifact_path,omitempty"`
	StagingPath            string    `json:"staging_path,omitempty"`
	InstalledReleasePath   string    `json:"installed_release_path,omitempty"`
	Error                  string    `json:"error,omitempty"`
	RollbackError          string    `json:"rollback_error,omitempty"`
	StartedAt              time.Time `json:"started_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

type UpdateRequest struct {
	Schema  int     `json:"schema"`
	JobID   string  `json:"job_id,omitempty"`
	Channel Channel `json:"channel"`
	Version string  `json:"version,omitempty"`
}

type BackupMetadata struct {
	Schema            int         `json:"schema"`
	CreatedAt         time.Time   `json:"created_at"`
	SourceVersion     string      `json:"source_version"`
	ConfigPath        string      `json:"config_path"`
	DataPath          string      `json:"data_path"`
	ConfigPresent     bool        `json:"config_present"`
	DataPresent       bool        `json:"data_present"`
	DeploymentPresent bool        `json:"deployment_present,omitempty"`
	ControlPresent    bool        `json:"control_present,omitempty"`
	InstallType       InstallType `json:"install_type,omitempty"`
	MainEnabled       bool        `json:"main_enabled,omitempty"`
	RecoverEnabled    bool        `json:"recover_enabled,omitempty"`
	ServiceActive     bool        `json:"service_active,omitempty"`
}
