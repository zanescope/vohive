package notify

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/zanescope/vohive/internal/db"
	"github.com/zanescope/vohive/internal/device"
	"github.com/zanescope/vohive/internal/modem"
	"github.com/zanescope/vowifi-go/runtimehost/messaging"
)

const defaultSIMCheckTimeout = 90 * time.Second

type simCheckCarrier struct {
	Name          string
	ServiceNumber string
	Query         string
}

type simCheckSnapshot struct {
	DeviceID      string
	DeviceDisplay string
	ICCID         string
	Home          string
	Network       string
	Data          string
	DataConfirmed bool
	DataEnabled   bool
	Registered    bool
	VoWiFi        bool
}

type pendingSIMCheck struct {
	snapshot simCheckSnapshot
	carrier  simCheckCarrier
	cmdCtx   CommandContext
	timer    *time.Timer
}

var mainlandSIMCheckCarriers = map[string]simCheckCarrier{
	"46000": {Name: "中国移动", ServiceNumber: "10086", Query: "YE"},
	"46002": {Name: "中国移动", ServiceNumber: "10086", Query: "YE"},
	"46004": {Name: "中国移动", ServiceNumber: "10086", Query: "YE"},
	"46007": {Name: "中国移动", ServiceNumber: "10086", Query: "YE"},
	"46008": {Name: "中国移动", ServiceNumber: "10086", Query: "YE"},
	"46013": {Name: "中国移动", ServiceNumber: "10086", Query: "YE"},
	"46001": {Name: "中国联通", ServiceNumber: "10010", Query: "YE"},
	"46006": {Name: "中国联通", ServiceNumber: "10010", Query: "YE"},
	"46009": {Name: "中国联通", ServiceNumber: "10010", Query: "YE"},
	"46003": {Name: "中国电信", ServiceNumber: "10001", Query: "YE"},
	"46005": {Name: "中国电信", ServiceNumber: "10001", Query: "YE"},
	"46011": {Name: "中国电信", ServiceNumber: "10001", Query: "YE"},
}

func (m *Manager) handleCmdSIMCheck(cmdCtx CommandContext, args []string) string {
	if len(args) > 1 {
		return commandUsageBlock("SIM 检测", "/simcheck [设备ID]", "/simcheck wwan9")
	}
	if m == nil || m.pool == nil {
		return commandEmptyBlock("SIM 检测", "没有可用设备")
	}

	var worker *device.Worker
	if len(args) == 1 {
		deviceID := strings.TrimSpace(args[0])
		worker = m.pool.GetWorker(deviceID)
		if worker == nil {
			return commandFailureBlock("SIM 检测", deviceID, "设备未找到")
		}
	} else {
		workers := m.pool.GetAllWorkers()
		switch len(workers) {
		case 0:
			return commandEmptyBlock("SIM 检测", "没有可用设备")
		case 1:
			worker = workers[0]
		default:
			return commandUsageBlock("SIM 检测", "/simcheck [设备ID]", "/simcheck wwan9")
		}
	}
	if worker == nil {
		return commandEmptyBlock("SIM 检测", "没有可用设备")
	}
	if m.pool.IsESIMSwitching(worker.ID) {
		return commandFailureBlock("SIM 检测", worker.ID, "eSIM 正在切换，请稍后重试")
	}

	status := worker.GetCachedDeviceStatus()
	isVoWiFi := m.pool.IsVoWiFiActive(worker.ID)
	snapshot := buildSIMCheckSnapshot(worker, status, isVoWiFi)
	if snapshot.ICCID == "" {
		return formatSIMCheckUnavailable(snapshot, "未检测到当前 SIM/eSIM 身份", "未发送", "无法判断是否欠费")
	}

	carrier, supported := resolveSIMCheckCarrier(status)
	if !supported {
		return formatUnsupportedSIMCheck(snapshot)
	}
	snapshot.Home = carrier.Name

	if isVoWiFi {
		state, ok := m.pool.GetVoWiFiRuntimeState(worker.ID)
		if !ok || !state.SMSReady {
			return formatSIMCheckUnavailable(snapshot, "VoWiFi 短信通道未就绪", "未发送", "无法判断是否欠费")
		}
	} else if !snapshot.Registered {
		return formatSIMCheckUnavailable(snapshot, "当前未驻网", "未发送", "可能是信号、卡状态或欠费，暂时无法判断")
	}

	pending := &pendingSIMCheck{
		snapshot: snapshot,
		carrier:  carrier,
		cmdCtx:   cmdCtx,
	}
	if !m.reservePendingSIMCheck(pending) {
		return fmt.Sprintf("SIM 检测 / 进行中\n设备    %s\n状态    已有查询正在等待运营商回复", snapshot.DeviceDisplay)
	}

	go m.sendSIMCheckQuery(worker, pending)
	return fmt.Sprintf(
		"SIM 检测 / 已受理\n设备    %s\nSIM     %s\n归属    %s\n网络注册  %s\n数据网络  %s\n查询    正在发送余额查询",
		snapshot.DeviceDisplay,
		maskedCommandICCID(snapshot.ICCID),
		snapshot.Home,
		snapshot.Network,
		snapshot.Data,
	)
}

