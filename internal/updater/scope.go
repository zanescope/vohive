package updater

import (
	"fmt"
	"path/filepath"
)

// ValidateProductionScope is the destructive-operation boundary. Deployment
// metadata is data, not authority: editing deployment.json must never turn an
// update or rollback into an arbitrary filesystem copy/removal primitive.
func ValidateProductionScope(paths RuntimePaths) error {
	allowed := map[string][]string{
		"deployment_file": {DefaultDeploymentPath},
		"install_root":    {DefaultInstallRoot},
		"config_file": {
			DefaultConfigPath,
			filepath.Join(DefaultInstallRoot, "config", "config.yaml"),
		},
		"data_dir": {
			DefaultDataPath,
			filepath.Join(DefaultInstallRoot, "data"),
		},
		"state_root": {DefaultStateRoot},
	}
	actual := map[string]string{
		"deployment_file": paths.DeploymentFile,
		"install_root":    paths.InstallRoot,
		"config_file":     paths.ConfigFile,
		"data_dir":        paths.DataDir,
		"state_root":      paths.StateRoot,
	}
	for name, value := range actual {
		clean := filepath.Clean(value)
		matched := false
		for _, candidate := range allowed[name] {
			if clean == filepath.Clean(candidate) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s %q is outside the supported VoHive deployment scope", name, value)
		}
	}
	return nil
}
