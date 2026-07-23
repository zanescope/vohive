package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/internal/backend"
	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/internal/db"
	"github.com/zanescope/vohive/internal/device"
)

type phoneNumberActionBackendStub struct {
	ussdDeviceBackendStub
	imsi   string
	iccid  string
	msisdn string
}

func (s *phoneNumberActionBackendStub) GetIMSILive(context.Context) (string, error) {
	return s.imsi, nil
}

func (s *phoneNumberActionBackendStub) GetICCIDLive(context.Context) (string, error) {
	return s.iccid, nil
}

func (s *phoneNumberActionBackendStub) GetMSISDN(context.Context) (string, error) {
	return s.msisdn, nil
}

func newPhoneNumberActionServer(t *testing.T, backendStub *phoneNumberActionBackendStub) (*Server, *device.Worker) {
	t.Helper()
	pool := device.NewPool(&config.Config{})
	worker := &device.Worker{ID: "phone-action-device", Backend: backendStub}
	setNestedPrivateField(t, pool, []string{"workers"}, map[string]*device.Worker{worker.ID: worker})
	return &Server{pool: pool}, worker
}

func performPhoneNumberAction(t *testing.T, server *Server, method, target, body string, handler gin.HandlerFunc) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "device_id", Value: "phone-action-device"}}
	ctx.Request = httptest.NewRequest(method, target, bytes.NewBufferString(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler(ctx)

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return recorder.Code, payload
}

func TestHandleDeviceMgmtSetPhoneNumberUsesLiveSIMIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initDeviceMgmtPhoneTestDB(t)
	server, _ := newPhoneNumberActionServer(t, &phoneNumberActionBackendStub{
		ussdDeviceBackendStub: ussdDeviceBackendStub{mode: backend.BackendQMI},
		imsi:                  "234150000003001",
		iccid:                 "894400000000003001",
	})

	status, payload := performPhoneNumberAction(
		t,
		server,
		http.MethodPatch,
		"/devices/phone-action-device/phone-number",
		`{"manual_phone_number":"+447700903001"}`,
		server.handleDeviceMgmtSetPhoneNumber,
	)
	if status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	if payload["local_phone"] != "+447700903001" || payload["local_phone_source"] != db.PhoneNumberSourceManual {
		t.Fatalf("payload=%v", payload)
	}

	snapshot, err := db.GetPhoneNumberSnapshotByIMSIOrICCID("234150000003001", "894400000000003001")
	if err != nil {
		t.Fatalf("GetPhoneNumberSnapshotByIMSIOrICCID() error=%v", err)
	}
	if snapshot.PhoneNumber != "+447700903001" || snapshot.PhoneNumberSource != db.PhoneNumberSourceManual {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestHandleDeviceMgmtSetPhoneNumberClearsToAutomaticFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initDeviceMgmtPhoneTestDB(t)
	const imsi = "234150000003002"
	const iccid = "894400000000003002"
	if err := db.RecordModemPhoneNumber(imsi, iccid, "+447700903002"); err != nil {
		t.Fatalf("RecordModemPhoneNumber() error=%v", err)
	}
	if _, err := db.SetManualPhoneNumber(imsi, iccid, "+447700903003"); err != nil {
		t.Fatalf("SetManualPhoneNumber() error=%v", err)
	}
	server, _ := newPhoneNumberActionServer(t, &phoneNumberActionBackendStub{
		ussdDeviceBackendStub: ussdDeviceBackendStub{mode: backend.BackendQMI},
		imsi:                  imsi,
		iccid:                 iccid,
	})

	status, payload := performPhoneNumberAction(
		t,
		server,
		http.MethodPatch,
		"/devices/phone-action-device/phone-number",
		`{"manual_phone_number":""}`,
		server.handleDeviceMgmtSetPhoneNumber,
	)
	if status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	if payload["local_phone"] != "+447700903002" || payload["local_phone_source"] != db.PhoneNumberSourceModem {
		t.Fatalf("payload=%v", payload)
	}
}

func TestHandleDeviceMgmtRefreshPhoneNumberPersistsBackendResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initDeviceMgmtPhoneTestDB(t)
	server, _ := newPhoneNumberActionServer(t, &phoneNumberActionBackendStub{
		ussdDeviceBackendStub: ussdDeviceBackendStub{mode: backend.BackendQMI},
		imsi:                  "234150000003004",
		iccid:                 "894400000000003004",
		msisdn:                "+447700903004",
	})

	status, payload := performPhoneNumberAction(
		t,
		server,
		http.MethodPost,
		"/devices/phone-action-device/actions/refresh-phone-number",
		"",
		server.handleDeviceMgmtRefreshPhoneNumber,
	)
	if status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	if payload["acquired"] != true || payload["local_phone"] != "+447700903004" {
		t.Fatalf("payload=%v", payload)
	}
	if payload["local_phone_source"] != db.PhoneNumberSourceModem || payload["channel"] != backend.BackendQMI {
		t.Fatalf("payload=%v", payload)
	}
}

