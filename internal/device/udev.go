package device

import (
	"errors"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink/nl"
	"github.com/zanescope/vohive/pkg/logger"
	"golang.org/x/sys/unix"
)

const kernelUeventMulticastGroup = 1

// UdevWatcher 监听 USB 设备热插拔事件
type UdevWatcher struct {
	pool     *Pool
	stop     chan struct{}
	stopOnce sync.Once

	// 防抖相关
	debounce             time.Duration
	pending              bool
	pendingMu            sync.Mutex
	timer                *time.Timer
	pendingRecovery      map[string]modemRebootRecoveryWakeTarget
	pendingGeneralRescan bool
	timerEpoch           uint64
	beforeTimerCallback  func(uint64)
}

// NewUdevWatcher 创建 udev 监听器
func NewUdevWatcher(pool *Pool) *UdevWatcher {
	return &UdevWatcher{
		pool:            pool,
		stop:            make(chan struct{}),
		debounce:        3 * time.Second, // 等待设备枚举完成
		pendingRecovery: make(map[string]modemRebootRecoveryWakeTarget),
	}
}

// Start 启动 udev 事件监听
func (w *UdevWatcher) Start() {
	select {
	case <-w.stop:
		return
	default:
	}
	go w.loop()
}

// Stop 停止监听
func (w *UdevWatcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stop)
		w.pendingMu.Lock()
		if w.timer != nil {
			w.timer.Stop()
		}
		w.timer = nil
		w.timerEpoch++
		w.pending = false
		clear(w.pendingRecovery)
		w.pendingGeneralRescan = false
		w.pendingMu.Unlock()
	})
}

func (w *UdevWatcher) loop() {
	// 创建 netlink 连接监听内核 uevent
	conn, err := nl.Subscribe(unix.NETLINK_KOBJECT_UEVENT, kernelUeventMulticastGroup)
	if err != nil {
		logger.Warn("udev 监听器启动失败，热插拔功能不可用", "err", err)
		return
	}
	defer conn.Close()

	logger.Info("udev 设备热插拔监听器已启动")

	for {
		select {
		case <-w.stop:
			logger.Info("udev 监听器已停止")
			return
		default:
		}

		// 设置读取超时，以便定期检查 stop 信号
		tv := unix.NsecToTimeval((1 * time.Second).Nanoseconds())
		_ = conn.SetReceiveTimeout(&tv)

		msgs, _, err := conn.Receive()
		if err != nil {
			// 超时错误是正常的
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				continue
			}
			// 其他错误记录但继续
			continue
		}

		for _, msg := range msgs {
			if event, ok := parseModemUevent(msg.Data); ok {
				logger.Debug("检测到调制解调器相关 udev 事件", "data_preview", truncateString(string(msg.Data), 200))
				w.scheduleModemEvent(event)
			}
		}
	}
}

type modemUevent struct {
	Action    string
	Subsystem string
	DevPath   string
	DevName   string
	Interface string
}

func parseModemUevent(data []byte) (modemUevent, bool) {
	var event modemUevent
	fields := strings.FieldsFunc(string(data), func(r rune) bool {
		return r == '\x00' || r == '\n'
	})
	for index, field := range fields {
		field = strings.TrimSpace(field)
		if index == 0 {
			if action, devPath, ok := strings.Cut(field, "@"); ok {
				event.Action = strings.ToLower(strings.TrimSpace(action))
				event.DevPath = strings.TrimSpace(devPath)
				continue
			}
		}
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(key)) {
		case "ACTION":
			event.Action = strings.ToLower(strings.TrimSpace(value))
		case "SUBSYSTEM":
			event.Subsystem = strings.ToLower(strings.TrimSpace(value))
		case "DEVPATH":
			event.DevPath = strings.TrimSpace(value)
		case "DEVNAME":
			event.DevName = strings.TrimSpace(value)
		case "INTERFACE":
			event.Interface = strings.TrimSpace(value)
		}
	}
	if event.Action != "add" && event.Action != "remove" {
		return modemUevent{}, false
	}
	switch event.Subsystem {
	case "usb", "usbmisc", "wwan":
	case "net":
		name := strings.TrimSpace(event.Interface)
		if name == "" {
			name = path.Base(strings.TrimSpace(event.DevPath))
		}
		if !strings.HasPrefix(name, "wwan") {
			return modemUevent{}, false
		}
	case "tty":
		name := path.Base(strings.TrimSpace(event.DevName))
		if name == "." || name == "/" || name == "" {
			name = path.Base(strings.TrimSpace(event.DevPath))
		}
		if !strings.HasPrefix(name, "ttyUSB") {
			return modemUevent{}, false
		}
	default:
		return modemUevent{}, false
	}
	return event, true
}

// isModemEvent 检查是否是 USB 调制解调器相关事件
func (w *UdevWatcher) isModemEvent(data []byte) bool {
	_, ok := parseModemUevent(data)
	return ok
}

