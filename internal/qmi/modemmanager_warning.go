package qmicore

import (
	"fmt"
	"strings"
	"time"

	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/pkg/logger"
)

func warnModemManagerQMIConflict(cfg config.DeviceConfig, decision qmiTransportDecision) {
	if len(decision.ModemManagerHolders) == 0 {
		return
	}
	controlDevice := strings.TrimSpace(cfg.ControlDevice)
	if controlDevice == "" {
		controlDevice = strings.TrimSpace(cfg.QMIDevice)
	}
	for _, holder := range decision.ModemManagerHolders {
		logger.WarnRate(
			fmt.Sprintf("qmi_modemmanager_conflict:%s:%d", controlDevice, holder.PID),
			10*time.Minute,
			"检测到 ModemManager 所属进程占用 VoHive QMI 控制口；请隔离设备所有权，避免 qmi-proxy 随 ModemManager 退出而中断",
			"qmi_modemmanager_conflict", true,
			"device", strings.TrimSpace(cfg.ID),
			"control_device", controlDevice,
			"holder_pid", holder.PID,
			"holder_command", holder.Command,
			"holder_cgroup", holder.Cgroup,
			"conflict_reason", holder.ModemManagerOwnerReason,
			"action", "isolate_modemmanager_from_vohive_devices",
		)
	}
}
