package hostconfig

import (
	"context"
	"errors"
)

const (
	DefaultStateDir    = "/var/lib/vohive/host-config"
	DefaultRequestPath = DefaultStateDir + "/request.json"
)

var (
	ErrUnsupported            = errors.New("host configuration is unsupported")
	ErrNotInstallable         = errors.New("ModemManager isolation configuration is not installable")
	ErrNotUninstallable       = errors.New("ModemManager isolation configuration is not uninstallable")
	ErrOperationBusy          = errors.New("another host configuration operation is in progress")
	ErrOperationIndeterminate = errors.New("host configuration changed without a confirmed udev reload")
	ErrPlanConflict           = errors.New("the ModemManager isolation preview changed")
	ErrWorkerResult           = errors.New("invalid host configuration worker result")
	ErrWorkerResultStale      = errors.New("host configuration worker result does not match the request")
)

type Action string

const (
	ActionInstall   Action = "install"
	ActionUninstall Action = "uninstall"
)

type IsolationState string

const (
	StateAbsent        IsolationState = "absent"
	StateCurrent       IsolationState = "current"
	StateStale         IsolationState = "stale"
	StatePartial       IsolationState = "partial"
	StatePendingReplug IsolationState = "pending_replug"
	StateForeign       IsolationState = "foreign"
	StateModified      IsolationState = "modified"
	StateUnsupported   IsolationState = "unsupported"
)

type Target struct {
	ID            string
	Name          string
	USBPath       string
	ControlDevice string
}

type DeviceStatus struct {
	DeviceID      string `json:"device_id"`
	Name          string `json:"name,omitempty"`
	ControlDevice string `json:"control_device,omitempty"`
	SelectorKind  string `json:"selector_kind,omitempty"`
	SelectorValue string `json:"selector_value,omitempty"`
	Covered       bool   `json:"covered"`
	Reason        string `json:"reason,omitempty"`
}

type Status struct {
	Status          IsolationState `json:"status"`
	Reason          string         `json:"reason,omitempty"`
	RulePath        string         `json:"rule_path,omitempty"`
	Revision        string         `json:"revision,omitempty"`
	PlanRevision    string         `json:"plan_revision,omitempty"`
	ManagedByVoHive bool           `json:"managed_by_vohive"`
	CanInstall      bool           `json:"can_install"`
	CanUninstall    bool           `json:"can_uninstall"`
	TotalDevices    int            `json:"total_devices"`
	CoveredDevices  int            `json:"covered_devices"`
	RequiresReplug  bool           `json:"requires_replug,omitempty"`
	Warning         string         `json:"warning,omitempty"`
	ManualAttention bool           `json:"manual_attention,omitempty"`
	Devices         []DeviceStatus `json:"devices"`
}

type Coordinator interface {
	Status(context.Context, []Target) (Status, error)
	Apply(context.Context, Action, []Target, string, string) (Status, error)
}
