package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/modem"
)

type simCheckCommandContext struct {
	replies chan string
}

func newSIMCheckCommandContext() *simCheckCommandContext {
	return &simCheckCommandContext{replies: make(chan string, 4)}
}

func (c *simCheckCommandContext) Reply(text string) {
	c.replies <- text
}

func waitSIMCheckReply(t *testing.T, ctx *simCheckCommandContext) string {
	t.Helper()
	select {
	case reply := <-ctx.replies:
		return reply
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SIM check reply")
		return ""
	}
}

func TestResolveSIMCheckCarrierUsesHomeNetwork(t *testing.T) {
	tests := []struct {
		name          string
		status        modem.DeviceStatus
		wantName      string
		wantNumber    string
		wantSupported bool
	}{
		{
			name:          "china mobile native plmn",
			status:        modem.DeviceStatus{NativeMCC: "460", NativeMNC: "00", IMSI: "460011234567890"},
			wantName:      "中国移动",
			wantNumber:    "10086",
			wantSupported: true,
		},
		{
			name:          "single digit native mnc is padded",
			status:        modem.DeviceStatus{NativeMCC: "460", NativeMNC: "1"},
			wantName:      "中国联通",
			wantNumber:    "10010",
			wantSupported: true,
		},
		{
			name:          "three digit native mnc is normalized",
			status:        modem.DeviceStatus{NativeMCC: "460", NativeMNC: "003"},
			wantName:      "中国电信",
			wantNumber:    "10001",
			wantSupported: true,
		},
		{
			name:          "imsi fallback",
			status:        modem.DeviceStatus{IMSI: "460031234567890"},
			wantName:      "中国电信",
			wantNumber:    "10001",
			wantSupported: true,
		},
		{
			name:          "unsupported china broadnet",
			status:        modem.DeviceStatus{NativeMCC: "460", NativeMNC: "15"},
			wantSupported: false,
		},
		{
			name:          "foreign native plmn is authoritative",
			status:        modem.DeviceStatus{NativeMCC: "454", NativeMNC: "12", IMSI: "460001234567890"},
			wantSupported: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			carrier, supported := resolveSIMCheckCarrier(test.status)
			if supported != test.wantSupported {
				t.Fatalf("supported=%v, want %v", supported, test.wantSupported)
			}
			if carrier.Name != test.wantName || carrier.ServiceNumber != test.wantNumber {
				t.Fatalf("carrier=(%q,%q), want (%q,%q)", carrier.Name, carrier.ServiceNumber, test.wantName, test.wantNumber)
			}
		})
	}
}

func TestSIMCheckCardScope(t *testing.T) {
	tests := []struct {
		status modem.DeviceStatus
		want   string
	}{
		{status: modem.DeviceStatus{NativeMCC: "454"}, want: "香港 SIM/eSIM"},
		{status: modem.DeviceStatus{IMSI: "455011234567890"}, want: "澳门 SIM/eSIM"},
		{status: modem.DeviceStatus{NativeMCC: "310"}, want: "境外 SIM/eSIM"},
		{status: modem.DeviceStatus{}, want: "归属未知的 SIM/eSIM"},
	}
	for _, test := range tests {
		if got := simCheckCardScope(test.status); got != test.want {
			t.Fatalf("simCheckCardScope(%+v)=%q, want %q", test.status, got, test.want)
		}
	}
}

func TestSIMCheckReplyHasAccountIssueIsConservative(t *testing.T) {
	tests := []struct {
		content string
		want    bool
	}{
		{content: "您的账户余额为 20.00 元", want: false},
		{content: "您的账户当前未欠费，服务正常", want: false},
		{content: "当前欠费0元", want: false},
		{content: "当前欠费0.5元", want: true},
		{content: "因账户欠费，服务已停机", want: true},
		{content: "余额不足，部分服务暂停", want: true},
	}
	for _, test := range tests {
		if got := simCheckReplyHasAccountIssue(test.content); got != test.want {
			t.Fatalf("simCheckReplyHasAccountIssue(%q)=%v, want %v", test.content, got, test.want)
		}
	}
}

func TestSIMCheckReplyCanRemainInconclusive(t *testing.T) {
	tests := []struct {
		content string
		want    bool
	}{
		{content: "系统繁忙，请稍后再试", want: true},
		{content: "您发送的指令有误", want: true},
		{content: "您的账户余额为20元", want: false},
	}
	for _, test := range tests {
		if got := simCheckReplyIsInconclusive(test.content); got != test.want {
			t.Fatalf("simCheckReplyIsInconclusive(%q)=%v, want %v", test.content, got, test.want)
		}
	}
}

