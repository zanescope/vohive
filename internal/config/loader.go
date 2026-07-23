package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

func loadCurrent(path string) (*Config, error) {
	initial, err := EnsureConfigFile(path)
	if err != nil {
		return nil, err
	}
	// Keep direct binary upgrades compatible while the updater is being rolled
	// out. The updater should plan and back up before this idempotent call.
	if _, err := MigrateFile(path); err != nil {
		return nil, err
	}

	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	defaults := DefaultConfig()
	applyViperDefaults(v, defaults)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	bindCompatibleEnvironment(v)

	cfg := defaults
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("decode config file: %w", err)
	}
	if cfg.ConfigSchema != CurrentConfigSchema {
		return nil, fmt.Errorf("%w: file=%d binary=%d", ErrConfigSchemaUnsupported, cfg.ConfigSchema, CurrentConfigSchema)
	}
	if cfg.FreeDeviceLimit < 0 {
		return nil, fmt.Errorf("free_device_limit cannot be negative (0 means unlimited)")
	}
	publicIPProbe, err := loadPublicIPProbeFromYAML(path)
	if err != nil {
		return nil, fmt.Errorf("public_ip_probe: %w", err)
	}
	cfg.PublicIPProbe = publicIPProbe

	if strings.TrimSpace(cfg.Web.Username) == "" || strings.TrimSpace(cfg.Web.Password) == "" {
		return nil, fmt.Errorf("schema %d requires initialized web credentials", CurrentConfigSchema)
	}
	if initial != nil && (cfg.Web.Username != initial.Username || cfg.Web.Password != initial.Password) {
		// Runtime environment credentials are authoritative. Do not print a
		// generated file fallback that cannot log in to this process.
		_ = ConsumeInitialAdminCredentials()
		initial.Username = ""
		initial.Password = ""
	}
	if len(cfg.Feishu.ChatIDs) == 0 && strings.TrimSpace(cfg.Feishu.ChatID) != "" {
		cfg.Feishu.ChatIDs = []string{strings.TrimSpace(cfg.Feishu.ChatID)}
	}
	if cfg.Server.Port != "" && !strings.Contains(cfg.Server.Port, ":") {
		cfg.Server.Port = ":" + cfg.Server.Port
	}
	return &cfg, nil
}

func bindCompatibleEnvironment(v *viper.Viper) {
	replacer := strings.NewReplacer(".", "_", "-", "_")
	for _, key := range v.AllKeys() {
		// Schema is file metadata, not a runtime setting.
		if key == "config_schema" {
			continue
		}
		envKey := strings.ToUpper(replacer.Replace(key))
		// VOHIVE_ is authoritative. PROXY_ remains a compatibility alias.
		_ = v.BindEnv(key, "VOHIVE_"+envKey, "PROXY_"+envKey)
	}
}
