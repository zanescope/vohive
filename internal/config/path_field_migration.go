package config

// deprecatedRuntimePathKeys are runtime-discovered values that must never be
// persisted in config.yaml.
var deprecatedRuntimePathKeys = []string{
	"usb_path",
	"at_port",
	"manage_port",
	"interface",
	"qmi_device",
	"control_device",
	"audio_device",
}