func (w *UdevWatcher) scheduleModemEvent(event modemUevent) {
	if w == nil {
		return
	}
	if w.pool != nil {
		if target, ok := w.pool.modemRebootRecoveryTargetForUevent(event); ok {
			w.scheduleRescanForTargets([]modemRebootRecoveryWakeTarget{target}, false)
			return
		}
	}
	w.scheduleRescanForTargets(nil, true)
}

// scheduleRescan 防抖：延迟执行扫描
// 每批事件绑定独立 epoch；旧 callback 不能清空或提前执行新一批事件。
func (w *UdevWatcher) scheduleRescan() {
	w.scheduleRescanForTargets(nil, true)
}

func (w *UdevWatcher) scheduleRescanForTargets(targets []modemRebootRecoveryWakeTarget, generalRescan bool) {
	select {
	case <-w.stop:
		return
	default:
	}
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	if w.pendingRecovery == nil {
		w.pendingRecovery = make(map[string]modemRebootRecoveryWakeTarget)
	}
	for _, target := range targets {
		if target.deviceID = strings.TrimSpace(target.deviceID); target.deviceID != "" && target.wakeCh != nil {
			w.pendingRecovery[target.deviceID] = target
		}
	}
	if generalRescan {
		w.pendingGeneralRescan = true
	}

	w.timerEpoch++
	epoch := w.timerEpoch
	if w.timer != nil {
		w.timer.Stop()
	}
	w.pending = true
	debounce := w.debounce
	w.timer = time.AfterFunc(debounce, func() {
		if hook := w.beforeTimerCallback; hook != nil {
			hook(epoch)
		}
		select {
		case <-w.stop:
			return
		default:
		}
		w.pendingMu.Lock()
		if epoch != w.timerEpoch {
			w.pendingMu.Unlock()
			return
		}
		w.pending = false
		w.timer = nil
		recoveryTargets := make([]modemRebootRecoveryWakeTarget, 0, len(w.pendingRecovery))
		for _, target := range w.pendingRecovery {
			recoveryTargets = append(recoveryTargets, target)
		}
		clear(w.pendingRecovery)
		generalRescan := w.pendingGeneralRescan
		w.pendingGeneralRescan = false
		w.pendingMu.Unlock()
		select {
		case <-w.stop:
			return
		default:
		}

		logger.Info("udev 检测到设备变化，执行重新扫描")
		if w.pool == nil {
			return
		}
		woken := 0
		for _, target := range recoveryTargets {
			if w.pool.wakeModemRebootRecoveryTarget(target, "udev_modem_event") {
				woken++
			} else {
				generalRescan = true
			}
		}
		if woken > 0 {
			logger.Debug("udev 事件已唤醒对应模组重启恢复流程", "recoveries", woken)
		}
		if generalRescan || woken == 0 {
			w.pool.scheduleRescan("udev")
		}
	})
}

func modemUeventMatchesRecoveryIdentity(event modemUevent, identity modemRebootRecoveryIdentity) bool {
	eventUSB := usbTopologyKey(event.DevPath)
	identityUSB := usbTopologyKey(identity.USBPath)
	if eventUSB != "" || identityUSB != "" {
		return eventUSB != "" && identityUSB != "" && eventUSB == identityUSB
	}
	eventNames := modemUeventDeviceNames(event)
	for _, candidate := range []string{
		identity.ControlDevice,
		identity.QMIDevice,
		identity.Interface,
		identity.ATPort,
		identity.ManagePort,
	} {
		name := strings.ToLower(path.Base(strings.TrimSpace(candidate)))
		if name == "" || name == "." || name == "/" {
			continue
		}
		if _, ok := eventNames[name]; ok {
			return true
		}
	}
	return false
}

func modemUeventDeviceNames(event modemUevent) map[string]struct{} {
	names := make(map[string]struct{})
	add := func(value string) {
		value = strings.ToLower(path.Base(strings.TrimSpace(value)))
		if value != "" && value != "." && value != "/" {
			names[value] = struct{}{}
		}
	}
	add(event.DevName)
	add(event.Interface)
	add(event.DevPath)
	for _, segment := range strings.Split(strings.ReplaceAll(event.DevPath, "\\", "/"), "/") {
		if strings.HasPrefix(segment, "wwan") ||
			strings.HasPrefix(segment, "ttyUSB") ||
			strings.HasPrefix(segment, "cdc-wdm") {
			add(segment)
		}
	}
	return names
}

func usbTopologyKey(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	segments := strings.Split(value, "/")
	for index := len(segments) - 1; index >= 0; index-- {
		segment := strings.TrimSpace(segments[index])
		if colon := strings.IndexByte(segment, ':'); colon >= 0 {
			segment = segment[:colon]
		}
		parts := strings.SplitN(segment, "-", 2)
		if len(parts) != 2 || !allDecimal(parts[0], false) || !allDecimal(parts[1], true) {
			continue
		}
		return segment
	}
	return ""
}

func allDecimal(value string, allowDot bool) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r >= '0' && r <= '9' {
			continue
		}
		if allowDot && r == '.' {
			continue
		}
		return false
	}
	return true
}

// truncateString 截断字符串用于日志
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
