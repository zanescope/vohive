package db

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestManualPhoneOverridePreservesAutomaticValuesAndClearsToFallback(t *testing.T) {
	initPhoneNumberTestDB(t)
	imsi := "234150000001001"
	iccid := "894400000000001001"

	if err := RecordModemPhoneNumber(imsi, iccid, "+447700901001"); err != nil {
		t.Fatalf("RecordModemPhoneNumber() error=%v", err)
	}
	if err := RecordVoWiFiPhoneNumber(imsi, iccid, "+447700901002"); err != nil {
		t.Fatalf("RecordVoWiFiPhoneNumber() error=%v", err)
	}

	snapshot, err := SetManualPhoneNumber(imsi, iccid, "+447700901003")
	if err != nil {
		t.Fatalf("SetManualPhoneNumber() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700901003" || snapshot.PhoneNumberSource != PhoneNumberSourceManual {
		t.Fatalf("manual snapshot=%+v", snapshot)
	}

	if err := RecordModemPhoneNumber(imsi, iccid, "+447700901004"); err != nil {
		t.Fatalf("RecordModemPhoneNumber(update) error=%v", err)
	}
	if err := RecordVoWiFiPhoneNumber(imsi, iccid, "+447700901005"); err != nil {
		t.Fatalf("RecordVoWiFiPhoneNumber(update) error=%v", err)
	}
	snapshot, err = GetPhoneNumberSnapshotByIMSIOrICCID(imsi, iccid)
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700901003" || snapshot.PhoneNumberSource != PhoneNumberSourceManual {
		t.Fatalf("automatic update replaced manual snapshot=%+v", snapshot)
	}
	if snapshot.ModemPhoneNumber != "+447700901004" || snapshot.VowifiPhoneNumber != "+447700901005" {
		t.Fatalf("automatic candidates were not retained: %+v", snapshot)
	}

	snapshot, err = SetManualPhoneNumber(imsi, iccid, "")
	if err != nil {
		t.Fatalf("clear SetManualPhoneNumber() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700901005" || snapshot.PhoneNumberSource != PhoneNumberSourceVoWiFi {
		t.Fatalf("clear snapshot=%+v, want VoWiFi fallback", snapshot)
	}
}

func TestManualPhoneNumberRejectsInvalidValueWithoutOverwriting(t *testing.T) {
	initPhoneNumberTestDB(t)
	imsi := "234150000001002"

	if err := RecordModemPhoneNumber(imsi, "", "+447700901006"); err != nil {
		t.Fatalf("RecordModemPhoneNumber() error=%v", err)
	}
	if _, err := SetManualPhoneNumber(imsi, "", "not-a-number"); !errors.Is(err, ErrInvalidPhoneNumber) {
		t.Fatalf("SetManualPhoneNumber() error=%v, want ErrInvalidPhoneNumber", err)
	}
	snapshot, err := GetPhoneNumberSnapshotByIMSIOrICCID(imsi, "")
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700901006" || snapshot.PhoneNumberSource != PhoneNumberSourceModem {
		t.Fatalf("invalid manual write changed snapshot=%+v", snapshot)
	}
}

func TestPhoneSnapshotResolvesMigratedSubscriptionByICCID(t *testing.T) {
	initPhoneNumberTestDB(t)
	imsi := "234150000001003"
	iccid := "894400000000001003"
	imei := "860000000001003"

	if err := UpsertSIMCardIdentity(iccid, imsi, "TestOp", &imei); err != nil {
		t.Fatalf("UpsertSIMCardIdentity() error=%v", err)
	}
	if err := RecordVoWiFiPhoneNumber(imsi, iccid, "+447700901007"); err != nil {
		t.Fatalf("RecordVoWiFiPhoneNumber() error=%v", err)
	}

	snapshot, err := GetPhoneNumberSnapshotByIMSIOrICCID("", iccid)
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700901007" || snapshot.PhoneNumberSource != PhoneNumberSourceVoWiFi {
		t.Fatalf("ICCID snapshot=%+v", snapshot)
	}
}

func TestPhoneSnapshotDoesNotUseStaleIMSIForDifferentICCID(t *testing.T) {
	initPhoneNumberTestDB(t)
	oldIMSI := "234150000001004"
	newIMSI := "234150000001005"
	newICCID := "894400000000001005"
	imei := "860000000001005"

	if err := RecordVoWiFiPhoneNumber(oldIMSI, "", "+447700901008"); err != nil {
		t.Fatalf("RecordVoWiFiPhoneNumber(old) error=%v", err)
	}
	if err := UpsertSIMCardIdentity(newICCID, newIMSI, "TestOp", &imei); err != nil {
		t.Fatalf("UpsertSIMCardIdentity(new) error=%v", err)
	}

	snapshot, err := GetPhoneNumberSnapshotByIMSIOrICCID(oldIMSI, newICCID)
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "" || snapshot.PhoneNumberSource != PhoneNumberSourceNone {
		t.Fatalf("stale IMSI leaked previous SIM snapshot=%+v", snapshot)
	}
}

func TestPhoneSnapshotPrefersMatchingLivePairOverStaleSIMMapping(t *testing.T) {
	initPhoneNumberTestDB(t)
	oldIMSI := "234150000001011"
	newIMSI := "234150000001012"
	iccid := "894400000000001011"
	imei := "860000000001011"

	if err := UpsertSIMCardIdentity(iccid, oldIMSI, "TestOp", &imei); err != nil {
		t.Fatalf("UpsertSIMCardIdentity() error=%v", err)
	}
	if err := RecordModemPhoneNumber(oldIMSI, iccid, "+447700901015"); err != nil {
		t.Fatalf("RecordModemPhoneNumber(old) error=%v", err)
	}
	if _, err := SetManualPhoneNumber(newIMSI, iccid, "+447700901016"); err != nil {
		t.Fatalf("SetManualPhoneNumber(new) error=%v", err)
	}

	snapshot, err := GetPhoneNumberSnapshotByIMSIOrICCID(newIMSI, iccid)
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700901016" || snapshot.PhoneNumberSource != PhoneNumberSourceManual {
		t.Fatalf("snapshot=%+v, want matching live IMSI/ICCID subscription", snapshot)
	}
}

