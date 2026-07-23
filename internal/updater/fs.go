package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type RuntimePaths struct {
	DeploymentFile string
	InstallRoot    string
	ConfigFile     string
	DataDir        string
	StateRoot      string
}

func PathsFor(deploymentFile string, deployment Deployment) RuntimePaths {
	return RuntimePaths{
		DeploymentFile: deploymentFile,
		InstallRoot:    deployment.InstallRoot,
		ConfigFile:     deployment.ConfigPath,
		DataDir:        deployment.DataPath,
		StateRoot:      deployment.StateRoot,
	}
}

func (p RuntimePaths) Validate() error {
	for name, value := range map[string]string{
		"deployment_file": p.DeploymentFile,
		"install_root":    p.InstallRoot,
		"config_file":     p.ConfigFile,
		"data_dir":        p.DataDir,
		"state_root":      p.StateRoot,
	} {
		if err := validateManagedPath(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func (p RuntimePaths) ReleasesDir() string  { return filepath.Join(p.InstallRoot, "releases") }
func (p RuntimePaths) CurrentLink() string  { return filepath.Join(p.InstallRoot, "current") }
func (p RuntimePaths) LastGoodLink() string { return filepath.Join(p.InstallRoot, "last-good") }
func (p RuntimePaths) ControlDir() string   { return filepath.Join(p.InstallRoot, "control") }
func (p RuntimePaths) StateFile() string    { return filepath.Join(p.StateRoot, "state.json") }
func (p RuntimePaths) RequestFile() string  { return filepath.Join(p.StateRoot, "request.json") }
func (p RuntimePaths) LockFile() string     { return filepath.Join(p.StateRoot, "update.lock") }
func (p RuntimePaths) DownloadsDir() string { return filepath.Join(p.StateRoot, "downloads") }
func (p RuntimePaths) BackupsDir() string   { return filepath.Join(filepath.Dir(p.StateRoot), "backups") }

func readManagedRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("managed metadata is not a regular file: %s", path)
	}
	return os.ReadFile(path)
}

func LoadDeployment(path string) (Deployment, error) {
	data, err := readManagedRegularFile(path)
	if err != nil {
		return Deployment{}, err
	}
	var deployment Deployment
	if err := json.Unmarshal(data, &deployment); err != nil {
		return Deployment{}, fmt.Errorf("decode deployment metadata: %w", err)
	}
	if err := deployment.Validate(); err != nil {
		return Deployment{}, err
	}
	return deployment, nil
}

func SaveDeployment(path string, deployment Deployment) error {
	if err := deployment.Validate(); err != nil {
		return err
	}
	return atomicWriteJSON(path, deployment, 0o600)
}

// DetectLayoutAt recognizes the supported binary layouts without changing them.
// A v1 installation keeps its config/data directories; only the program pointer
// is taken over by the updater when an update is explicitly requested.
func DetectLayoutAt(installRoot string) (Layout, error) {
	if err := validateManagedPath(installRoot); err != nil {
		return "", err
	}
	if info, err := os.Lstat(filepath.Join(installRoot, "current")); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return LayoutV2, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	for _, legacy := range []string{
		filepath.Join(installRoot, "bin", "vohive"),
		filepath.Join(installRoot, "vohive"),
	} {
		if info, err := os.Stat(legacy); err == nil && !info.IsDir() {
			return LayoutV1, nil
		} else if err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	return LayoutV2, nil
}

func DiscoverDeployment(deploymentPath string) (Deployment, error) {
	if deploymentPath == "" {
		deploymentPath = DefaultDeploymentPath
	}
	if deployment, err := LoadDeployment(deploymentPath); err == nil {
		return deployment, nil
	} else if !os.IsNotExist(err) {
		return Deployment{}, err
	}

	deployment := DefaultDeployment()
	layout, err := DetectLayoutAt(DefaultInstallRoot)
	if err != nil {
		return Deployment{}, err
	}
	deployment.Layout = layout
	if layout == LayoutV1 {
		deployment.ConfigPath = filepath.Join(DefaultInstallRoot, "config", "config.yaml")
		deployment.DataPath = filepath.Join(DefaultInstallRoot, "data")
	}
	if _, err := os.Stat("/etc/init.d/vohive"); err == nil {
		deployment.InstallType = InstallOpenWrt
	} else if _, err := os.Stat("/run/systemd/system"); err != nil {
		deployment.InstallType = InstallPortable
	}
	return deployment, nil
}

func LoadState(path string) (TransactionState, error) {
	data, err := readManagedRegularFile(path)
	if err != nil {
		return TransactionState{}, err
	}
	var state TransactionState
	if err := json.Unmarshal(data, &state); err != nil {
		return TransactionState{}, fmt.Errorf("decode update state: %w", err)
	}
	if state.Schema != 1 || state.ID == "" || !state.Phase.Valid() {
		return TransactionState{}, errors.New("invalid update state")
	}
	if err := validateJobID(state.ID); err != nil {
		return TransactionState{}, err
	}
	switch state.Operation {
	case "install", "update", "rollback":
	default:
		return TransactionState{}, errors.New("invalid update operation")
	}
	return state, nil
}

func validateJobID(jobID string) error {
	if jobID == "" {
		return nil
	}
	if jobID == "." || jobID == ".." {
		return fmt.Errorf("unsafe update job id %q", jobID)
	}
	if len(jobID) > 128 {
		return errors.New("update job id is too long")
	}
	for _, character := range jobID {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			character != '.' && character != '_' && character != '-' {
			return fmt.Errorf("unsafe update job id %q", jobID)
		}
	}
	return nil
}

func SaveRequest(path string, request UpdateRequest) error {
	if request.Schema == 0 {
		request.Schema = 1
	}
	if request.Schema != 1 {
		return fmt.Errorf("unsupported request schema %d", request.Schema)
	}
	if err := validateJobID(request.JobID); err != nil {
		return err
	}
	channel, err := ParseChannel(string(request.Channel))
	if err != nil {
		return err
	}
	request.Channel = channel
	if request.Version != "" && !validVersion(request.Version) {
		return fmt.Errorf("invalid requested version %q", request.Version)
	}
	if request.Channel == ChannelPinned && request.Version == "" {
		return errors.New("pinned channel requires an exact version")
	}
	return atomicWriteJSON(path, request, 0o600)
}

func LoadRequest(path string) (UpdateRequest, error) {
	data, err := readManagedRegularFile(path)
	if err != nil {
		return UpdateRequest{}, err
	}
	var request UpdateRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return UpdateRequest{}, fmt.Errorf("decode update request: %w", err)
	}
	if request.Schema != 1 {
		return UpdateRequest{}, fmt.Errorf("unsupported request schema %d", request.Schema)
	}
	if err := validateJobID(request.JobID); err != nil {
		return UpdateRequest{}, err
	}
	if request.Version != "" && !validVersion(request.Version) {
		return UpdateRequest{}, fmt.Errorf("invalid requested version %q", request.Version)
	}
	channel, err := ParseChannel(string(request.Channel))
	if err != nil {
		return UpdateRequest{}, err
	}
	request.Channel = channel
	if request.Channel == ChannelPinned && request.Version == "" {
		return UpdateRequest{}, errors.New("pinned channel requires an exact version")
	}
	return request, nil
}
func atomicWriteJSON(path string, value any, mode fs.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(path, data, mode)
}

func atomicWriteFile(path string, data []byte, mode fs.FileMode) error {
	if err := validateManagedPath(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	// Syncing directories is not implemented by every platform/filesystem. The
	// file itself is already durable; ignore only the directory-open failure.
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}

type UpdateLock struct {
	path string
	file *os.File
}

func AcquireUpdateLock(path string) (*UpdateLock, error) {
	if err := validateManagedPath(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil, ErrUpdateLocked
	}
	if err != nil {
		return nil, err
	}
	bootID, processStartTicks, identityErr := currentUpdateProcessIdentity(os.Getpid())
	if identityErr != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, identityErr
	}
	_, writeErr := fmt.Fprintf(file, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if writeErr == nil && bootID != "" {
		_, writeErr = fmt.Fprintf(file, "boot_id=%s\nprocess_start_ticks=%s\n", bootID, processStartTicks)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if writeErr != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, writeErr
	}
	return &UpdateLock{path: path, file: file}, nil
}

func (l *UpdateLock) Release() error {
	if l == nil {
		return nil
	}
	closeErr := l.file.Close()
	removeErr := os.Remove(l.path)
	if closeErr != nil {
		return closeErr
	}
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return nil
}

func IsLockStale(path string, maxAge time.Duration) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return time.Since(info.ModTime()) > maxAge, nil
}

func copyFile(source, destination string, mode fs.FileMode) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".copy-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, destination); err != nil {
		return err
	}
	ok = true
	return nil
}

