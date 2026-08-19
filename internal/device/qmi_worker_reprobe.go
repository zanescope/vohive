package device

import "strings"

// qmiTerminalWorkerNeedsLiveReprobe identifies a terminal Worker that must be
// replaced even when the re-enumerated USB device kept the same cdc-wdm, WWAN,
// and topology paths. Path equality is not evidence that the old QMI file
// descriptor or client is still usable after a USB disconnect.
func qmiTerminalWorkerNeedsLiveReprobe(worker *Worker) (bool, string) {
	if worker == nil || !requiresQMICore(worker.Config) {
		return false, ""
	}
	snapshot := worker.HealthSnapshot()
	if snapshot.State != HealthStateFailed {
		return false, ""
	}
	reason := strings.TrimSpace(snapshot.EventType)
	if reason == "" {
		reason = strings.TrimSpace(snapshot.Reason)
	}
	if reason == "" {
		reason = "health_failed"
	}
	return true, reason
}
