package updater

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPackagingShellSyntax(t *testing.T) {
	shell := "sh"
	if runtime.GOOS == "windows" {
		candidates := []string{
			`C:\Program Files\Git\usr\bin\sh.exe`,
			`C:\Program Files\Git\bin\bash.exe`,
		}
		shell = ""
		for _, candidate := range candidates {
			if _, err := os.Stat(candidate); err == nil {
				shell = candidate
				break
			}
		}
		if shell == "" {
			t.Skip("POSIX shell is unavailable")
		}
	}
	for _, name := range []string{"install.sh", "uninstall.sh"} {
		path := filepath.Join("..", "..", "packaging", name)
		if output, err := exec.Command(shell, "-n", path).CombinedOutput(); err != nil {
			t.Fatalf("%s syntax: %v\n%s", name, err, output)
		}
	}
}

func TestBootstrapTrustAndRepositoryAreFixed(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"REPOSITORY='zanescope/vohive'",
		"BOOTSTRAP_VERSION='@VOHIVE_BOOTSTRAP_VERSION@'",
		"@VOHIVE_MINISIGN_PUBLIC_KEYS@",
		"@VOHIVE_VERIFY_SHA256@",
		"vohive-verify_${BOOTSTRAP_VERSION}_linux_${ARCH}",
		"$RELEASE_BASE/download/$BOOTSTRAP_VERSION/$name",
		"--repair", "--dry-run", "--no-service",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("installer is missing %q", required)
		}
	}
	if strings.Contains(text, "vohive-verify_${VERSION}_linux_${ARCH}") {
		t.Fatal("installer binds its fallback verifier to the untrusted target release")
	}
	if strings.Contains(text, "iniwex") || strings.Contains(text, "admin123") || strings.Contains(text, "password: admin") {
		t.Fatal("installer contains a legacy repository or fixed password")
	}
}

func TestSystemdUnitKeepsConfigWritableWithoutHOMEOverride(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "systemd", "vohive.service"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "Environment=HOME=") {
		t.Fatal("unit overrides HOME")
	}
	if !strings.Contains(text, "ReadWritePaths=/etc/vohive /var/lib/vohive") {
		t.Fatal("unit does not keep managed config/data writable")
	}
}

func TestOpenWrtTemplateHasNoFixedPassword(t *testing.T) {
	path := filepath.Join("..", "..", "packaging", "openwrt", "vohive", "files", "config.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "password: admin") || strings.Contains(text, "password: admin123") {
		t.Fatal("OpenWrt template ships a fixed administrator password")
	}
}
