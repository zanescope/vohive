package config

import (
	"reflect"
	"strings"

	"github.com/spf13/viper"
)

// DefaultConfig is the single source of truth for logical configuration
// defaults. These values are merged in memory; they are not used as a template
// that overwrites a user's config.yaml.
func DefaultConfig() Config {
	return Config{
		ConfigSchema:    CurrentConfigSchema,
		FreeDeviceLimit: DefaultFreeDeviceLimit,
		Server: ServerConfig{
			Port: "7575",
		},
		Webhook: WebhookConfig{
			TimeoutMs:    5000,
			RetryMax:     3,
			TextTemplate: DefaultWebhookTextTemplate,
		},
		Bark: BarkConfig{
			Group: "vohive",
			Level: "active",
		},
		VoWiFi: VoWiFiConfig{
			Mode: "vowifi",
		},
	}
}

// applyViperDefaults derives every key and value from DefaultConfig, including
// zero-value booleans. Keeping this reflective avoids a second, drifting list of
// default values in the loader.
func applyViperDefaults(v *viper.Viper, defaults Config) {
	setStructDefaults(v, "", reflect.ValueOf(defaults))
}

func setStructDefaults(v *viper.Viper, prefix string, value reflect.Value) {
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	valueType := value.Type()
	for i := 0; i < value.NumField(); i++ {
		fieldType := valueType.Field(i)
		if !fieldType.IsExported() {
			continue
		}
		name := strings.Split(fieldType.Tag.Get("mapstructure"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(fieldType.Name)
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}

		field := value.Field(i)
		if field.Kind() == reflect.Struct {
			setStructDefaults(v, key, field)
			continue
		}
		v.SetDefault(key, field.Interface())
	}
}
