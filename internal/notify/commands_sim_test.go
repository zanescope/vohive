package notify

import (
	"strings"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestNormalizeFeishuCommandText(t *testing.T) {
	tests := []struct {
		name           string
		text           string
		mentions       []string
		requireMention bool
		want           string
		ok             bool
	}{
		{name: "p2p direct command", text: " /help ", want: "/help", ok: true},
		{name: "group mention command", text: "@_user_1 /send wwan9 13600136286 Test", mentions: []string{"@_user_1"}, requireMention: true, want: "/send wwan9 13600136286 Test", ok: true},
		{name: "group without mention", text: "/help", mentions: []string{"@_user_1"}, requireMention: true},
		{name: "mention not leading", text: "hello @_user_1 /help", mentions: []string{"@_user_1"}, requireMention: true},
		{name: "mention prefix collision", text: "@_user_12 /help", mentions: []string{"@_user_1"}, requireMention: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := normalizeFeishuCommandText(test.text, test.mentions, test.requireMention)
			if got != test.want || ok != test.ok {
				t.Fatalf("normalizeFeishuCommandText()=(%q,%v), want (%q,%v)", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestFormatCommandSIMEntries(t *testing.T) {
	got := formatCommandSIMEntries([]commandSIMEntry{{
		DeviceID: "wwan9",
		ICCID:    "8986000000000012345",
		Phone:    "13800138000",
		Operator: "中国移动",
		Note:     "主卡",
		Enabled:  true,
		Current:  true,
	}})
	for _, want := range []string{"设备  wwan9", "ICCID   ****2345", "本机    13800138000", "运营商  中国移动", "备注    主卡"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted SIM list missing %q: %s", want, got)
		}
	}
}

func TestFeishuMessageChatIDUsesOnlyOriginChat(t *testing.T) {
	chatID := " oc_origin "
	msg := &larkim.EventMessage{ChatId: &chatID}
	if got := feishuMessageChatID(msg); got != "oc_origin" {
		t.Fatalf("feishuMessageChatID()=%q", got)
	}
	if got := feishuMessageChatID(nil); got != "" {
		t.Fatalf("feishuMessageChatID(nil)=%q", got)
	}
	msg.ChatId = nil
	if got := feishuMessageChatID(msg); got != "" {
		t.Fatalf("feishuMessageChatID(without chat)=%q", got)
	}
}
