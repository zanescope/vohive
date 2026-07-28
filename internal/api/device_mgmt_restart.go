package api

import (
	"reflect"

	"github.com/zanescope/vohive/internal/config"
)

// deviceConfigForChangeDetection removes fields that are discovered at runtime
// and are only echoed to the Web UI for display. Device persistence deliberately
// never loads these fields, so comparing a submitted DTO against the persisted
// config without normalizing them makes a partial Web save look like a device
// behavior change.
func deviceConfigForChangeDetection(cfg config.DeviceConfig) config.DeviceConfig {
	// Runtime-discovered attachment.
	cfg.USBPath = ""
	cfg.ATPort = ""
	cfg.ManagePort = ""
	cfg.Interface = ""
	cfg.QMIDevice = ""
	cfg.ControlDevice = ""
	cfg.AudioDevice = ""

	// Card policy and SMS runtime state are not device-file intent.
	cfg.APN = ""
	cfg.NetworkEnabled = false
	cfg.IPVersion = ""
	cfg.VoWiFiEnabled = false
	cfg.AirplaneEnabled = false
	cfg.SMSEnabled = false

	// GET emits canonical Web defaults even when the compact YAML omits them.
	// Compare their effective meaning instead of their serialized spelling.
	cfg.ESIMTransport = config.NormalizeESIMTransport(cfg.ESIMTransport)
	cfg.ModuleVendor = config.NormalizeModuleVendor(cfg.ModuleVendor)
	cfg.MBIMTransport = config.NormalizeMBIMTransport(cfg.MBIMTransport)
	return cfg
}

func deviceConfigIntentChanged(old config.DeviceConfig, next config.DeviceConfig) bool {
	old = deviceConfigForChangeDetection(old)
	next = deviceConfigForChangeDetection(next)
	return !reflect.DeepEqual(old, next)
}

func preserveDeviceConfigFieldsOutsideWebForm(next config.DeviceConfig, base config.DeviceConfig) config.DeviceConfig {
	next.MBIMTransport = base.MBIMTransport
	next.USBNetMode = base.USBNetMode
	next.ESIMSwitch = base.ESIMSwitch
	return next
}

func deviceConfigRequiresRestart(old config.DeviceConfig, next config.DeviceConfig) bool {
	if config.NormalizeIMEI(old.ModemIMEI) != config.NormalizeIMEI(next.ModemIMEI) {
		return true
	}
	if old.USBPath != next.USBPath {
		return true
	}
	if old.Interface != next.Interface {
		return true
	}
	if old.ProxyPort != next.ProxyPort {
		return true
	}
	if old.ATPort != next.ATPort {
		return true
	}
	if old.ControlDevice != next.ControlDevice {
		return true
	}
	if config.NormalizeESIMTransport(old.ESIMTransport) != config.NormalizeESIMTransport(next.ESIMTransport) {
		return true
	}
	if old.BaudRate != next.BaudRate {
		return true
	}
	if old.DataBits != next.DataBits {
		return true
	}
	if old.StopBits != next.StopBits {
		return true
	}
	if old.Parity != next.Parity {
		return true
	}
	if old.APN != next.APN {
		return true
	}
	if old.IPVersion != next.IPVersion {
		return true
	}
	// 后端模式变更（at↔qmi↔auto）需要重建 Worker
	if old.DeviceBackend != next.DeviceBackend {
		return true
	}
	if qmiProxyConfigChanged(old, next) {
		return true
	}
	return false
}

func qmiProxyConfigChanged(old config.DeviceConfig, next config.DeviceConfig) bool {
	if old.QMIUseProxy != next.QMIUseProxy {
		return true
	}
	if old.QMIProxyPath != next.QMIProxyPath {
		return true
	}
	if old.QMIProxyExecutable != next.QMIProxyExecutable {
		return true
	}
	return false
}

func managedNetworkConfigChanged(old, next config.DeviceConfig) bool {
	return old.APN != next.APN ||
		old.Interface != next.Interface ||
		old.ControlDevice != next.ControlDevice ||
		old.IPVersion != next.IPVersion
}
