package config

import (
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

type DeviceLimitError struct {
	Limit int
}

func (e *DeviceLimitError) Error() string {
	return fmt.Sprintf("device count has reached the limit: %d", e.Limit)
}

func AddDeviceInFile(path string, device DeviceConfig) error {
	return addDeviceInFile(path, device, 0, false)
}

// AddDeviceInFileWithLimit serializes the limit check and write through the
// shared atomic config mutation. A limit of 0 means unlimited.
func AddDeviceInFileWithLimit(path string, device DeviceConfig, limit int) error {
	return addDeviceInFile(path, device, limit, false)
}

// AddDeviceInFileWithLimitAndHostNetworkBackup atomically adds a device and,
// when requested, appends it to the ordered host-network candidate list.
func AddDeviceInFileWithLimitAndHostNetworkBackup(path string, device DeviceConfig, limit int, enabled bool) error {
	return addDeviceInFile(path, device, limit, enabled)
}

func addDeviceInFile(path string, device DeviceConfig, limit int, hostNetworkBackup bool) error {
	return updateDevicesAndRootAST(path, func(root, devices *yaml.Node) error {
		if _, err := mutateAddDevice(devices, device, limit); err != nil {
			return err
		}
		if hostNetworkBackup {
			return updateHostNetworkBackupCandidate(root, device.ID, true)
		}
		return nil
	})
}

func mutateAddDevice(devices *yaml.Node, device DeviceConfig, limit int) (*yaml.Node, error) {
	if findDeviceNodeByID(devices, device.ID) != nil {
		return nil, fmt.Errorf("设备已存在: %s", device.ID)
	}
	if limit > 0 && len(devices.Content) >= limit {
		return nil, &DeviceLimitError{Limit: limit}
	}
	devices.Content = append(devices.Content, deviceConfigToNode(device))
	return devices, nil
}

func UpdateDeviceInFile(path string, deviceID string, newDevice DeviceConfig) error {
	return updateDeviceInFile(path, deviceID, newDevice, nil)
}

// UpdateDeviceInFileWithHostNetworkBackup atomically saves the device and
// applies an optional Web checkbox change to the ordered candidate list.
func UpdateDeviceInFileWithHostNetworkBackup(path string, deviceID string, newDevice DeviceConfig, enabled *bool) error {
	return updateDeviceInFile(path, deviceID, newDevice, enabled)
}

func updateDeviceInFile(path string, deviceID string, newDevice DeviceConfig, hostNetworkBackup *bool) error {
	return updateDevicesAndRootAST(path, func(root, devices *yaml.Node) error {
		if _, err := mutateUpdateDevice(devices, deviceID, newDevice); err != nil {
			return err
		}
		if hostNetworkBackup != nil {
			return updateHostNetworkBackupCandidate(root, deviceID, *hostNetworkBackup)
		}
		return nil
	})
}

func mutateUpdateDevice(devices *yaml.Node, deviceID string, newDevice DeviceConfig) (*yaml.Node, error) {
	n := findDeviceNodeByID(devices, deviceID)
	if n == nil {
		return nil, fmt.Errorf("设备未找到: %s", deviceID)
	}

	setMapScalar(n, "id", newDevice.ID)
	setMapScalar(n, "name", newDevice.Name)
	if newDevice.ModemIMEI != "" {
		setMapScalar(n, "modem_imei", newDevice.ModemIMEI)
	} else {
		deleteMapKey(n, "modem_imei")
	}
	setMapScalar(n, "device_backend", newDevice.DeviceBackend)
	if strings.TrimSpace(newDevice.ModuleVendor) != "" && NormalizeModuleVendor(newDevice.ModuleVendor) != ModuleVendorQuectel {
		setMapScalar(n, "module_vendor", NormalizeModuleVendor(newDevice.ModuleVendor))
	} else {
		deleteMapKey(n, "module_vendor")
	}
	if newDevice.QMIUseProxy {
		setMapBool(n, "qmi_use_proxy", true)
	} else {
		deleteMapKey(n, "qmi_use_proxy")
	}
	if newDevice.QMIProxyPath != "" {
		setMapScalar(n, "qmi_proxy_path", newDevice.QMIProxyPath)
	} else {
		deleteMapKey(n, "qmi_proxy_path")
	}
	if newDevice.QMIProxyExecutable != "" {
		setMapScalar(n, "qmi_proxy_executable", newDevice.QMIProxyExecutable)
	} else {
		deleteMapKey(n, "qmi_proxy_executable")
	}

	if newDevice.ProxyPort > 0 {
		setMapInt(n, "proxy_port", newDevice.ProxyPort)
	} else {
		deleteMapKey(n, "proxy_port")
	}

	deleteMapKey(n, legacyManagedNetworkKey)

	return devices, nil
}

// UpdateDeviceIMEIInFile 仅回填 modem_imei,绝不触碰路径字段;IMEI 为空时跳过(不擦除已有值)。
func UpdateDeviceIMEIInFile(path string, updates map[string]string) error {
	return updateDevicesInFile(path, func(devices *yaml.Node) (*yaml.Node, error) {
		for deviceID, imei := range updates {
			if strings.TrimSpace(imei) == "" {
				continue
			}
			n := findDeviceNodeByID(devices, deviceID)
			if n == nil {
				return nil, fmt.Errorf("设备未找到: %s", deviceID)
			}
			setMapScalar(n, "modem_imei", strings.TrimSpace(imei))
		}
		return devices, nil
	})
}

func DeleteDeviceInFile(path string, deviceID string) error {
	return updateDevicesAndRootAST(path, func(root, devices *yaml.Node) error {
		if _, err := mutateDeleteDevice(devices, deviceID); err != nil {
			return err
		}
		return updateHostNetworkBackupCandidate(root, deviceID, false)
	})
}

func mutateDeleteDevice(devices *yaml.Node, deviceID string) (*yaml.Node, error) {
	for i, item := range devices.Content {
		if item == nil || item.Kind != yaml.MappingNode {
			continue
		}
		if v := getMapScalar(item, "id"); v == deviceID {
			devices.Content = append(devices.Content[:i], devices.Content[i+1:]...)
			return devices, nil
		}
	}
	return nil, fmt.Errorf("设备未找到: %s", deviceID)
}

func updateHostNetworkBackupCandidate(root *yaml.Node, deviceID string, enabled bool) error {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return fmt.Errorf("host network backup device ID cannot be empty")
	}

	hostFailover := getMapValue(root, "host_network_failover")
	if hostFailover == nil {
		if !enabled {
			return nil
		}
		hostFailover = ensureMapping(root, "host_network_failover")
	}
	if hostFailover.Kind != yaml.MappingNode {
		return fmt.Errorf("host_network_failover must be a mapping")
	}
	deleteMapKey(hostFailover, "enabled")

	candidateNode := getMapValue(hostFailover, "candidate_device_ids")
	candidates := make([]string, 0)
	seen := make(map[string]struct{})
	selected := false
	if candidateNode != nil {
		if candidateNode.Kind != yaml.SequenceNode {
			return fmt.Errorf("host_network_failover.candidate_device_ids must be a sequence")
		}
		candidates = make([]string, 0, len(candidateNode.Content)+1)
		for _, item := range candidateNode.Content {
			if item == nil || item.Kind != yaml.ScalarNode {
				return fmt.Errorf("host_network_failover.candidate_device_ids must contain strings")
			}
			candidateID := strings.TrimSpace(item.Value)
			if candidateID == "" {
				continue
			}
			if _, duplicate := seen[candidateID]; duplicate {
				continue
			}
			seen[candidateID] = struct{}{}
			if candidateID == deviceID {
				selected = true
				if !enabled {
					continue
				}
			}
			candidates = append(candidates, candidateID)
		}
	}
	if enabled && !selected {
		candidates = append(candidates, deviceID)
	}
	if len(candidates) == 0 {
		deleteMapKey(hostFailover, "candidate_device_ids")
		return nil
	}

	sequence := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, candidateID := range candidates {
		sequence.Content = append(sequence.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: candidateID})
	}
	setMapNode(hostFailover, "candidate_device_ids", sequence)
	return nil
}