func TestPendingManualPhoneMigratesAtomically(t *testing.T) {
	initPhoneNumberTestDB(t)
	imsi := "234150000001006"
	iccid := "894400000000001006"
	imei := "860000000001006"

	if err := RecordVoWiFiPhoneNumber("", iccid, "+447700901009"); err != nil {
		t.Fatalf("RecordVoWiFiPhoneNumber() error=%v", err)
	}
	if _, err := SetManualPhoneNumber("", iccid, "+447700901010"); err != nil {
		t.Fatalf("SetManualPhoneNumber(pending) error=%v", err)
	}
	if err := UpsertSIMCard(iccid, imsi, "", "TestOp", &imei); err != nil {
		t.Fatalf("UpsertSIMCard() error=%v", err)
	}

	snapshot, err := GetPhoneNumberSnapshotByIMSIOrICCID(imsi, iccid)
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700901010" || snapshot.PhoneNumberSource != PhoneNumberSourceManual {
		t.Fatalf("migrated snapshot=%+v", snapshot)
	}
	if snapshot.VowifiPhoneNumber != "+447700901009" {
		t.Fatalf("migrated automatic value lost: %+v", snapshot)
	}
	var count int64
	if err := DB.Model(&PendingPhoneNumber{}).Where("iccid = ?", iccid).Count(&count).Error; err != nil {
		t.Fatalf("Count(pending) error=%v", err)
	}
	if count != 0 {
		t.Fatalf("pending rows=%d, want 0", count)
	}
}

func TestOlderPendingManualPhoneDoesNotReplaceNewerSubscriptionManual(t *testing.T) {
	initPhoneNumberTestDB(t)
	imsi := "234150000001013"
	iccid := "894400000000001013"

	if _, err := SetManualPhoneNumber("", iccid, "+447700901017"); err != nil {
		t.Fatalf("SetManualPhoneNumber(pending) error=%v", err)
	}
	if err := DB.Model(&PendingPhoneNumber{}).
		Where("iccid = ?", iccid).
		Update("updated_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatalf("age pending manual error=%v", err)
	}
	if _, err := SetManualPhoneNumber(imsi, "", "+447700901018"); err != nil {
		t.Fatalf("SetManualPhoneNumber(subscription) error=%v", err)
	}

	if err := migratePendingPhoneToSubscriptionAtomic(imsi, iccid); err != nil {
		t.Fatalf("migratePendingPhoneToSubscriptionAtomic() error=%v", err)
	}
	snapshot, err := GetPhoneNumberSnapshotByIMSIOrICCID(imsi, iccid)
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700901018" || snapshot.PhoneNumberSource != PhoneNumberSourceManual {
		t.Fatalf("snapshot=%+v, want newer subscription manual", snapshot)
	}
	var count int64
	if err := DB.Model(&PendingPhoneNumber{}).Where("iccid = ?", iccid).Count(&count).Error; err != nil {
		t.Fatalf("Count(pending) error=%v", err)
	}
	if count != 0 {
		t.Fatalf("pending rows=%d, want 0", count)
	}
}

