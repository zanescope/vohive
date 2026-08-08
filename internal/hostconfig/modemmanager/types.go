package modemmanager

import (
	"context"
	"errors"
)

const (
	DefaultRulePath   = "/etc/udev/rules.d/78-mm-vohive-managed.rules"
	DefaultSysfsRoot  = "/sys/bus/usb/devices"
	DefaultSysfsMount = "/sys"

	AbsentRevision = "absent"
)

var (
	ErrForeignRule            = errors.New("the ModemManager isolation rule is not managed by VoHive")
	ErrTamperedRule           = errors.New("the VoHive-managed ModemManager isolation rule was modified")
	ErrRevisionConflict       = errors.New("the ModemManager isolation rule revision changed")
	ErrTargetSnapshotConflict = errors.New("the resolved USB target snapshot changed")
	ErrUnsafeUSBPath          = errors.New("unsafe sysfs USB path")
	ErrNoStableMatcher        = errors.New("no stable USB matcher is available")
	ErrInvalidRequest         = errors.New("invalid ModemManager isolation request")
	ErrCleanupIncomplete      = errors.New("committed ModemManager isolation cleanup is incomplete")
)

type RuleState string

const (
	StateAbsent   RuleState = "absent"
	StateManaged  RuleState = "managed"
	StateForeign  RuleState = "foreign"
	StateTampered RuleState = "tampered"
)

type MatcherKind string

const (
	MatcherSerial     MatcherKind = "serial"
	MatcherKernelPath MatcherKind = "kernel_path"
)

type Target struct {
	ID      string `json:"id"`
	USBPath string `json:"usb_path"`
}

type Matcher struct {
	Kind       MatcherKind `json:"kind"`
	VendorID   string      `json:"vendor_id"`
	Serial     string      `json:"serial,omitempty"`
	KernelPath string      `json:"kernel_path,omitempty"`
}

type Entry struct {
	TargetID string  `json:"target_id"`
	Matcher  Matcher `json:"matcher"`
}

type Request struct {
	Targets          []Target `json:"targets,omitempty"`
	ExpectedRevision string   `json:"expected_revision,omitempty"`
	ExpectedEntries  []Entry  `json:"-"`
}

type Status struct {
	State    RuleState `json:"state"`
	Revision string    `json:"revision"`
	Entries  []Entry   `json:"entries,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

type Result struct {
	State               RuleState `json:"state"`
	Revision            string    `json:"revision"`
	Changed             bool      `json:"changed"`
	Reloaded            bool      `json:"reloaded"`
	ReloadIndeterminate bool      `json:"reload_indeterminate,omitempty"`
	Entries             []Entry   `json:"entries,omitempty"`
}

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type Options struct {
	RulePath   string
	SysfsRoot  string
	SysfsMount string
	Runner     Runner
}