func buildSIMCheckSnapshot(worker *device.Worker, status modem.DeviceStatus, isVoWiFi bool) simCheckSnapshot {
	deviceID := ""
	display := ""
	dataEnabled := false
	publicIP := ""
	if worker != nil {
		deviceID = strings.TrimSpace(worker.ID)
		display = deviceID
		if name := strings.TrimSpace(worker.Config.Name); name != "" {
			display = fmt.Sprintf("%s (%s)", name, deviceID)
		}
		dataEnabled = worker.Config.NetworkEnabled
		publicIP = firstNonEmpty(worker.GetCachedIP(), worker.GetCachedIPv6())
	}

	dataStatus := "未启用"
	dataConfirmed := false
	switch {
	case dataEnabled && publicIP != "":
		dataStatus = fmt.Sprintf("已连接（%s）", publicIP)
		dataConfirmed = true
	case dataEnabled:
		dataStatus = "已启用，当前未取得公网 IP"
	case isVoWiFi:
		dataStatus = "蜂窝数据未启用（VoWiFi 通道在线）"
	}

	carrier, supported := resolveSIMCheckCarrier(status)
	home := carrier.Name
	if !supported {
		scope := simCheckCardScope(status)
		if spn := strings.TrimSpace(status.NativeSPN); spn != "" {
			home = fmt.Sprintf("%s（%s）", spn, scope)
		} else {
			home = scope
		}
	}

	return simCheckSnapshot{
		DeviceID:      deviceID,
		DeviceDisplay: commandValueOrDash(display),
		ICCID:         strings.TrimSpace(status.ICCID),
		Home:          home,
		Network:       simCheckRegistrationText(status.RegStatus, status.RegStatusText),
		Data:          dataStatus,
		DataConfirmed: dataConfirmed,
		DataEnabled:   dataEnabled,
		Registered:    status.RegStatus == 1 || status.RegStatus == 5,
		VoWiFi:        isVoWiFi,
	}
}

func resolveSIMCheckCarrier(status modem.DeviceStatus) (simCheckCarrier, bool) {
	mcc := digitsOnly(status.NativeMCC)
	mnc := digitsOnly(status.NativeMNC)
	if mcc != "" && mnc != "" {
		if len(mnc) == 1 {
			mnc = "0" + mnc
		}
		carrier, ok := mainlandSIMCheckCarriers[mcc+mnc]
		if !ok && len(mnc) == 3 && strings.HasPrefix(mnc, "0") {
			carrier, ok = mainlandSIMCheckCarriers[mcc+mnc[1:]]
		}
		return carrier, ok
	}

	imsi := digitsOnly(status.IMSI)
	if len(imsi) < 5 {
		return simCheckCarrier{}, false
	}
	carrier, ok := mainlandSIMCheckCarriers[imsi[:5]]
	return carrier, ok
}

