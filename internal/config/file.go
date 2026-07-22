package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	yaml "go.yaml.in/yaml/v3"
)

// configFileMu serializes every in-process config.yaml writer. Previously the
// device, proxy and notification writers used unrelated locks and could replace
// each other's updates.
var configFileMu sync.Mutex

func readConfigDocument(data []byte) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse config file: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0] == nil {
		return nil, nil, fmt.Errorf("config file is empty")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("config root must be a mapping")
	}
	return &doc, root, nil
}

func patchConfigFile(path string, mutate func(root *yaml.Node) error) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}
	doc, root, err := readConfigDocument(data)
	if err != nil {
		return err
	}
	if err := mutate(root); err != nil {
		return err
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal config file: %w", err)
	}
	if err := atomicWriteConfigFile(path, out); err != nil {
		return err
	}
	return nil
}

// atomicWriteConfigFile fsyncs a same-directory temporary file before an
// atomic rename. Creating backups is deliberately the updater's responsibility.
func atomicWriteConfigFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary config file: %w", err)
	}
	tmpPath := tmp.Name()
	keepTemp := true
	defer func() {
		_ = tmp.Close()
		if keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary config file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config file: %w", err)
	}
	keepTemp = false

	// Windows does not support syncing directory handles. On Unix this closes
	// the final rename durability gap.
	if runtime.GOOS != "windows" {
		dirHandle, err := os.Open(dir)
		if err != nil {
			return fmt.Errorf("open config directory for sync: %w", err)
		}
		if err := dirHandle.Sync(); err != nil {
			_ = dirHandle.Close()
			return fmt.Errorf("sync config directory: %w", err)
		}
		if err := dirHandle.Close(); err != nil {
			return fmt.Errorf("close config directory: %w", err)
		}
	}
	return nil
}

func ensureMapping(parent *yaml.Node, key string) *yaml.Node {
	node := getMapValue(parent, key)
	if node == nil || node.Kind != yaml.MappingNode {
		node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMapNode(parent, key, node)
	}
	return node
}

type yamlFieldPatch struct {
	key   string
	value any
}

func patchMapping(parent *yaml.Node, key string, fields ...yamlFieldPatch) error {
	mapping := ensureMapping(parent, key)
	for _, field := range fields {
		node := &yaml.Node{}
		if err := node.Encode(field.value); err != nil {
			return fmt.Errorf("encode config field %s.%s: %w", key, field.key, err)
		}
		setMapNode(mapping, field.key, node)
	}
	return nil
}
