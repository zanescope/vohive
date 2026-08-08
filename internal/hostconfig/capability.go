package hostconfig

import (
	"os"
	"os/exec"
	"runtime"
)

const defaultHelperUnitPath = "/etc/systemd/system/vohive-host-config.service"

type Capability struct {
	Supported bool
	Reason    string
}

func ProbeCapability() Capability {
	if runtime.GOOS != "linux" {
		return Capability{Reason: "仅原生 Linux systemd 安装支持网页管理"}
	}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return Capability{Reason: "容器部署必须在宿主机配置 ModemManager 隔离"}
		}
	}
	if os.Geteuid() != 0 {
		return Capability{Reason: "VoHive 服务未以所需的主机权限运行"}
	}
	if info, err := os.Lstat(defaultHelperUnitPath); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Capability{Reason: "当前安装缺少受限的主机配置 helper；从旧版本升级时，请使用当前版本的签名安装器执行 sudo sh vohive-install.sh --repair"}
	}
	if info, err := os.Stat("/etc/udev/rules.d"); err != nil || !info.IsDir() {
		return Capability{Reason: "主机没有可用的 udev 规则目录"}
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return Capability{Reason: "当前部署不是可管理的 systemd 安装"}
	}
	if _, err := exec.LookPath("udevadm"); err != nil {
		return Capability{Reason: "主机缺少 udevadm，无法安全加载规则"}
	}
	return Capability{Supported: true}
}
