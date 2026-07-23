package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/viper"
	yaml "go.yaml.in/yaml/v3"
)

const (
	LegacyConfigSchema           = 0
	MinimumSupportedConfigSchema = LegacyConfigSchema
	CurrentConfigSchema          = 1
)

var (
	ErrConfigSchemaTooNew      = errors.New("config schema is newer than this binary")
	ErrConfigSchemaUnsupported = errors.New("config schema is unsupported")
)

type ConfigMigrationStep struct {
	From        int    `json:"from"`
	To          int    `json:"to"`
	Description string `json:"description"`
}

type ConfigMigrationPlan struct {
	CurrentSchema int                   `json:"current_schema"`
	TargetSchema  int                   `json:"target_schema"`
	Steps         []ConfigMigrationStep `json:"steps"`
}

func (p ConfigMigrationPlan) NeedsMigration() bool { return len(p.Steps) > 0 }

type ConfigSchemaCompatibility struct {
	MinimumSupported int `json:"minimum_supported"`
	Target           int `json:"target"`
}

func ConfigCompatibility() ConfigSchemaCompatibility {
	return ConfigSchemaCompatibility{
		MinimumSupported: MinimumSupportedConfigSchema,
		Target:           CurrentConfigSchema,
	}
}

// InspectYAML returns the explicit schema. A valid legacy file without a
// config_schema key is schema 0.
func InspectYAML(data []byte) (int, error) {
	_, root, err := readConfigDocument(data)
	if err != nil {
		return 0, err
	}
	node := getMapValue(root, "config_schema")
	if node == nil {
		return LegacyConfigSchema, nil
	}
	if node.Kind != yaml.ScalarNode {
		return 0, fmt.Errorf("config_schema must be an integer")
	}
	version, err := strconv.Atoi(strings.TrimSpace(node.Value))
	if err != nil || version < 0 {
		return 0, fmt.Errorf("config_schema must be a non-negative integer")
	}
	return version, nil
}

func PlanMigration(data []byte) (ConfigMigrationPlan, error) {
	return PlanMigrationTo(data, CurrentConfigSchema)
}

func PlanMigrationTo(data []byte, target int) (ConfigMigrationPlan, error) {
	current, err := InspectYAML(data)
	if err != nil {
		return ConfigMigrationPlan{}, err
	}
	plan := ConfigMigrationPlan{CurrentSchema: current, TargetSchema: target}
	if target < MinimumSupportedConfigSchema || target > CurrentConfigSchema {
		return plan, fmt.Errorf("%w: target schema %d", ErrConfigSchemaUnsupported, target)
	}
	if current > target {
		return plan, fmt.Errorf("%w: file=%d binary=%d", ErrConfigSchemaTooNew, current, target)
	}
	if current < MinimumSupportedConfigSchema {
		return plan, fmt.Errorf("%w: file=%d minimum=%d", ErrConfigSchemaUnsupported, current, MinimumSupportedConfigSchema)
	}
	for version := current; version < target; version++ {
		switch version {
		case 0:
			plan.Steps = append(plan.Steps, ConfigMigrationStep{
				From:        0,
				To:          1,
				Description: "add explicit schema metadata and normalize legacy network/runtime path fields",
			})
		default:
			return plan, fmt.Errorf("%w: no migration from schema %d", ErrConfigSchemaUnsupported, version)
		}
	}
	return plan, nil
}

func PlanFile(path string) (ConfigMigrationPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ConfigMigrationPlan{}, fmt.Errorf("read config file: %w", err)
	}
	return PlanMigration(data)
}

// ValidateYAML validates structure, schema compatibility, and decoding. Unknown
// keys are accepted so future/plugin settings survive updates.
func ValidateYAML(data []byte) error {
	if _, err := PlanMigration(data); err != nil {
		return err
	}
	v := viper.New()
	v.SetConfigType("yaml")
	applyViperDefaults(v, DefaultConfig())
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cfg := DefaultConfig()
	if err := v.Unmarshal(&cfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	schema, err := InspectYAML(data)
	if err != nil {
		return err
	}
	if schema == CurrentConfigSchema && (strings.TrimSpace(cfg.Web.Username) == "" || strings.TrimSpace(cfg.Web.Password) == "") {
		return fmt.Errorf("schema %d requires initialized web credentials", CurrentConfigSchema)
	}
	return nil
}

func ValidateFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}
	return ValidateYAML(data)
}