func TestSIMCheckReplyMatchesDeviceAndCarrierAndKeepsRawNotification(t *testing.T) {
	commandCtx := newSIMCheckCommandContext()
	channel := &captureChannel{}
	manager := &Manager{channels: []Channel{channel}}
	pending := &pendingSIMCheck{
		snapshot: simCheckSnapshot{
			DeviceID:      "wwan0",
			DeviceDisplay: "主卡 (wwan0)",
			ICCID:         "8986000000000012345",
			Network:       "已注册（本地）",
			Data:          "已连接（1.2.3.4）",
		},
		carrier: simCheckCarrier{Name: "中国移动", ServiceNumber: "10086", Query: "YE"},
		cmdCtx:  commandCtx,
	}
	if !manager.reservePendingSIMCheck(pending) {
		t.Fatal("reservePendingSIMCheck()=false")
	}

	manager.NotifySMSWithSource("wwan0", "+86 10086", "您的账户当前未欠费，余额为20元", "蜂窝", time.Now())

	reply := waitSIMCheckReply(t, commandCtx)
	for _, want := range []string{"SIM 检测 / 已完成", "短信查询  往返正常", "当前可用，未发现明确欠费提示"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("SIM check reply missing %q: %s", want, reply)
		}
	}
	waitUntil(t, time.Second, func() bool {
		return strings.Contains(channel.Last(), "您的账户当前未欠费，余额为20元")
	})
}

func TestSIMCheckReplyIgnoresUnexpectedSender(t *testing.T) {
	commandCtx := newSIMCheckCommandContext()
	manager := &Manager{}
	pending := &pendingSIMCheck{
		snapshot: simCheckSnapshot{DeviceID: "wwan0", ICCID: "8986000000000012345"},
		carrier:  simCheckCarrier{Name: "中国移动", ServiceNumber: "10086", Query: "YE"},
		cmdCtx:   commandCtx,
	}
	if !manager.reservePendingSIMCheck(pending) {
		t.Fatal("reservePendingSIMCheck()=false")
	}

	manager.completePendingSIMCheck("wwan0", "10010", "余额不足")
	select {
	case reply := <-commandCtx.replies:
		t.Fatalf("unexpected reply for wrong sender: %s", reply)
	case <-time.After(30 * time.Millisecond):
	}

	manager.simCheckMu.Lock()
	stillPending := manager.pendingSIMChecks["wwan0"] == pending
	manager.simCheckMu.Unlock()
	if !stillPending {
		t.Fatal("pending check was consumed by unexpected sender")
	}
	manager.cancelPendingSIMChecks()
}

func TestSIMCheckTimeoutStaysUnknown(t *testing.T) {
	commandCtx := newSIMCheckCommandContext()
	manager := &Manager{simCheckTimeout: 20 * time.Millisecond}
	pending := &pendingSIMCheck{
		snapshot: simCheckSnapshot{DeviceID: "wwan0", DeviceDisplay: "wwan0", ICCID: "8986000000000012345"},
		carrier:  simCheckCarrier{Name: "中国电信", ServiceNumber: "10001", Query: "YE"},
		cmdCtx:   commandCtx,
	}
	if !manager.reservePendingSIMCheck(pending) || !manager.armPendingSIMCheck(pending) {
		t.Fatal("failed to reserve and arm pending SIM check")
	}

	reply := waitSIMCheckReply(t, commandCtx)
	for _, want := range []string{"运营商回复  超时", "无法判断是否欠费"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("timeout reply missing %q: %s", want, reply)
		}
	}
}

func TestSIMCheckAllowsOnlyOnePendingQueryPerDevice(t *testing.T) {
	manager := &Manager{}
	first := &pendingSIMCheck{snapshot: simCheckSnapshot{DeviceID: " WWAN0 "}}
	second := &pendingSIMCheck{snapshot: simCheckSnapshot{DeviceID: "wwan0"}}
	if !manager.reservePendingSIMCheck(first) {
		t.Fatal("first reservePendingSIMCheck()=false")
	}
	if manager.reservePendingSIMCheck(second) {
		t.Fatal("second reservePendingSIMCheck()=true, want false")
	}
	manager.cancelPendingSIMChecks()
}
