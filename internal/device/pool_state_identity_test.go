package device

import (
	"runtime"
	"testing"
)

func TestSIMIdentityForPhoneLookupStatePolicy(t *testing.T) {
	tests := []struct {
		name      string
		ready     bool
		phase     string
		imsi      string
		iccid     string
		wantIMSI  string
		wantICCID string
		wantOK    bool
	}{
		{
			name:      "ready identity",
			ready:     true,
			phase:     simIdentityPhaseReady,
			imsi:      " 460001234567890 ",
			iccid:     " 8986000000000000001 ",
			wantIMSI:  "460001234567890",
			wantICCID: "8986000000000000001",
			wantOK:    true,
		},
		{
			name:      "ready iccid only",
			ready:     true,
			phase:     simIdentityPhaseReady,
			iccid:     " 8986000000000000002 ",
			wantICCID: "8986000000000000002",
			wantOK:    true,
		},
		{
			name:  "not ready",
			phase: simIdentityPhaseReady,
			imsi:  "460001234567890",
			iccid: "8986000000000000001",
		},
		{
			name:  "transitioning",
			ready: true,
			phase: simIdentityPhaseTransitioning,
			imsi:  "460001234567890",
			iccid: "8986000000000000001",
		},
		{
			name:  "degraded",
			ready: true,
			phase: simIdentityPhaseDegraded,
			imsi:  "460001234567890",
			iccid: "8986000000000000001",
		},
		{
			name:  "unknown future phase",
			ready: true,
			phase: "future_phase",
			imsi:  "460001234567890",
			iccid: "8986000000000000001",
		},
		{
			name:  "empty ready identity",
			ready: true,
			phase: simIdentityPhaseReady,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := &Worker{}
			worker.state.Identity = deviceIdentityState{
				Ready: tt.ready,
				Phase: tt.phase,
				IMSI:  tt.imsi,
				ICCID: tt.iccid,
			}
			imsi, iccid, ok := worker.SIMIdentityForPhoneLookup()
			if imsi != tt.wantIMSI || iccid != tt.wantICCID || ok != tt.wantOK {
				t.Fatalf("SIMIdentityForPhoneLookup()=(%q,%q,%t), want (%q,%q,%t)",
					imsi, iccid, ok, tt.wantIMSI, tt.wantICCID, tt.wantOK)
			}
		})
	}

	t.Run("nil worker", func(t *testing.T) {
		var worker *Worker
		imsi, iccid, ok := worker.SIMIdentityForPhoneLookup()
		if imsi != "" || iccid != "" || ok {
			t.Fatalf("SIMIdentityForPhoneLookup()=(%q,%q,%t), want empty unusable identity", imsi, iccid, ok)
		}
	})
}

func TestSIMIdentityForPhoneLookupKeepsGenerationPairAtomic(t *testing.T) {
	const iterations = 10000
	worker := &Worker{}
	setIdentity := func(imsi, iccid string) {
		worker.cacheMu.Lock()
		worker.state.Identity.Ready = true
		worker.state.Identity.Phase = simIdentityPhaseReady
		worker.state.Identity.IMSI = imsi
		worker.state.Identity.ICCID = iccid
		worker.cacheMu.Unlock()
	}
	setIdentity("old-imsi", "old-iccid")

	start := make(chan struct{})
	done := make(chan struct{})
	go func() {
		<-start
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				setIdentity("new-imsi", "new-iccid")
			} else {
				setIdentity("old-imsi", "old-iccid")
			}
			runtime.Gosched()
		}
		close(done)
	}()

	close(start)
	for i := 0; i < iterations; i++ {
		imsi, iccid, ok := worker.SIMIdentityForPhoneLookup()
		if !ok {
			t.Fatal("SIMIdentityForPhoneLookup() returned unusable ready identity")
		}
		oldPair := imsi == "old-imsi" && iccid == "old-iccid"
		newPair := imsi == "new-imsi" && iccid == "new-iccid"
		if !oldPair && !newPair {
			t.Fatalf("SIMIdentityForPhoneLookup() mixed generations: imsi=%q iccid=%q", imsi, iccid)
		}
		runtime.Gosched()
	}
	<-done
}
