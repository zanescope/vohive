package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"aead.dev/minisign"
)

// TrustedMinisignPublicKeys is populated by the release build with -ldflags.
// It may contain the current and next public key separated by a semicolon. An
// empty value deliberately fails closed; no development or placeholder key is
// trusted by default.
var TrustedMinisignPublicKeys string

type SignatureVerifier interface {
	Verify(message, signature []byte) error
}

type MinisignVerifier struct {
	keys []minisign.PublicKey
}

func NewMinisignVerifier(encodedKeys string) (*MinisignVerifier, error) {
	var keys []minisign.PublicKey
	for _, encoded := range publicKeyLines(encodedKeys) {
		var key minisign.PublicKey
		if err := key.UnmarshalText([]byte(encoded)); err != nil {
			return nil, fmt.Errorf("parse minisign public key: %w", err)
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, ErrSignatureUnavailable
	}
	return &MinisignVerifier{keys: keys}, nil
}

func NewMinisignVerifierFromFile(path string) (*MinisignVerifier, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return NewMinisignVerifier(string(data))
}

func DefaultSignatureVerifier() (SignatureVerifier, error) {
	if strings.TrimSpace(TrustedMinisignPublicKeys) != "" {
		return NewMinisignVerifier(TrustedMinisignPublicKeys)
	}
	verifier, err := NewMinisignVerifierFromFile("/etc/vohive/update.pub")
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrSignatureUnavailable
	}
	return verifier, err
}

func (v *MinisignVerifier) Verify(message, signature []byte) error {
	if v == nil || len(v.keys) == 0 {
		return ErrSignatureUnavailable
	}
	for _, key := range v.keys {
		if minisign.Verify(key, message, signature) {
			return nil
		}
	}
	return errors.New("minisign signature verification failed")
}

func publicKeyLines(value string) []string {
	value = strings.ReplaceAll(value, ";", "\n")
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(strings.ToLower(line), "untrusted comment:") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func VerifyFileSHA256(path, expected string) error {
	expected = strings.ToLower(strings.TrimSpace(expected))
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("invalid SHA-256 value %q", expected)
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return fmt.Errorf("invalid SHA-256 value: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return fmt.Errorf("SHA-256 mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}