func simCheckCardScope(status modem.DeviceStatus) string {
	mcc := digitsOnly(status.NativeMCC)
	if mcc == "" {
		imsi := digitsOnly(status.IMSI)
		if len(imsi) >= 3 {
			mcc = imsi[:3]
		}
	}
	switch mcc {
	case "460":
		return "中国大陆 SIM/eSIM"
	case "454":
		return "香港 SIM/eSIM"
	case "455":
		return "澳门 SIM/eSIM"
	case "":
		return "归属未知的 SIM/eSIM"
	default:
		return "境外 SIM/eSIM"
	}
}

func simCheckRegistrationText(code int, text string) string {
	if text = strings.TrimSpace(text); text != "" {
		return text
	}
	switch code {
	case 1:
		return "已注册（本地）"
	case 2:
		return "搜索中"
	case 3:
		return "注册被拒"
	case 5:
		return "已注册（漫游）"
	default:
		return "未注册"
	}
}

func formatUnsupportedSIMCheck(snapshot simCheckSnapshot) string {
	conclusion := "当前未驻网；原因可能包括信号、卡状态或欠费，无法直接判断"
	switch {
	case snapshot.Registered && snapshot.DataConfirmed:
		conclusion = "网络与数据链路当前可用；余额状态无法通过通用短信确认"
	case snapshot.Registered && snapshot.DataEnabled:
		conclusion = "当前已驻网；数据链路尚未确认，余额状态未知"
	case snapshot.Registered:
		conclusion = "当前已驻网；数据未启用，余额状态未知"
	case snapshot.VoWiFi:
		conclusion = "VoWiFi 通道在线；蜂窝驻网和余额状态仍无法确认"
	}
	return fmt.Sprintf(
		"SIM 检测 / 已完成\n设备    %s\nSIM     %s\n归属    %s\n网络注册  %s\n数据网络  %s\n余额查询  不支持（未发送短信）\n结论    %s",
		snapshot.DeviceDisplay,
		maskedCommandICCID(snapshot.ICCID),
		snapshot.Home,
		snapshot.Network,
		snapshot.Data,
		conclusion,
	)
}

func formatSIMCheckUnavailable(snapshot simCheckSnapshot, reason, query, conclusion string) string {
	return fmt.Sprintf(
		"SIM 检测 / 无法确认\n设备    %s\nSIM     %s\n归属    %s\n网络注册  %s\n数据网络  %s\n查询短信  %s\n原因    %s\n结论    %s",
		snapshot.DeviceDisplay,
		maskedCommandICCID(snapshot.ICCID),
		commandValueOrDash(snapshot.Home),
		snapshot.Network,
		snapshot.Data,
		query,
		strings.TrimSpace(reason),
		strings.TrimSpace(conclusion),
	)
}

