package device

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/backend"
	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/internal/db"
)

func TestAcquirePhoneNumberFallsBackToCNUMForQMI(t *testing.T) {
	original := queryMSISDNOnTransientATPort
	defer func() { queryMSISDNOnTransientATPort = original }()

	var gotPort string
	queryMSISDNOnTransientATPort = func(port string) (string, error) {
		gotPort = port
		return "+447700902001", nil
	}
	worker := &Worker{
		ID:     "phone-fallback",
		Config: config.DeviceConfig{ATPort: "/dev/ttyUSB9"},
		Backend: &workerPhoneBackendStub{
			workerStatusBackendStub: workerStatusBackendStub{mode: backend.BackendQMI},
		},
	}

	result := worker.AcquirePhoneNumber(context.Background(), true)
	if result.Err != nil {
		t.Fatalf("AcquirePhoneNumber() error=%v", result.Err)
	}
	if result.Number != "+447700902001" || result.Channel != "at_cnum" {
		t.Fatalf("result=%+v", result)
	}
	if gotPort != "/dev/ttyUSB9" {
		t.Fatalf("fallback port=%q", gotPort)
	}
}

func TestPersistPhoneNumberFallsBackWhenBackendEchoesIMSI(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	original := queryMSISDNOnTransientATPort
	defer func() { queryMSISDNOnTransientATPort = original }()

	queryMSISDNOnTransientATPort = func(string) (string, error) {
		return "+447700902007", nil
	}
	const imsi = "234150000002007"
	worker := &Worker{
		ID:     "phone-imsi-echo",
		Config: config.DeviceConfig{ATPort: "/dev/ttyUSB9"},
		Backend: &workerPhoneBackendStub{
			workerStatusBackendStub: workerStatusBackendStub{mode: backend.BackendQMI},
			msisdn:                  imsi,
		},
	}

	result := NewPool(nil).PersistPhoneNumber(
		context.Background(),
		worker,
		imsi,
		"894400000000002007",
		true,
	)
	if result.Err != nil {
		t.Fatalf("PersistPhoneNumber() error=%v", result.Err)
	}
	if result.Number != "+447700902007" || result.Channel != "at_cnum" {
		t.Fatalf("result=%+v", result)
	}
	snapshot, err := db.GetPhoneNumberSnapshotByIMSIOrICCID(imsi, "894400000000002007")
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700902007" || snapshot.PhoneNumberSource != db.PhoneNumberSourceModem {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestAcquirePhoneNumberRejectsPlaceholderAsSuccess(t *testing.T) {
	worker := &Worker{
		Backend: &workerPhoneBackendStub{
			workerStatusBackendStub: workerStatusBackendStub{mode: backend.BackendQMI},
			msisdn:                  "Own Number",
		},
	}
	result := worker.AcquirePhoneNumber(context.Background(), false)
	if result.Number != "" {
		t.Fatalf("result=%+v, want no acquired number", result)
	}
}

func TestAcquirePhoneNumberDoesNotUseTransientATDuringAutomaticSync(t *testing.T) {
	original := queryMSISDNOnTransientATPort
	defer func() { queryMSISDNOnTransientATPort = original }()

	var called atomic.Bool
	queryMSISDNOnTransientATPort = func(string) (string, error) {
		called.Store(true)
		return "+447700902002", nil
	}
	worker := &Worker{
		Config: config.DeviceConfig{ATPort: "/dev/ttyUSB9"},
		Backend: &workerPhoneBackendStub{
			workerStatusBackendStub: workerStatusBackendStub{mode: backend.BackendQMI},
		},
	}

	result := worker.AcquirePhoneNumber(context.Background(), false)
	if result.Number != "" {
		t.Fatalf("result=%+v, want no automatic AT fallback", result)
	}
	if called.Load() {
		t.Fatal("automatic phone sync opened a transient AT session")
	}
}