func copyTree(source, destination string) error {
	if err := validateManagedPath(source); err != nil {
		return err
	}
	if err := validateManagedPath(destination); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("source path escapes backup root: %q", path)
		}
		target := filepath.Join(destination, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in managed data: %s", path)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular file in managed data: %s", path)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func createBackup(paths RuntimePaths, version string, now time.Time) (string, error) {
	if err := paths.Validate(); err != nil {
		return "", err
	}
	backupName := now.UTC().Format("20060102T150405.000000000Z") + "-" + sanitizeVersion(version)
	backupPath := filepath.Join(paths.BackupsDir(), backupName)
	if err := os.MkdirAll(backupPath, 0o700); err != nil {
		return "", err
	}
	metadata := BackupMetadata{
		Schema: 1, CreatedAt: now.UTC(), SourceVersion: version,
		ConfigPath: paths.ConfigFile, DataPath: paths.DataDir,
	}
	if info, err := os.Stat(paths.ConfigFile); err == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("config is not a regular file: %s", paths.ConfigFile)
		}
		metadata.ConfigPresent = true
		if err := copyFile(paths.ConfigFile, filepath.Join(backupPath, "config.yaml"), 0o600); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if info, err := os.Stat(paths.DataDir); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("data path is not a directory: %s", paths.DataDir)
		}
		metadata.DataPresent = true
		if err := copyTree(paths.DataDir, filepath.Join(backupPath, "data")); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	controlBinary := filepath.Join(paths.ControlDir(), "vohivectl")
	if info, err := os.Stat(controlBinary); err == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("control binary is not a regular file: %s", controlBinary)
		}
		metadata.ControlPresent = true
		if err := copyFile(controlBinary, filepath.Join(backupPath, "control", "vohivectl"), 0o755); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if data, err := os.ReadFile(paths.DeploymentFile); err == nil {
		metadata.DeploymentPresent = true
		if err := atomicWriteFile(filepath.Join(backupPath, "deployment.json"), data, 0o600); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := atomicWriteJSON(filepath.Join(backupPath, "metadata.json"), metadata, 0o600); err != nil {
		return "", err
	}
	return backupPath, nil
}

