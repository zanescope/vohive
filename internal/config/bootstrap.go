package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	yaml "go.yaml.in/yaml/v3"
)

const InitialAdminUsername = "admin"

type InitialAdminCredentials struct {
	Username string
	Password string
}

var pendingInitialCredentials struct {
	sync.Mutex
	value *InitialAdminCredentials
}

// EnsureConfigFile creates a secure minimal config only when the requested file
// does not exist. Existing files, including invalid files, are never replaced.
func EnsureConfigFile(path string) (*InitialAdminCredentials, error) {
	configFileMu.Lock()
	defer configFileMu.Unlock()

	if _, err := os.Stat(path); err == nil {
		return nil, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat config file: %w", err)
	}

	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create private config directory: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat config directory: %w", err)
	}
	password, err := generateInitialAdminPassword()
	if err != nil {
		return nil, err
	}
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setMapInt(root, "config_schema", CurrentConfigSchema)
	server := ensureMapping(root, "server")
	setMapInt(server, "port", 7575)
	setMapBool(server, "debug", false)
	setMapInt(root, "free_device_limit", DefaultConfig().FreeDeviceLimit)
	vowifi := ensureMapping(root, "vowifi")
	setMapBool(vowifi, "enabled", false)
	web := ensureMapping(root, "web")
	setMapScalar(web, "username", InitialAdminUsername)
	setMapScalar(web, "password", password)

	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal initial config: %w", err)
	}
	if err := atomicWriteConfigFile(path, data); err != nil {
		return nil, err
	}
	credentials := &InitialAdminCredentials{
		Username: InitialAdminUsername,
		Password: password,
	}
	pendingInitialCredentials.Lock()
	pendingInitialCredentials.value = credentials
	pendingInitialCredentials.Unlock()
	return credentials, nil
}

// ConsumeInitialAdminCredentials returns credentials exactly once so the
// startup path can print a console-only banner before file logging starts.
func ConsumeInitialAdminCredentials() *InitialAdminCredentials {
	pendingInitialCredentials.Lock()
	defer pendingInitialCredentials.Unlock()
	credentials := pendingInitialCredentials.value
	pendingInitialCredentials.value = nil
	return credentials
}

func generateInitialAdminPassword() (string, error) {
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate initial administrator password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}