func (m *Manager) sendSIMCheckQuery(worker *device.Worker, pending *pendingSIMCheck) {
	if m == nil || m.pool == nil || worker == nil || pending == nil {
		return
	}

	var sendErr error
	channel := "蜂窝"
	if pending.snapshot.VoWiFi {
		channel = "VoWiFi"
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ctx = messaging.WithSuppressSendTGSuccess(ctx)
		sendErr = m.pool.SendVoWiFiSMS(ctx, pending.snapshot.DeviceID, pending.carrier.ServiceNumber, pending.carrier.Query)
	} else {
		sendErr = worker.SendSMS(pending.carrier.ServiceNumber, pending.carrier.Query)
		if sendErr == nil {
			_ = db.SaveSMS(worker.GetIMSI(), worker.ID, pending.carrier.ServiceNumber, pending.carrier.Query, 2, 2, time.Now())
		}
	}
	if sendErr != nil {
		if m.takePendingSIMCheck(pending) {
			replyCommandAsync(pending.cmdCtx, fmt.Sprintf(
				"SIM 检测 / 无法确认\n设备    %s\n运营商  %s\n查询短信  发送失败（%s）\n原因    %v\n结论    无法判断是否欠费",
				pending.snapshot.DeviceDisplay,
				pending.carrier.Name,
				channel,
				sendErr,
			))
		}
		return
	}

	if current := currentSIMCheckICCID(worker); current != pending.snapshot.ICCID {
		if m.takePendingSIMCheck(pending) {
			replyCommandAsync(pending.cmdCtx, fmt.Sprintf(
				"SIM 检测 / 已取消\n设备    %s\n原因    查询期间 SIM/eSIM 已变化\n结论    本次结果已作废，请重新检测",
				pending.snapshot.DeviceDisplay,
			))
		}
		return
	}
	if !m.armPendingSIMCheck(pending) {
		return
	}

	replyCommandAsync(pending.cmdCtx, fmt.Sprintf(
		"SIM 检测 / 查询已发送\n设备    %s\n运营商  %s\n号码    %s\n通道    %s\n状态    等待运营商回复（%d 秒）",
		pending.snapshot.DeviceDisplay,
		pending.carrier.Name,
		pending.carrier.ServiceNumber,
		channel,
		int(m.pendingSIMCheckTimeout()/time.Second),
	))
}

func (m *Manager) reservePendingSIMCheck(pending *pendingSIMCheck) bool {
	if m == nil || pending == nil {
		return false
	}
	key := normalizeSIMCheckDeviceID(pending.snapshot.DeviceID)
	if key == "" {
		return false
	}
	m.simCheckMu.Lock()
	defer m.simCheckMu.Unlock()
	if m.pendingSIMChecks == nil {
		m.pendingSIMChecks = make(map[string]*pendingSIMCheck)
	}
	if _, exists := m.pendingSIMChecks[key]; exists {
		return false
	}
	m.pendingSIMChecks[key] = pending
	return true
}

func (m *Manager) armPendingSIMCheck(pending *pendingSIMCheck) bool {
	if m == nil || pending == nil {
		return false
	}
	key := normalizeSIMCheckDeviceID(pending.snapshot.DeviceID)
	m.simCheckMu.Lock()
	defer m.simCheckMu.Unlock()
	if m.pendingSIMChecks[key] != pending {
		return false
	}
	pending.timer = time.AfterFunc(m.pendingSIMCheckTimeout(), func() {
		if m.takePendingSIMCheck(pending) {
			replyCommandAsync(pending.cmdCtx, fmt.Sprintf(
				"SIM 检测 / 无法确认\n设备    %s\n运营商  %s\n查询短信  已提交\n运营商回复  超时\n结论    无法判断是否欠费；可能是回复延迟、短信下行异常或查询不受支持",
				pending.snapshot.DeviceDisplay,
				pending.carrier.Name,
			))
		}
	})
	return true
}

func (m *Manager) takePendingSIMCheck(pending *pendingSIMCheck) bool {
	if m == nil || pending == nil {
		return false
	}
	key := normalizeSIMCheckDeviceID(pending.snapshot.DeviceID)
	m.simCheckMu.Lock()
	defer m.simCheckMu.Unlock()
	if m.pendingSIMChecks[key] != pending {
		return false
	}
	delete(m.pendingSIMChecks, key)
	if pending.timer != nil {
		pending.timer.Stop()
	}
	return true
}

