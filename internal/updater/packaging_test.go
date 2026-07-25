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

func TestDocumentedInstallerBootstrapPaths(t *testing.T) {
	readmeData, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(readmeData)
	for _, required := range []string{
		"releases/latest/download/vohive-install.sh",
		"sudo sh vohive-install.sh",
		"DEPLOYMENT.md#52-严格验证安装",
	} {
		if !strings.Contains(readme, required) {
			t.Errorf("README.md is missing quick-install content %q", required)
		}
	}
	if strings.Contains(readme, "gh attestation verify vohive-install.sh") {
		t.Fatal("README.md should keep strict provenance details in DEPLOYMENT.md")
	}

	deploymentData, err := os.ReadFile(filepath.Join("..", "..", "DEPLOYMENT.md"))
	if err != nil {
		t.Fatal(err)
	}
	deployment := string(deploymentData)
	for _, required := range []string{
		"VOHIVE_BOOTSTRAP_VERSION=v1.6.0",
		"gh attestation verify vohive-install.sh",
		"--repo zanescope/vohive",
		"--signer-workflow zanescope/vohive/.github/workflows/binary-release.yml",
		`--source-ref "refs/tags/${VOHIVE_BOOTSTRAP_VERSION}"`,
		"--deny-self-hosted-runners",
		"config_schema",
		"free_device_limit",
		"public_ip_probe.ipv4_urls",
		"public_ip_probe.ipv6_urls",
	} {
		if !strings.Contains(deployment, required) {
			t.Errorf("DEPLOYMENT.md is missing strict-install or configuration content %q", required)
		}
	}
	verify := strings.Index(deployment, "gh attestation verify vohive-install.sh")
	execute := -1
	if verify >= 0 {
		if offset := strings.Index(deployment[verify:], "sudo sh vohive-install.sh"); offset >= 0 {
			execute = verify + offset
		}
	}
	if verify < 0 || execute < 0 || verify > execute {
		t.Errorf("DEPLOYMENT.md must verify installer provenance before strict root execution: verify=%d execute=%d", verify, execute)
	}
}

func TestInstallerRequiresExplicitNoServiceMode(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		`if [ "$NO_SERVICE" -eq 1 ]; then SERVICE_TYPE='portable'`,
		"no supported service manager detected",
		"use --no-service only if you will start and monitor VoHive yourself",
		"files installed in explicit --no-service mode",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("installer service-manager preflight is missing %q", required)
		}
	}
	if strings.Contains(text, "else SERVICE_TYPE='portable'") {
		t.Fatal("installer still silently falls back to an unmanaged portable installation")
	}
	preflight := strings.LastIndex(text, "\ndetect_service\n")
	download := strings.Index(text, "\nif [ \"$REPAIR\" -eq 0 ] || [ \"$VERSION_SET\" -eq 1 ]; then")
	stateWrite := strings.Index(text, "\nmkdir -p \"$STATE_ROOT\"")
	if preflight < 0 || download < 0 || stateWrite < 0 || !(preflight < download && download < stateWrite) {
		t.Fatalf("service-manager preflight must precede downloads and state writes: service=%d download=%d state=%d", preflight, download, stateWrite)
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