func TestTransientATPhoneFallbackIsSerializedPerWorker(t *testing.T) {
	original := queryMSISDNOnTransientATPort
	defer func() { queryMSISDNOnTransientATPort = original }()

	var active atomic.Int32
	var maxActive atomic.Int32
	queryMSISDNOnTransientATPort = func(string) (string, error) {
		current := active.Add(1)
		for {
			max := maxActive.Load()
			if current <= max || maxActive.CompareAndSwap(max, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		return "+447700902003", nil
	}
	worker := &Worker{
		Config: config.DeviceConfig{ATPort: "/dev/ttyUSB9"},
		Backend: &workerPhoneBackendStub{
			workerStatusBackendStub: workerStatusBackendStub{mode: backend.BackendMBIM},
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := worker.AcquirePhoneNumber(context.Background(), true)
			if result.Err != nil || result.Number == "" {
				t.Errorf("AcquirePhoneNumber() result=%+v", result)
			}
		}()
	}
	wg.Wait()
	if maxActive.Load() != 1 {
		t.Fatalf("max concurrent transient AT sessions=%d, want 1", maxActive.Load())
	}
}

func TestTransientATWaitHonorsCanceledContext(t *testing.T) {
	worker := &Worker{Config: config.DeviceConfig{ATPort: "/dev/ttyUSB9"}}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = worker.WithTransientATPort(func(string) (string, error) {
			close(entered)
			<-release
			return "", nil
		})
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err := worker.WithTransientATPortContext(ctx, func(string) (string, error) {
		called = true
		return "", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithTransientATPortContext() error=%v, want context.Canceled", err)
	}
	if called {
		t.Fatal("canceled transient AT operation was executed")
	}

	close(release)
	<-done
}

func TestPersistIdentityStateStoresPhoneWithIMSIOnly(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	pool := NewPool(nil)
	worker := &Worker{
		ID: "phone-imsi-only",
		Backend: &workerPhoneBackendStub{
			workerStatusBackendStub: workerStatusBackendStub{mode: backend.BackendQMI},
			msisdn:                  "+447700902004",
		},
	}
	worker.state.Identity.IMSI = "234150000002004"

	pool.PersistIdentityState(worker)

	snapshot, err := db.GetPhoneNumberSnapshotByIMSIOrICCID("234150000002004", "")
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700902004" || snapshot.PhoneNumberSource != db.PhoneNumberSourceModem {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestRefreshIdentityLiveVerifiedDoesNotMixPartialIdentity(t *testing.T) {
	tests := []struct {
		name      string
		liveIMSI  string
		liveICCID string
		wantIMSI  string
		wantICCID string
	}{
		{
			name:      "IMSI only clears stale ICCID",
			liveIMSI:  "234150000002005",
			wantIMSI:  "234150000002005",
			wantICCID: "",
		},
		{
			name:      "ICCID only clears stale IMSI",
			liveICCID: "894400000000002006",
			wantIMSI:  "",
			wantICCID: "894400000000002006",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			worker := &Worker{
				ID: "phone-live-identity",
				Backend: &workerStartupIdentityBackendStub{
					workerPhoneBackendStub: workerPhoneBackendStub{
						workerStatusBackendStub: workerStatusBackendStub{mode: backend.BackendQMI},
					},
					liveIMSI:  tc.liveIMSI,
					liveICCID: tc.liveICCID,
				},
			}
			worker.state.Identity.IMSI = "stale-imsi"
			worker.state.Identity.ICCID = "stale-iccid"
			worker.state.Identity.Ready = true

			identity, err := worker.RefreshIdentityLiveVerified(context.Background(), "phone_number_action")
			if err != nil {
				t.Fatalf("RefreshIdentityLiveVerified() error=%v", err)
			}
			if identity.IMSI != tc.wantIMSI || identity.ICCID != tc.wantICCID {
				t.Fatalf("live identity=%+v, want IMSI=%q ICCID=%q", identity, tc.wantIMSI, tc.wantICCID)
			}
			status := worker.ProjectDeviceStatus()
			if status.IMSI != tc.wantIMSI || status.ICCID != tc.wantICCID {
				t.Fatalf("cached identity IMSI=%q ICCID=%q, want IMSI=%q ICCID=%q",
					status.IMSI, status.ICCID, tc.wantIMSI, tc.wantICCID)
			}
		})
	}
}

func TestRefreshIdentityLiveVerifiedRejectsMetadataOnlyRefresh(t *testing.T) {
	worker := &Worker{
		ID: "phone-live-identity-empty",
		Backend: &workerStartupIdentityBackendStub{
			liveNativeSPN: "metadata-without-identity",
		},
	}
	worker.state.Identity.IMSI = "existing-imsi"
	worker.state.Identity.ICCID = "existing-iccid"

	if _, err := worker.RefreshIdentityLiveVerified(context.Background(), "phone_number_action"); err == nil {
		t.Fatal("RefreshIdentityLiveVerified() expected an empty identity error")
	}
	status := worker.ProjectDeviceStatus()
	if status.IMSI != "existing-imsi" || status.ICCID != "existing-iccid" {
		t.Fatalf("failed verified refresh changed cached identity: IMSI=%q ICCID=%q", status.IMSI, status.ICCID)
	}
}
