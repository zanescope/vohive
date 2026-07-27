package notify

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/pkg/logger"
)

type captureChannel struct {
	mu    sync.Mutex
	msgs  []string
	calls []NotificationContext
}

func (c *captureChannel) Name() string { return "capture" }

func (c *captureChannel) Send(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, text)
	return nil
}

func (c *captureChannel) SendWithContext(ctx NotificationContext) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, ctx)
	c.msgs = append(c.msgs, ctx.Text)
	return nil
}

func (c *captureChannel) RegisterCommand(_ string, _ CommandHandler) {}
func (c *captureChannel) Start() error                               { return nil }
func (c *captureChannel) Close() error                               { return nil }

func (c *captureChannel) Last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.msgs) == 0 {
		return ""
	}
	return c.msgs[len(c.msgs)-1]
}

func (c *captureChannel) LastContext() NotificationContext {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return NotificationContext{}
	}
	return c.calls[len(c.calls)-1]
}

func readLogFields(t *testing.T, entry logger.LogEntry) map[string]any {
	t.Helper()
	if entry.Fields == "" {
		return map[string]any{}
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(entry.Fields), &fields); err != nil {
		t.Fatalf("failed to parse log fields: %v", err)
	}
	return fields
}

func waitLogEntry(t *testing.T, ch <-chan logger.LogEntry, match func(entry logger.LogEntry) bool) logger.LogEntry {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case entry := <-ch:
			if match(entry) {
				return entry
			}
		case <-deadline:
			t.Fatal("matched log entry not found")
		}
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestFormatSMSNotificationIncludesLocalPhone(t *testing.T) {
	ts := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		source     string
		localPhone string
		simNote    string
		want       string
	}{
		{
			name:       "cellular known number",
			source:     "蜂窝",
			localPhone: " +8613900000000 ",
			simNote:    "香港卡",
			want:       "收到新短信 / 蜂窝\n设备  wwan0\n本机  +8613900000000 (香港卡)\n号码  +8613800000000\n时间  2026-04-13 12:00:00\n内容  hello",
		},
		{
			name:   "vowifi unknown number",
			source: "VoWiFi",
			want:   "收到新短信 / VoWiFi\n设备  wwan0\n本机  --\n号码  +8613800000000\n时间  2026-04-13 12:00:00\n内容  hello",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatSMSNotification("wwan0", tt.localPhone, tt.simNote, "+8613800000000", "hello", tt.source, ts)
			if got != tt.want {
				t.Fatalf("formatSMSNotification()=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestNotificationDeviceDisplayNamePrefersNameAndFallsBackToID(t *testing.T) {
	tests := []struct {
		name       string
		deviceID   string
		deviceName string
		want       string
	}{
		{name: "configured name", deviceID: "wwan0", deviceName: " Living Room SIM ", want: "Living Room SIM"},
		{name: "missing name", deviceID: " wwan0 ", want: "wwan0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notificationDeviceDisplayName(tt.deviceID, tt.deviceName); got != tt.want {
				t.Fatalf("notificationDeviceDisplayName()=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveNotificationLocalPhoneUsesCoherentIdentityKeys(t *testing.T) {
	tests := []struct {
		name      string
		imsi      string
		iccid     string
		wantIMSI  string
		wantICCID string
	}{
		{
			name:      "imsi and iccid",
			imsi:      " 460001234567890 ",
			iccid:     " 8986000000000000001 ",
			wantIMSI:  "460001234567890",
			wantICCID: "8986000000000000001",
		},
		{
			name:      "iccid only",
			iccid:     " 8986000000000000002 ",
			wantICCID: "8986000000000000002",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			got, err := resolveNotificationLocalPhone(tt.imsi, tt.iccid, true, func(imsi, iccid string) (string, error) {
				calls++
				if imsi != tt.wantIMSI || iccid != tt.wantICCID {
					t.Fatalf("lookup identity=(%q,%q), want (%q,%q)", imsi, iccid, tt.wantIMSI, tt.wantICCID)
				}
				return " +8613900000000 ", nil
			})
			if err != nil {
				t.Fatalf("resolveNotificationLocalPhone() error=%v", err)
			}
			if got != "+8613900000000" {
				t.Fatalf("resolveNotificationLocalPhone()=%q, want +8613900000000", got)
			}
			if calls != 1 {
				t.Fatalf("lookup calls=%d, want 1", calls)
			}
		})
	}
}

func TestResolveNotificationLocalPhoneFallbacksWithoutBlocking(t *testing.T) {
	lookupErr := errors.New("database unavailable")
	tests := []struct {
		name           string
		imsi           string
		iccid          string
		identityUsable bool
		lookupPhone    string
		lookupErr      error
		wantCalls      int
		wantLookupErr  bool
	}{
		{
			name:      "identity unusable",
			imsi:      "460001234567890",
			iccid:     "8986000000000000001",
			wantCalls: 0,
		},
		{
			name:           "identity unavailable",
			identityUsable: true,
			wantCalls:      0,
		},
		{
			name:           "number unavailable",
			imsi:           "460001234567890",
			identityUsable: true,
			lookupPhone:    "  ",
			wantCalls:      1,
		},
		{
			name:           "database error",
			imsi:           "460001234567890",
			identityUsable: true,
			lookupErr:      lookupErr,
			wantCalls:      1,
			wantLookupErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			got, err := resolveNotificationLocalPhone(tt.imsi, tt.iccid, tt.identityUsable, func(_, _ string) (string, error) {
				calls++
				return tt.lookupPhone, tt.lookupErr
			})
			if got != unknownNotificationLocalPhone {
				t.Fatalf("resolveNotificationLocalPhone()=%q, want %q", got, unknownNotificationLocalPhone)
			}
			if tt.wantLookupErr {
				if !errors.Is(err, lookupErr) {
					t.Fatalf("resolveNotificationLocalPhone() error=%v, want %v", err, lookupErr)
				}
			} else if err != nil {
				t.Fatalf("resolveNotificationLocalPhone() error=%v", err)
			}
			if calls != tt.wantCalls {
				t.Fatalf("lookup calls=%d, want %d", calls, tt.wantCalls)
			}
		})
	}
}

func TestManagerNotifyEventsToWebhookWithTemplate(t *testing.T) {
	var mu sync.Mutex
	var payloads []webhookPayload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload webhookPayload
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		payloads = append(payloads, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh, err := NewWebhookChannel(webhookConfigForTest(srv.URL, "[{{device_label}}] {{text}}"))
	if err != nil {
		t.Fatalf("NewWebhookChannel() error = %v", err)
	}

	m := &Manager{channels: []Channel{wh}}

	ts := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	m.NotifySMS("wwan0", "+8613800000000", "hello", ts)
	m.NotifyIPRotated("wwan0", "1.1.1.1", "2.2.2.2", 2*time.Second)
	m.NotifyRaw("raw message")

	waitUntil(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(payloads) == 3
	})
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 3 {
		t.Fatalf("payload count=%d, want=3", len(payloads))
	}
	byEvent := make(map[string]webhookPayload, len(payloads))
	for _, payload := range payloads {
		byEvent[payload.Event] = payload
	}
	if got := byEvent["sms_received"].Text; got != "[wwan0] 收到新短信 / 蜂窝\n设备  wwan0\n本机  --\n号码  +8613800000000\n时间  2026-04-13 12:00:00\n内容  hello" {
		t.Fatalf("sms text=%q", got)
	}
	if got := byEvent["ip_rotated"].Meta.DeviceID; got != "wwan0" {
		t.Fatalf("ip_rotated meta.device_id=%q", got)
	}
	if _, ok := byEvent["raw"]; !ok {
		t.Fatal("raw event missing")
	}
}

func TestManagerNotifyRawKeepsPlainChannelText(t *testing.T) {
	capture := &captureChannel{}
	m := &Manager{channels: []Channel{capture}}

	m.NotifyRaw("plain channel text")
	waitUntil(t, time.Second, func() bool { return capture.Last() != "" })
	if got := capture.Last(); got != "plain channel text" {
		t.Fatalf("plain channel text=%q", got)
	}
}

func TestManagerNotifyIPRotatedUsesPlainTemplate(t *testing.T) {
	capture := &captureChannel{}
	m := &Manager{channels: []Channel{capture}}

	m.NotifyIPRotated("wwan0", "1.1.1.1", "2.2.2.2", 2*time.Second)
	waitUntil(t, time.Second, func() bool { return capture.Last() != "" })
	want := "公网切换 / 完成\n设备    wwan0\n旧 IP   1.1.1.1\n新 IP   2.2.2.2\n耗时    2s"
	if got := capture.Last(); got != want {
		t.Fatalf("ip rotated text=%q, want %q", got, want)
	}
}

func TestManagerNotifyIncomingCallUsesPlainTemplate(t *testing.T) {
	capture := &captureChannel{}
	m := &Manager{channels: []Channel{capture}}

	m.NotifyIncomingCall("wwan0", "10086", "10010")
	time.Sleep(20 * time.Millisecond)
	want := "来电通知\n设备    wwan0\n主叫    10086\n被叫    10010"
	if got := capture.Last(); got != want {
		t.Fatalf("incoming call text=%q, want %q", got, want)
	}
}

func TestManagerNotifySMSLogsBroadcastSummary(t *testing.T) {
	logger.Setup(logger.LogConfig{Debug: true, Filename: filepath.Join(t.TempDir(), "app.log")})
	capture := &captureChannel{}
	m := &Manager{channels: []Channel{capture}}
	ch := logger.GlobalBroadcaster.Subscribe()
	defer logger.GlobalBroadcaster.Unsubscribe(ch)

	ts := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	m.NotifySMS("wwan0", "+8613800000000", "hello", ts)

	entry := waitLogEntry(t, ch, func(entry logger.LogEntry) bool {
		return entry.Message == "开始发送短信通知"
	})
	fields := readLogFields(t, entry)
	if fields["event"] != "sms_received" {
		t.Fatalf("event=%v want sms_received", fields["event"])
	}
	if fields["channel_count"] != float64(1) {
		t.Fatalf("channel_count=%v want 1", fields["channel_count"])
	}
}

func TestManagerNotifySMSWithSourceUsesProvidedSourceLabel(t *testing.T) {
	capture := &captureChannel{}
	m := &Manager{channels: []Channel{capture}}
	notifier, ok := any(m).(interface {
		NotifySMSWithSource(deviceID, sender, content, source string, timestamp time.Time)
	})
	if !ok {
		t.Fatal("NotifySMSWithSource missing")
	}

	ts := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	notifier.NotifySMSWithSource("wwan0", "+8613800000000", "hello", "VoWiFi", ts)

	waitUntil(t, time.Second, func() bool { return capture.Last() != "" })
	want := "收到新短信 / VoWiFi\n设备  wwan0\n本机  --\n号码  +8613800000000\n时间  2026-04-13 12:00:00\n内容  hello"
	if got := capture.Last(); got != want {
		t.Fatalf("text=%q, want %q", got, want)
	}
	if got := capture.LastContext().Event; got != "sms_received" {
		t.Fatalf("event=%q, want sms_received", got)
	}
}

func webhookConfigForTest(url, template string) config.WebhookConfig {
	return config.WebhookConfig{
		Enabled:      true,
		URLs:         []string{url},
		TimeoutMs:    5000,
		RetryMax:     0,
		TextTemplate: template,
	}
}