func TestAutomaticPhoneWriteLinksCurrentICCID(t *testing.T) {
	initPhoneNumberTestDB(t)
	imsi := "234150000001009"
	iccid := "894400000000001009"

	if err := RecordModemPhoneNumber(imsi, iccid, "+447700901014"); err != nil {
		t.Fatalf("RecordModemPhoneNumber() error=%v", err)
	}
	var sub SIMSubscription
	if err := DB.Where("imsi = ?", imsi).First(&sub).Error; err != nil {
		t.Fatalf("First(subscription) error=%v", err)
	}
	if sub.CurrentICCID != iccid {
		t.Fatalf("CurrentICCID=%q, want %q", sub.CurrentICCID, iccid)
	}
}

func TestIdentityOnlySubscriptionStartsWithNoneSource(t *testing.T) {
	initPhoneNumberTestDB(t)
	imsi := "234150000001010"
	iccid := "894400000000001010"
	imei := "860000000001010"

	if err := UpsertSIMCardIdentity(iccid, imsi, "TestOp", &imei); err != nil {
		t.Fatalf("UpsertSIMCardIdentity() error=%v", err)
	}
	var sub SIMSubscription
	if err := DB.Where("imsi = ?", imsi).First(&sub).Error; err != nil {
		t.Fatalf("First(subscription) error=%v", err)
	}
	if sub.PhoneNumberSource != PhoneNumberSourceNone {
		t.Fatalf("PhoneNumberSource=%q, want %q", sub.PhoneNumberSource, PhoneNumberSourceNone)
	}
}

func TestConcurrentAutomaticPhoneUpdatesKeepVoWiFiPriority(t *testing.T) {
	initPhoneNumberTestDB(t)
	imsi := "234150000001007"

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := RecordModemPhoneNumber(imsi, "", "+447700901011"); err != nil {
				t.Errorf("RecordModemPhoneNumber() error=%v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := RecordVoWiFiPhoneNumber(imsi, "", "+447700901012"); err != nil {
				t.Errorf("RecordVoWiFiPhoneNumber() error=%v", err)
			}
		}()
	}
	wg.Wait()

	snapshot, err := GetPhoneNumberSnapshotByIMSIOrICCID(imsi, "")
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700901012" || snapshot.PhoneNumberSource != PhoneNumberSourceVoWiFi {
		t.Fatalf("concurrent snapshot=%+v", snapshot)
	}
}

func TestPhoneNumberSourceBackfillPreservesLegacyValue(t *testing.T) {
	old := DB
	dbPath := filepath.Join(t.TempDir(), "phone_source_backfill.db")
	if err := Init(dbPath); err != nil {
		t.Fatalf("Init() error=%v", err)
	}
	t.Cleanup(func() {
		closePhoneNumberTestDB(t)
		DB = old
	})

	now := time.Now()
	if err := DB.Exec(
		"INSERT INTO sim_subscriptions (imsi, phone_number, phone_number_source, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"234150000001008", "+447700901013", "", now, now,
	).Error; err != nil {
		t.Fatalf("seed subscription error=%v", err)
	}
	closePhoneNumberTestDB(t)
	if err := Init(dbPath); err != nil {
		t.Fatalf("re-Init() error=%v", err)
	}

	snapshot, err := GetPhoneNumberSnapshotByIMSIOrICCID("234150000001008", "")
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700901013" || snapshot.PhoneNumberSource != PhoneNumberSourceLegacy {
		t.Fatalf("backfilled snapshot=%+v", snapshot)
	}
}