func updateDevicesInFile(path string, mutate func(*yaml.Node) (*yaml.Node, error)) error {
	return updateDevicesAST(path, mutate)
}
func findDeviceNodeByID(devices *yaml.Node, id string) *yaml.Node {
	for _, item := range devices.Content {
		if item == nil || item.Kind != yaml.MappingNode {
			continue
		}
		if v := getMapScalar(item, "id"); v == id {
			return item
		}
	}
	return nil
}

func deviceConfigToNode(d DeviceConfig) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	appendMapScalar(m, "id", d.ID)
	if d.Name != "" {
		appendMapScalar(m, "name", d.Name)
	}
	if d.ModemIMEI != "" {
		appendMapScalar(m, "modem_imei", d.ModemIMEI)
	}
	if d.DeviceBackend != "" {
		appendMapScalar(m, "device_backend", d.DeviceBackend)
	}
	if strings.TrimSpace(d.ModuleVendor) != "" && NormalizeModuleVendor(d.ModuleVendor) != ModuleVendorQuectel {
		appendMapScalar(m, "module_vendor", NormalizeModuleVendor(d.ModuleVendor))
	}
	if d.QMIUseProxy {
		appendMapBool(m, "qmi_use_proxy", true)
	}
	if d.QMIProxyPath != "" {
		appendMapScalar(m, "qmi_proxy_path", d.QMIProxyPath)
	}
	if d.QMIProxyExecutable != "" {
		appendMapScalar(m, "qmi_proxy_executable", d.QMIProxyExecutable)
	}
	if d.ProxyPort > 0 {
		appendMapInt(m, "proxy_port", d.ProxyPort)
	}

	return m
}

func getMapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(m.Content); i += 2 {
		k := m.Content[i]
		v := m.Content[i+1]
		if k != nil && k.Value == key {
			return v
		}
	}
	return nil
}

func getMapScalar(m *yaml.Node, key string) string {
	v := getMapValue(m, key)
	if v == nil {
		return ""
	}
	return v.Value
}

func setMapNode(m *yaml.Node, key string, val *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(m.Content); i += 2 {
		k := m.Content[i]
		if k != nil && k.Value == key {
			previous := m.Content[i+1]
			if previous != nil {
				if val.HeadComment == "" {
					val.HeadComment = previous.HeadComment
				}
				if val.LineComment == "" {
					val.LineComment = previous.LineComment
				}
				if val.FootComment == "" {
					val.FootComment = previous.FootComment
				}
			}
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

func deleteMapKey(m *yaml.Node, key string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(m.Content); i += 2 {
		k := m.Content[i]
		if k != nil && k.Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func setMapScalar(m *yaml.Node, key, value string) {
	if value == "" {
		deleteMapKey(m, key)
		return
	}
	setMapNode(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func setMapInt(m *yaml.Node, key string, value int) {
	setMapNode(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", value)})
}

func setMapBool(m *yaml.Node, key string, value bool) {
	val := "false"
	if value {
		val = "true"
	}
	setMapNode(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: val})
}

func appendMapScalar(m *yaml.Node, key, value string) {
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func appendMapInt(m *yaml.Node, key string, value int) {
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", value)},
	)
}

func appendMapBool(m *yaml.Node, key string, value bool) {
	val := "false"
	if value {
		val = "true"
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: val},
	)
}
