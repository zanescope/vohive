package main

import (
	"context"
	"strings"
	"testing"
)

func TestHostConfigCleanupCLIHasFixedScope(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "requires one mode",
			want: "requires exactly one of --request or --cleanup-managed-for-uninstall",
		},
		{
			name: "modes are mutually exclusive",
			args: []string{
				"--request", "request.json", "--cleanup-managed-for-uninstall",
			},
			want: "requires exactly one of --request or --cleanup-managed-for-uninstall",
		},
		{
			name: "rejects alternate absolute request path",
			args: []string{"--request", "/tmp/host-config/request.json"},
			want: "host-config --request must be /var/lib/vohive/host-config/request.json",
		},
		{
			name: "rejects targets",
			args: []string{"--cleanup-managed-for-uninstall", "device-a"},
			want: "takes no positional arguments",
		},
		{
			name: "rejects custom rule path",
			args: []string{
				"--cleanup-managed-for-uninstall",
				"--rule-path",
				"/tmp/administrator.rules",
			},
			want: "flag provided but not defined",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := applyHostConfig(context.Background(), test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("applyHostConfig(%q) error = %v, want %q", test.args, err, test.want)
			}
		})
	}
}
