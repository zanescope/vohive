package updater

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

const (
	readinessKeyBytes   = 32
	readinessProofLabel = "vohive-readiness-v1"
)

type ReadyExpectation struct {
	Endpoint        string
	ExpectedVersion string
	KeyFile         string
	AllowLegacy     bool
}

type ReadinessResponse struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Proof   string `json:"proof,omitempty"`
}

func ResolveReadyURL(deployment Deployment) (string, error) {
	return ResolveProbeURL(deployment.ConfigPath, "/readyz")
}

func ResolveProbeURL(configPath, endpointPath string) (string, error) {
	if strings.TrimSpace(configPath) == "" {
		return "", errors.New("config path is required to resolve the service port")
	}
	if endpointPath == "" || !strings.HasPrefix(endpointPath, "/") {
		return "", fmt.Errorf("probe endpoint path must be absolute: %q", endpointPath)
	}
	portValue, err := configuredServerPort(configPath)
	if err != nil {
		return "", err
	}
	port, err := listenPort(portValue)
	if err != nil {
		return "", err
	}
	endpoint := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort("127.0.0.1", port),
		Path:   endpointPath,
	}
	return endpoint.String(), nil
}

func configuredServerPort(configPath string) (string, error) {
	if value := strings.TrimSpace(os.Getenv("VOHIVE_SERVER_PORT")); value != "" {
		return value, nil
	}
	if value := strings.TrimSpace(os.Getenv("PROXY_SERVER_PORT")); value != "" {
		return value, nil
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("read configured service port: %w", err)
	}
	var document struct {
		Server struct {
			Port yaml.Node `yaml:"port"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return "", fmt.Errorf("decode configured service port: %w", err)
	}
	if document.Server.Port.Kind == 0 {
		return "7575", nil
	}
	if document.Server.Port.Kind != yaml.ScalarNode {
		return "", errors.New("server.port must be a scalar")
	}
	return strings.TrimSpace(document.Server.Port.Value), nil
}

func listenPort(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "7575"
	}
	port := value
	if strings.HasPrefix(value, ":") && !strings.HasPrefix(value, "::") {
		port = strings.TrimPrefix(value, ":")
	} else if strings.Contains(value, ":") {
		host, parsedPort, err := net.SplitHostPort(value)
		if err != nil {
			return "", fmt.Errorf("invalid server.port %q: %w", value, err)
		}
		host = strings.Trim(strings.TrimSpace(host), "[]")
		switch strings.ToLower(host) {
		case "", "0.0.0.0", "::", "localhost", "127.0.0.1":
		default:
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				return "", fmt.Errorf("server.port host %q is not loopback or wildcard; local readiness cannot be verified safely", host)
			}
		}
		port = parsedPort
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", fmt.Errorf("invalid server.port %q", value)
	}
	return strconv.Itoa(number), nil
}

func RotateReadinessKey(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("readiness key path is required")
	}
	key := make([]byte, readinessKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generate readiness key: %w", err)
	}
	encoded := make([]byte, hex.EncodedLen(len(key))+1)
	hex.Encode(encoded, key)
	encoded[len(encoded)-1] = '\n'
	if err := atomicWriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write readiness key: %w", err)
	}
	return nil
}

func ReadinessKeyPath() string {
	if path := strings.TrimSpace(os.Getenv(ReadinessKeyFileEnv)); path != "" {
		return path
	}
	return DefaultReadinessKey
}

func SignReadinessChallenge(keyFile, version, challenge string) (string, string, error) {
	canonicalVersion := normalizeVersion(version)
	if !validVersion(canonicalVersion) {
		return "", "", fmt.Errorf("invalid runtime version %q", version)
	}
	if err := validateChallenge(challenge); err != nil {
		return "", "", err
	}
	key, err := loadReadinessKey(keyFile)
	if err != nil {
		return "", "", err
	}
	return canonicalVersion, readinessProof(key, canonicalVersion, challenge), nil
}

func VerifyReadinessProof(keyFile, version, challenge, proof string) error {
	canonicalVersion := normalizeVersion(version)
	if !validVersion(canonicalVersion) {
		return fmt.Errorf("invalid readiness version %q", version)
	}
	if err := validateChallenge(challenge); err != nil {
		return err
	}
	key, err := loadReadinessKey(keyFile)
	if err != nil {
		return err
	}
	expected, err := hex.DecodeString(readinessProof(key, canonicalVersion, challenge))
	if err != nil {
		return err
	}
	actual, err := hex.DecodeString(strings.TrimSpace(proof))
	if err != nil || len(actual) != sha256.Size || !hmac.Equal(actual, expected) {
		return errors.New("readiness proof does not match the managed service")
	}
	return nil
}

func NewReadinessChallenge() (string, error) {
	challenge := make([]byte, readinessKeyBytes)
	if _, err := rand.Read(challenge); err != nil {
		return "", fmt.Errorf("generate readiness challenge: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(challenge), nil
}

func validateChallenge(challenge string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(challenge))
	if err != nil || len(decoded) != readinessKeyBytes {
		return errors.New("invalid readiness challenge")
	}
	return nil
}

func loadReadinessKey(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("readiness key path is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read readiness key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("readiness key is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("readiness key permissions allow group or other access")
	}
	if info.Size() > 1024 {
		return nil, errors.New("readiness key is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read readiness key: %w", err)
	}
	data = bytes.TrimSpace(data)
	decoded := make([]byte, hex.DecodedLen(len(data)))
	n, err := hex.Decode(decoded, data)
	if err != nil || n != readinessKeyBytes {
		return nil, errors.New("readiness key has an invalid format")
	}
	return decoded[:n], nil
}

func readinessProof(key []byte, version, challenge string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%s\n%s\n%s", readinessProofLabel, version, challenge)
	return hex.EncodeToString(mac.Sum(nil))
}
