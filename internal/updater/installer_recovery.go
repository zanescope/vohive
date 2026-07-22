package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type installerBackupMarker struct {
	name        string
	destination string
	mode        os.FileMode
}

// restoreInstallerManagedFiles restores the host integration surface that is
// unique to the bootstrap installer. Update-created backups never use this
// path; callers must additionally gate it on TransactionState.Operation.
func restoreInstallerManagedFiles(ctx context.Context, paths RuntimePaths, backupPath string) error {
	if err := validateBackupPath(paths, backupPath); err != nil {
		return err
	}
	metadataData, err := os.ReadFile(filepath.Join(backupPath, "metadata.json"))
	if err != nil {
		return err
	}
	var metadata BackupMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		return fmt.Errorf("decode installer backup metadata: %w", err)
	}
	if metadata.Schema != 1 || filepath.Clean(metadata.ConfigPath) != filepath.Clean(paths.ConfigFile) || filepath.Clean(metadata.DataPath) != filepath.Clean(paths.DataDir) {
		return errors.New("installer backup metadata does not match this deployment")
	}

	var restoreErrors []error
	markers := []installerBackupMarker{
		{name: "trust", destination: filepath.Join(filepath.Dir(paths.ConfigFile), "update.pub"), mode: 0o600},
		{name: "control_previous", destination: filepath.Join(paths.ControlDir(), "vohivectl.previous"), mode: 0o755},
		{name: "convenience", destination: "/usr/local/sbin/vohivectl", mode: 0o755},
	}
	switch metadata.InstallType {
	case InstallSystemd:
		if !metadata.MainEnabled {
			if _, err := (OSCommandRunner{}).Run(ctx, "systemctl", "stop", "--no-block", "vohive.service"); err != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("cancel pending VoHive start before installer recovery: %w", err))
			}
		}
		markers = append(markers,
			installerBackupMarker{name: "unit_main", destination: "/etc/systemd/system/vohive.service", mode: 0o644},
			installerBackupMarker{name: "unit_update", destination: "/etc/systemd/system/vohive-update.service", mode: 0o644},
			installerBackupMarker{name: "unit_recover", destination: "/etc/systemd/system/vohive-recover.service", mode: 0o644},
			installerBackupMarker{name: "enable_main", destination: "/etc/systemd/system/multi-user.target.wants/vohive.service", mode: 0o644},
			installerBackupMarker{name: "enable_recover", destination: "/etc/systemd/system/multi-user.target.wants/vohive-recover.service", mode: 0o644},
		)
	case InstallOpenWrt:
		markers = append(markers,
			installerBackupMarker{name: "unit_main", destination: "/etc/init.d/vohive", mode: 0o755},
			installerBackupMarker{name: "unit_update", destination: "/etc/init.d/vohive-update", mode: 0o755},
			installerBackupMarker{name: "unit_recover", destination: "/etc/init.d/vohive-recover", mode: 0o755},
			installerBackupMarker{name: "enable_main", destination: "/etc/rc.d/S99vohive", mode: 0o755},
			installerBackupMarker{name: "enable_recover", destination: "/etc/rc.d/S97vohive-recover", mode: 0o755},
		)
	case InstallPortable:
	default:
		return fmt.Errorf("installer backup has invalid install type %q", metadata.InstallType)
	}

	for _, marker := range markers {
		if err := restoreInstallerBackupMarker(backupPath, marker); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore %s: %w", marker.name, err))
		}
	}
	if metadata.InstallType == InstallSystemd {
		if _, err := (OSCommandRunner{}).Run(ctx, "systemctl", "daemon-reload"); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("reload restored systemd units: %w", err))
		}
	}
	return errors.Join(restoreErrors...)
}

func restoreInstallerBackupMarker(backupPath string, marker installerBackupMarker) error {
	if err := validateManagedPath(marker.destination); err != nil {
		return err
	}
	type markerKind struct {
		suffix string
		path   string
	}
	var found []markerKind
	for _, suffix := range []string{"file", "link", "absent"} {
		path := filepath.Join(backupPath, marker.name+"."+suffix)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("backup marker is not a regular file: %s", path)
		}
		found = append(found, markerKind{suffix: suffix, path: path})
	}
	if len(found) != 1 {
		return fmt.Errorf("expected exactly one presence marker for %s, found %d", marker.name, len(found))
	}
	if info, err := os.Lstat(marker.destination); err == nil {
		if info.IsDir() {
			return fmt.Errorf("refusing to replace directory %s", marker.destination)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	switch found[0].suffix {
	case "file":
		return copyFile(found[0].path, marker.destination, marker.mode)
	case "absent":
		if err := os.Remove(marker.destination); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case "link":
		data, err := os.ReadFile(found[0].path)
		if err != nil {
			return err
		}
		if len(data) == 0 || len(data) > 4096 || strings.IndexByte(string(data), 0) >= 0 {
			return errors.New("invalid backed-up symlink target")
		}
		target := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
		if target == "" || strings.ContainsAny(target, "\r\n") {
			return errors.New("invalid backed-up symlink target")
		}
		if err := os.MkdirAll(filepath.Dir(marker.destination), 0o700); err != nil {
			return err
		}
		temporary := marker.destination + ".restore"
		if err := os.Remove(temporary); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(target, temporary); err != nil {
			return err
		}
		if err := os.Rename(temporary, marker.destination); err != nil {
			_ = os.Remove(temporary)
			return err
		}
		return nil
	default:
		return errors.New("unknown backup marker kind")
	}
}