func (m *Manager) completePendingSIMCheck(deviceID, sender, content string) {
	if m == nil {
		return
	}
	key := normalizeSIMCheckDeviceID(deviceID)
	m.simCheckMu.Lock()
	pending := m.pendingSIMChecks[key]
	m.simCheckMu.Unlock()
	if pending == nil || normalizeSIMCheckSender(sender) != normalizeSIMCheckSender(pending.carrier.ServiceNumber) {
		return
	}

	if m.pool != nil {
		worker := m.pool.GetWorker(strings.TrimSpace(deviceID))
		if currentSIMCheckICCID(worker) != pending.snapshot.ICCID {
			if m.takePendingSIMCheck(pending) {
				replyCommandAsync(pending.cmdCtx, fmt.Sprintf(
					"SIM 检测 / 已取消\n设备    %s\n原因    收到回复时 SIM/eSIM 已变化\n结论    本次结果已作废，请重新检测",
					pending.snapshot.DeviceDisplay,
				))
			}
			return
		}
	}
	if !m.takePendingSIMCheck(pending) {
		return
	}

	account := "当前可用，未发现明确欠费提示"
	if simCheckReplyHasAccountIssue(content) {
		account = "疑似欠费或停机；请核对运营商原文"
	} else if simCheckReplyIsInconclusive(content) {
		account = "已收到运营商回复，但内容不足以判断是否欠费"
	}
	replyCommandAsync(pending.cmdCtx, fmt.Sprintf(
		"SIM 检测 / 已完成\n设备    %s\n运营商  %s\n网络注册  %s\n数据网络  %s\n短信查询  往返正常（已收到 %s 回复）\n账户判断  %s\n提示    运营商原文已作为新短信通知",
		pending.snapshot.DeviceDisplay,
		pending.carrier.Name,
		pending.snapshot.Network,
		pending.snapshot.Data,
		pending.carrier.ServiceNumber,
		account,
	))
}

func (m *Manager) cancelPendingSIMChecks() {
	if m == nil {
		return
	}
	m.simCheckMu.Lock()
	defer m.simCheckMu.Unlock()
	for key, pending := range m.pendingSIMChecks {
		if pending != nil && pending.timer != nil {
			pending.timer.Stop()
		}
		delete(m.pendingSIMChecks, key)
	}
}

func (m *Manager) pendingSIMCheckTimeout() time.Duration {
	if m != nil && m.simCheckTimeout > 0 {
		return m.simCheckTimeout
	}
	return defaultSIMCheckTimeout
}

func normalizeSIMCheckDeviceID(deviceID string) string {
	return strings.ToLower(strings.TrimSpace(deviceID))
}

func normalizeSIMCheckSender(sender string) string {
	sender = digitsOnly(sender)
	switch {
	case strings.HasPrefix(sender, "0086") && len(sender) > 4:
		return sender[4:]
	case strings.HasPrefix(sender, "86") && len(sender) > 5:
		return sender[2:]
	default:
		return sender
	}
}

func digitsOnly(value string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func currentSIMCheckICCID(worker *device.Worker) string {
	if worker == nil {
		return ""
	}
	if iccid := strings.TrimSpace(worker.CurrentICCID()); iccid != "" {
		return iccid
	}
	return strings.TrimSpace(worker.GetCachedDeviceStatus().ICCID)
}

var zeroArrearsPattern = regexp.MustCompile(`欠费(?:金额)?(?:为|[:：])?0(?:\.0+)?(?:元|，|。|；|;|$)`)

func simCheckReplyHasAccountIssue(content string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(content), " ", "")
	normalized = zeroArrearsPattern.ReplaceAllString(normalized, "")
	for _, phrase := range []string{"未欠费", "无欠费", "没有欠费", "不欠费"} {
		normalized = strings.ReplaceAll(normalized, phrase, "")
	}
	for _, phrase := range []string{"欠费", "停机", "暂停服务", "余额不足", "服务暂停", "已暂停"} {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
}

func simCheckReplyIsInconclusive(content string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(content), " ", "")
	for _, phrase := range []string{"指令错误", "指令有误", "无法查询", "查询失败", "系统繁忙", "稍后再试", "暂不支持", "不支持该查询"} {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
}

func replyCommandAsync(cmdCtx CommandContext, text string) {
	if cmdCtx == nil || strings.TrimSpace(text) == "" {
		return
	}
	go cmdCtx.Reply(text)
}