func MigrateYAML(data []byte) ([]byte, ConfigMigrationPlan, error) {
	plan, err := PlanMigration(data)
	if err != nil {
		return nil, plan, err
	}
	if err := ValidateYAML(data); err != nil {
		return nil, plan, err
	}
	if !plan.NeedsMigration() {
		return data, plan, nil
	}

	doc, root, err := readConfigDocument(data)
	if err != nil {
		return nil, plan, err
	}
	for _, step := range plan.Steps {
		switch step.From {
		case 0:
			migrateConfigSchema0To1(root)
		default:
			return nil, plan, fmt.Errorf("%w: no migration from schema %d", ErrConfigSchemaUnsupported, step.From)
		}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, plan, fmt.Errorf("marshal migrated config: %w", err)
	}
	if err := ValidateYAML(out); err != nil {
		return nil, plan, fmt.Errorf("validate migrated config: %w", err)
	}
	return out, plan, nil
}

// MigrateFile applies a complete, idempotent migration through a same-directory
// temporary file. The updater must create a backup before calling this function.
func MigrateFile(path string) (ConfigMigrationPlan, error) {
	configFileMu.Lock()
	defer configFileMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return ConfigMigrationPlan{}, fmt.Errorf("read config file: %w", err)
	}
	out, plan, err := MigrateYAML(data)
	if err != nil {
		return plan, err
	}
	if !plan.NeedsMigration() {
		return plan, nil
	}
	if err := atomicWriteConfigFile(path, out); err != nil {
		return plan, err
	}
	return plan, nil
}

func migrateConfigSchema0To1(root *yaml.Node) {
	migrateLegacyManagedNetworkNode(root)
	migrateDeprecatedRuntimePathNodes(root)

	// These values are sticky defaults: writing the old effective behavior keeps
	// a later default change from silently altering an upgraded installation.
	if getMapValue(root, "free_device_limit") == nil {
		setMapInt(root, "free_device_limit", DefaultConfig().FreeDeviceLimit)
	}
	vowifi := ensureMapping(root, "vowifi")
	if getMapValue(vowifi, "enabled") == nil {
		setMapBool(vowifi, "enabled", DefaultConfig().VoWiFi.Enabled)
	}

	// Schema 0 used admin/admin as its effective fallback. Persist it only for
	// legacy files so those installations keep working. Fresh schema 1 files
	// receive a random password in EnsureConfigFile instead.
	web := ensureMapping(root, "web")
	if getMapValue(web, "username") == nil {
		setMapScalar(web, "username", InitialAdminUsername)
	}
	if getMapValue(web, "password") == nil {
		setMapScalar(web, "password", "admin")
	}

	// Normalize only the legacy shorthand (:7575). Host-qualified listen
	// addresses remain untouched.
	server := ensureMapping(root, "server")
	if getMapValue(server, "port") == nil {
		setMapInt(server, "port", 7575)
	}
	if getMapValue(server, "debug") == nil {
		setMapBool(server, "debug", false)
	}
	if port := getMapValue(server, "port"); port != nil && port.Kind == yaml.ScalarNode {
		trimmed := strings.TrimSpace(port.Value)
		if strings.HasPrefix(trimmed, ":") {
			if _, err := strconv.Atoi(strings.TrimPrefix(trimmed, ":")); err == nil {
				port.Tag = "!!int"
				port.Value = strings.TrimPrefix(trimmed, ":")
				// The legacy shorthand is commonly quoted. Once it becomes an
				// integer, clear that presentation style as well; retaining
				// DoubleQuotedStyle would serialize the migrated scalar as an
				// explicitly tagged quoted integer instead of plain YAML.
				port.Style = 0
			}
		}
	}

	// TODO(auth-schema): move file-backed credentials only in a coordinated
	// config+database transaction once the database credential store exists.
	deleteMapKey(root, "config_schema")
	root.Content = append([]*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "config_schema"},
		{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(CurrentConfigSchema)},
	}, root.Content...)
}
