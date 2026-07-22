package updater

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"aead.dev/minisign"
)

func TestMinisignVerifierAndSHA256(t *testing.T) {
	publicKey, privateKey, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewMinisignVerifier(publicKey.String())
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("signed release manifest")
	signature := minisign.Sign(privateKey, message)
	if err := verifier.Verify(message, signature); err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify([]byte("tampered"), signature); err == nil {
		t.Fatal("tampered message was accepted")
	}
	path := filepath.Join(t.TempDir(), "asset")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFileSHA256(path, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFileSHA256(path, "aa7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"); err == nil {
		t.Fatal("incorrect hash was accepted")
	}
}
