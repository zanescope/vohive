package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerBindsVerifierToBootstrapRelease(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"BOOTSTRAP_VERSION='@VOHIVE_BOOTSTRAP_VERSION@'",
		"vohive-verify_${BOOTSTRAP_VERSION}_linux_${ARCH}",
		"$RELEASE_BASE/download/$BOOTSTRAP_VERSION/$name",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("installer bootstrap verifier contract is missing %q", required)
		}
	}
	if strings.Contains(text, "vohive-verify_${VERSION}_linux_${ARCH}") {
		t.Fatal("verifier is incorrectly bound to the user-selected target version")
	}
}

func TestInstallerStagesBeforeStoppingAndHasTransactionTrap(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	stage := strings.Index(text, "STAGING_DIR=\"$INSTALL_ROOT/releases/.staging-")
	stop := strings.LastIndex(text, "stop_service_best_effort || die")
	activate := strings.Index(text, "TRANSACTION_ACTIVE=1")
	if stage < 0 || stop < 0 || activate < 0 || stage > activate || activate > stop {
		t.Fatalf("expected complete staging and transaction activation before stop; stage=%d active=%d stop=%d", stage, activate, stop)
	}
	for _, required := range []string{"rollback_transaction", "trap cleanup EXIT", "mv \"$STAGING_DIR\" \"$RELEASE_DIR\""} {
		if !strings.Contains(text, required) {
			t.Errorf("transactional installer is missing %q", required)
		}
	}
}

func TestInstallerValidatesAllVerifierHashes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "for wanted_arch in amd64 arm64 armv7") {
		t.Fatal("installer does not validate the complete verifier hash map")
	}
}
