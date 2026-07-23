package device

import (
	"context"
	"fmt"
	"strings"
)

// ResolvedATPort 返回当前设备真实可用的 AT 口,统一的内存兜底链:
// 运行时 Config.ATPort → Config.ManagePort → Modem 在设备获取时刻的快照端口。
// 零路径架构下持久化侧不再承载路径,因此这里绝不读配置文件,只读内存。
func (w *Worker) ResolvedATPort() string {
	if w == nil {
		return ""
	}
	if v := strings.TrimSpace(w.Config.ATPort); v != "" {
		return v
	}
	if v := strings.TrimSpace(w.Config.ManagePort); v != "" {
		return v
	}
	if w.Modem != nil {
		return strings.TrimSpace(w.Modem.ATPort())
	}
	return ""
}

// WithTransientATPort serializes short-lived direct serial sessions for one
// worker so manual AT commands and automatic AT fallbacks cannot race.
func (w *Worker) WithTransientATPort(fn func(port string) (string, error)) (string, error) {
	return w.WithTransientATPortContext(context.Background(), fn)
}

// WithTransientATPortContext also allows callers to abandon lock contention
// when their request has already expired.
func (w *Worker) WithTransientATPortContext(ctx context.Context, fn func(port string) (string, error)) (string, error) {
	if w == nil {
		return "", fmt.Errorf("worker is nil")
	}
	if fn == nil {
		return "", fmt.Errorf("transient AT operation is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.transientATOnce.Do(func() {
		w.transientATGate = make(chan struct{}, 1)
	})
	select {
	case w.transientATGate <- struct{}{}:
		defer func() { <-w.transientATGate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if port := w.ResolvedATPort(); port != "" {
		return fn(port)
	}
	return "", fmt.Errorf("current device has no available AT port")
}
