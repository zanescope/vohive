package config

import yaml "go.yaml.in/yaml/v3"

func migrateLegacyManagedNetworkNode(root *yaml.Node) bool {
	devices := getMapValue(root, "devices")
	if devices == nil || devices.Kind != yaml.SequenceNode {
		return false
	}
	changed := false
	for _, item := range devices.Content {
		if item == nil || item.Kind != yaml.MappingNode {
			continue
		}
		if getMapValue(item, legacyManagedNetworkKey) == nil {
			continue
		}
		if getMapValue(item, managedNetworkKey) == nil {
			// Preserve the established compatibility behavior: legacy managed
			// devices start disabled until the card policy is applied.
			setMapBool(item, managedNetworkKey, false)
		}
		deleteMapKey(item, legacyManagedNetworkKey)
		changed = true
	}
	return changed
}

func migrateDeprecatedRuntimePathNodes(root *yaml.Node) bool {
	devices := getMapValue(root, "devices")
	if devices == nil || devices.Kind != yaml.SequenceNode {
		return false
	}
	changed := false
	for _, item := range devices.Content {
		if item == nil || item.Kind != yaml.MappingNode {
			continue
		}
		for _, key := range deprecatedRuntimePathKeys {
			if getMapValue(item, key) == nil {
				continue
			}
			deleteMapKey(item, key)
			changed = true
		}
	}
	return changed
}
