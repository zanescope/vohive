package device

import "testing"

func TestQMIErrorIndicatesTransportDown(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"broken_pipe", "write failed: write unix @->@qmi-proxy: write: broken pipe", true},
		{"eof", "QMI: read failed: EOF", true},
		{"connection_closed", "connection closed", true},
		{"no_such_device", "open /dev/cdc-wdm2: no such device", true},
		{"failed_open", "failed to open qmi device", true},
		{"bad_fd", "read /dev/cdc-wdm2: bad file descriptor", true},
		{"closed_network", "use of closed network connection", true},
		{"connection_reset", "read: connection reset by peer", true},
		{"io_error", "read /dev/cdc-wdm2: input/output error", true},
		{"transport_endpoint", "transport endpoint is not connected", true},
		{"empty", "", false},
		{"identity_empty", "refresh_identity: live_identity_empty", false},
		{"deadline", "context deadline exceeded", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := qmiErrorIndicatesTransportDown(tc.msg); got != tc.want {
				t.Fatalf("qmiErrorIndicatesTransportDown(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}