func TestHandleDeviceMgmtSetPhoneNumberRequiresRequestField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initDeviceMgmtPhoneTestDB(t)
	server, _ := newPhoneNumberActionServer(t, &phoneNumberActionBackendStub{})

	status, _ := performPhoneNumberAction(
		t,
		server,
		http.MethodPatch,
		"/devices/phone-action-device/phone-number",
		`{}`,
		server.handleDeviceMgmtSetPhoneNumber,
	)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d", status, http.StatusBadRequest)
	}
}

func TestPhoneNumberActionsRejectDuringESIMSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initDeviceMgmtPhoneTestDB(t)
	server, _ := newPhoneNumberActionServer(t, &phoneNumberActionBackendStub{
		ussdDeviceBackendStub: ussdDeviceBackendStub{mode: backend.BackendQMI},
		imsi:                  "234150000003005",
		iccid:                 "894400000000003005",
	})
	setNestedPrivateField(t, server.pool, []string{"switchingDevices"}, map[string]bool{
		"phone-action-device": true,
	})

	tests := []struct {
		name    string
		method  string
		target  string
		body    string
		handler gin.HandlerFunc
	}{
		{
			name:    "manual",
			method:  http.MethodPatch,
			target:  "/devices/phone-action-device/phone-number",
			body:    "{\"manual_phone_number\":\"+447700903005\"}",
			handler: server.handleDeviceMgmtSetPhoneNumber,
		},
		{
			name:    "refresh",
			method:  http.MethodPost,
			target:  "/devices/phone-action-device/actions/refresh-phone-number",
			handler: server.handleDeviceMgmtRefreshPhoneNumber,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := performPhoneNumberAction(t, server, tc.method, tc.target, tc.body, tc.handler)
			if status != http.StatusConflict {
				t.Fatalf("status=%d want=%d", status, http.StatusConflict)
			}
		})
	}
}

func TestPartialLiveIdentityKeepsOverviewOnUpdatedPhoneNumber(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initDeviceMgmtPhoneTestDB(t)
	backendStub := &phoneNumberActionBackendStub{
		ussdDeviceBackendStub: ussdDeviceBackendStub{mode: backend.BackendQMI},
		imsi:                  "234150000003006",
		iccid:                 "894400000000003006",
	}
	server, worker := newPhoneNumberActionServer(t, backendStub)
	if _, err := worker.RefreshIdentityLiveVerified(context.Background(), "seed_old_identity"); err != nil {
		t.Fatalf("seed RefreshIdentityLiveVerified() error=%v", err)
	}
	if _, err := db.SetManualPhoneNumber(
		"234150000003006",
		"894400000000003006",
		"+447700903006",
	); err != nil {
		t.Fatalf("seed SetManualPhoneNumber() error=%v", err)
	}

	backendStub.imsi = "234150000003007"
	backendStub.iccid = ""
	status, payload := performPhoneNumberAction(
		t,
		server,
		http.MethodPatch,
		"/devices/phone-action-device/phone-number",
		"{\"manual_phone_number\":\"+447700903007\"}",
		server.handleDeviceMgmtSetPhoneNumber,
	)
	if status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	if payload["local_phone"] != "+447700903007" {
		t.Fatalf("payload=%v", payload)
	}

	projected := worker.ProjectDeviceStatus()
	if projected.IMSI != "234150000003007" || projected.ICCID != "" {
		t.Fatalf("projected identity IMSI=%q ICCID=%q", projected.IMSI, projected.ICCID)
	}
	item := server.buildOverviewLiteDetailItemFromWorker(
		worker,
		config.DeviceConfig{ID: worker.ID},
		projected,
		nil,
	)
	if item.LocalPhone != "+447700903007" || item.LocalPhoneSource != db.PhoneNumberSourceManual {
		t.Fatalf("overview phone=%q source=%q", item.LocalPhone, item.LocalPhoneSource)
	}
}
