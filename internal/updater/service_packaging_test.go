package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllSystemdUnitsUseTransactionalManagedPaths(t *testing.T) {
	root := filepath.Join("..", "..", "packaging", "systemd")
	for _, name := range []string{"vohive.service", "vohive-update.service", "vohive-recover.service"} {
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
