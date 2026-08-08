package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllSystemdUnitsUseTransactionalManagedPaths(t *testing.T) {
	root := filepath.Join("..", "..", "packaging", "systemd")
	for _, name := range []string{"vohive.service", "vohive-update.service", "vohive-recover.service", "vohive-host-config.service"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if strings.Contains(text, "Environment=HOME=") {
			t.Fatalf("%s overrides HOME", name)
		}
	}
	for _, name := range []string{"vohive-update.service", "vohive-recover.service"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, "ProtectSystem=full") ||
			!strings.Contains(text, "ReadWritePaths=/opt/vohive /etc/vohive /var/lib/vohive") {
			t.Fatalf("%s does not confine writes to managed transaction paths", name)
		}
	}
	recoverData, err := os.ReadFile(filepath.Join(root, "vohive-recover.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recoverData), "vohivectl recover --boot") {
		t.Fatal("systemd recovery unit is not explicitly boot-authorized")
	}
	hostConfigData, err := os.ReadFile(filepath.Join(root, "vohive-host-config.service"))
	if err != nil {
		t.Fatal(err)
	}
	hostConfig := string(hostConfigData)
	normalizedHostConfig := strings.ReplaceAll(hostConfig, "\r\n", "\n")
	hostConfigDirectives := "\n" + strings.TrimSuffix(normalizedHostConfig, "\n") + "\n"
	for _, required := range []string{
		"ProtectSystem=strict",
		"ReadOnlyPaths=/sys",
		"PrivateDevices=true",
		"CapabilityBoundingSet=",
		"AmbientCapabilities=",
		"RestrictSUIDSGID=true",
		"LockPersonality=true",
		"ProtectKernelModules=true",
		"ProtectKernelLogs=true",
		"ProtectClock=true",
		"ExecStart=/opt/vohive/control/vohivectl host-config --request /var/lib/vohive/host-config/request.json",
	} {
		if !strings.Contains(hostConfigDirectives, "\n"+required+"\n") {
			t.Errorf("host-config unit is missing %q", required)
		}
	}
	const hostConfigWritePaths = "ReadWritePaths=/etc/udev/rules.d /var/lib/vohive/host-config /var/lib/vohive/update"
	if strings.Count(normalizedHostConfig, "ReadWritePaths=") != 1 ||
		!strings.Contains(hostConfigDirectives, "\n"+hostConfigWritePaths+"\n") {
		t.Fatalf("host-config unit must grant exactly its three managed write paths:\n%s", hostConfig)
	}
	installer := readPackagingScript(t, "install.sh")
	const heredocStart = "cat >/etc/systemd/system/vohive-host-config.service <<'EOF'\n"
	start := strings.Index(installer, heredocStart)
	if start < 0 {
		t.Fatal("installer host-config unit heredoc is missing")
	}
	start += len(heredocStart)
	end := strings.Index(installer[start:], "\nEOF")
	if end < 0 {
		t.Fatal("installer host-config unit heredoc is unterminated")
	}
	generatedHostConfig := strings.ReplaceAll(installer[start:start+end], "\r\n", "\n")
	packagedHostConfig := strings.TrimSuffix(normalizedHostConfig, "\n")
	if generatedHostConfig != packagedHostConfig {
		t.Fatal("installer host-config unit must exactly match the packaged static unit")
	}
	mainData, err := os.ReadFile(filepath.Join(root, "vohive.service"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mainData), "/etc/udev/rules.d") {
		t.Fatal("main service must not be able to write udev rules")
	}
}

func TestOpenWrtUnitsMatchTransactionalLayout(t *testing.T) {
	root := filepath.Join("..", "..", "packaging", "openwrt", "vohive", "files")
	files := make(map[string]string)
	for _, name := range []string{"vohive.init", "vohive-update.init", "vohive-recover.init"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(data)
		if strings.Contains(files[name], "HOME=") || strings.Contains(files[name], "env HOME") {
			t.Fatalf("%s overrides HOME", name)
		}
	}
	if !strings.Contains(files["vohive.init"], "/opt/vohive/current/vohive") {
		t.Fatal("OpenWrt main service bypasses the transactional current pointer")
	}
	if strings.Contains(files["vohive-update.init"], "START=") {
		t.Fatal("OpenWrt updater must not race the main service during boot")
	}
	if !strings.Contains(files["vohive-recover.init"], "recover --boot") {
		t.Fatal("OpenWrt recovery unit is not explicitly boot-authorized")
	}
}