func restoreBackup(paths RuntimePaths, backupPath string) error {
	if err := paths.Validate(); err != nil {
		return err
	}
	if err := validateBackupPath(paths, backupPath); err != nil {
		return err
	}
	metadataData, err := os.ReadFile(filepath.Join(backupPath, "metadata.json"))
	if err != nil {
		return err
	}
	var metadata BackupMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		return err
	}
	if metadata.Schema != 1 || filepath.Clean(metadata.ConfigPath) != filepath.Clean(paths.ConfigFile) || filepath.Clean(metadata.DataPath) != filepath.Clean(paths.DataDir) {
		return errors.New("backup metadata does not match this deployment")
	}
	if metadata.ConfigPresent {
		if err := copyFile(filepath.Join(backupPath, "config.yaml"), paths.ConfigFile, 0o600); err != nil {
			return err
		}
	} else if err := os.Remove(paths.ConfigFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(paths.DataDir); err != nil {
		return err
	}
	if metadata.DataPresent {
		if err := copyTree(filepath.Join(backupPath, "data"), paths.DataDir); err != nil {
			return err
		}
	} else if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return err
	}
	return nil
}

func restoreControlFromBackup(paths RuntimePaths, backupPath string) error {
	if err := validateBackupPath(paths, backupPath); err != nil {
		return err
	}
	metadataData, err := os.ReadFile(filepath.Join(backupPath, "metadata.json"))
	if err != nil {
		return err
	}
	var metadata BackupMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		return fmt.Errorf("decode backup metadata: %w", err)
	}
	if metadata.Schema != 1 ||
		filepath.Clean(metadata.ConfigPath) != filepath.Clean(paths.ConfigFile) ||
		filepath.Clean(metadata.DataPath) != filepath.Clean(paths.DataDir) {
		return errors.New("backup metadata does not match this deployment")
	}
	if !metadata.ControlPresent {
		destination := filepath.Join(paths.ControlDir(), "vohivectl")
		if info, err := os.Lstat(destination); err == nil {
			if info.IsDir() {
				return errors.New("refusing to remove a control binary path that is a directory")
			}
			if err := os.Remove(destination); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	source := filepath.Join(backupPath, "control", "vohivectl")
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("backed-up control binary is not a regular file")
	}
	if err := copyFile(source, filepath.Join(paths.ControlDir(), "vohivectl"), 0o755); err != nil {
		return err
	}
	previous := filepath.Join(paths.ControlDir(), "vohivectl.previous")
	if err := os.Remove(previous); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
func validateBackupPath(paths RuntimePaths, backupPath string) error {
	if err := validateManagedPath(backupPath); err != nil {
		return err
	}
	root, err := filepath.Abs(paths.BackupsDir())
	if err != nil {
		return err
	}
	candidate, err := filepath.Abs(backupPath)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("backup path %q is outside %q", backupPath, root)
	}
	return nil
}

func sanitizeVersion(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	var out strings.Builder
	for _, char := range version {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '.' || char == '-' {
			out.WriteRune(char)
		}
	}
	if out.Len() == 0 {
		return "unknown-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	return "v" + out.String()
}
