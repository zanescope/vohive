package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/internal/device"
)

type deviceConfigApplySpy struct {
	updateCalls  int
	rebuildCalls int
	networkCalls int
}

func (s *deviceConfigApplySpy) UpdateWorkerConfig(string, config.DeviceConfig, bool) bool {
	s.updateCalls++
	return true
}

func (s *deviceConfigApplySpy) RebuildWorker(string) error {
	s.rebuildCalls++
	return nil
}

func (s *deviceConfigApplySpy) ApplyConfiguredNetwork(string) error {
	s.networkCalls++
	return nil
}

func TestHostNetworkBackupOnlySaveDoesNotTouchWorker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initPolicyTestDB(t)

	const deviceID = "device02"
	path := writeDeviceMgmtLimitConfig(t, `
server:
  port: ":7575"
devices:
  - id: device02
    name: device02
    modem_imei: "866069053675384"
    device_backend: qmi
    mbim_transport: proxy
    usbnet_mode: 2
    esim_switch:
      use_refresh_true: true
      event_gated_converge: true
      reinit_window_ms: 12000
`)
	if err := config.InitGlobalManager(path); err != nil {
		t.Fatalf("InitGlobalManager() error = %v", err)
	}

	persisted, err := config.GetDeviceByID(deviceID)
	if err != nil {
		t.Fatalf("GetDeviceByID() error = %v", err)
	}
	if persisted == nil {
		t.Fatal("GetDeviceByID() = nil")
	}

	runtimeCfg := *persisted
	runtimeCfg.USBPath = "/sys/bus/usb/devices/1-2"
	runtimeCfg.ATPort = "/dev/ttyUSB6"
	runtimeCfg.ManagePort = "/dev/ttyUSB6"
	runtimeCfg.Interface = "wwan1"
	runtimeCfg.QMIDevice = "/dev/cdc-wdm1"
	runtimeCfg.ControlDevice = "/dev/cdc-wdm1"
	runtimeCfg.AudioDevice = "hw:2,0"

	pool := device.NewPool(&config.Config{})
	injectWorker(pool, &device.Worker{ID: deviceID, Config: runtimeCfg})
	applySpy := &deviceConfigApplySpy{}
	server := &Server{
		pool:        pool,
		configPath:  path,
		configApply: applySpy,
	}

	getRecorder := httptest.NewRecorder()
	getContext, _ := gin.CreateTestContext(getRecorder)
	getContext.Params = gin.Params{{Key: "device_id", Value: deviceID}}
	getContext.Request = httptest.NewRequest(http.MethodGet, "/devices/"+deviceID+"/config", nil)
	server.handleDeviceMgmtGetDeviceConfig(getContext)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getRecorder.Code, getRecorder.Body.String())
	}

	var getResponse struct {
		Config deviceConfigDTO `json:"config"`
	}
	if err := json.Unmarshal(getRecorder.Body.Bytes(), &getResponse); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if getResponse.Config.Interface != runtimeCfg.Interface ||
		getResponse.Config.ControlDevice != runtimeCfg.ControlDevice ||
		getResponse.Config.ATPort != runtimeCfg.ATPort ||
		getResponse.Config.USBPath != runtimeCfg.USBPath {
		t.Fatalf("GET did not expose runtime attachment: %+v", getResponse.Config)
	}

	getResponse.Config.HostNetworkBackup = boolPtr(true)
	body, err := json.Marshal(updateDeviceRequest{Config: getResponse.Config})
	if err != nil {
		t.Fatalf("encode PUT request: %v", err)
	}
	putRecorder := httptest.NewRecorder()
	putContext, _ := gin.CreateTestContext(putRecorder)
	putContext.Params = gin.Params{{Key: "device_id", Value: deviceID}}
	putContext.Request = httptest.NewRequest(http.MethodPut, "/devices/"+deviceID, bytes.NewReader(body))
	putContext.Request.Header.Set("Content-Type", "application/json")
	server.handleDeviceMgmtUpdateDevice(putContext)

	if putRecorder.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", putRecorder.Code, putRecorder.Body.String())
	}
	var putResponse struct {
		RequiresRestart bool `json:"requires_restart"`
	}
	if err := json.Unmarshal(putRecorder.Body.Bytes(), &putResponse); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	if putResponse.RequiresRestart {
		t.Fatalf("requires_restart=true body=%s", putRecorder.Body.String())
	}
	if applySpy.updateCalls != 0 || applySpy.rebuildCalls != 0 || applySpy.networkCalls != 0 {
		t.Fatalf("backup-only save touched runtime: update=%d rebuild=%d network=%d",
			applySpy.updateCalls, applySpy.rebuildCalls, applySpy.networkCalls)
	}

	gotConfig := config.GetConfig()
	if len(gotConfig.HostFailover.CandidateDeviceIDs) != 1 ||
		gotConfig.HostFailover.CandidateDeviceIDs[0] != deviceID {
		t.Fatalf("candidate_device_ids=%v, want [%s]",
			gotConfig.HostFailover.CandidateDeviceIDs, deviceID)
	}
	saved, err := config.GetDeviceByID(deviceID)
	if err != nil {
		t.Fatalf("GetDeviceByID(saved) error = %v", err)
	}
	if saved == nil {
		t.Fatal("saved device = nil")
	}
	if saved.USBPath != "" || saved.ATPort != "" || saved.ManagePort != "" ||
		saved.Interface != "" || saved.QMIDevice != "" || saved.ControlDevice != "" ||
		saved.AudioDevice != "" {
		t.Fatalf("runtime attachment leaked into persisted config: %+v", *saved)
	}
	if saved.MBIMTransport != "proxy" || saved.USBNetMode == nil || *saved.USBNetMode != 2 ||
		!saved.ESIMSwitch.UseRefreshTrue || !saved.ESIMSwitch.EventGatedConverge ||
		saved.ESIMSwitch.ReinitWindowMS != 12000 {
		t.Fatalf("fields outside Web form were not preserved: %+v", *saved)
	}

	// Positive control: a real QMI transport change must still update and
	// rebuild the Worker, then apply the configured network.
	getResponse.Config.QMIUseProxy = boolPtr(true)
	qmiBody, err := json.Marshal(updateDeviceRequest{Config: getResponse.Config})
	if err != nil {
		t.Fatalf("encode QMI PUT request: %v", err)
	}
	qmiRecorder := httptest.NewRecorder()
	qmiContext, _ := gin.CreateTestContext(qmiRecorder)
	qmiContext.Params = gin.Params{{Key: "device_id", Value: deviceID}}
	qmiContext.Request = httptest.NewRequest(http.MethodPut, "/devices/"+deviceID, bytes.NewReader(qmiBody))
	qmiContext.Request.Header.Set("Content-Type", "application/json")
	server.handleDeviceMgmtUpdateDevice(qmiContext)

	if qmiRecorder.Code != http.StatusOK {
		t.Fatalf("QMI PUT status=%d body=%s", qmiRecorder.Code, qmiRecorder.Body.String())
	}
	var qmiResponse struct {
		RequiresRestart bool `json:"requires_restart"`
	}
	if err := json.Unmarshal(qmiRecorder.Body.Bytes(), &qmiResponse); err != nil {
		t.Fatalf("decode QMI PUT response: %v", err)
	}
	if !qmiResponse.RequiresRestart {
		t.Fatalf("QMI proxy change requires_restart=false body=%s", qmiRecorder.Body.String())
	}
	if applySpy.updateCalls != 1 || applySpy.rebuildCalls != 1 || applySpy.networkCalls != 1 {
		t.Fatalf("QMI proxy change runtime calls: update=%d rebuild=%d network=%d, want 1/1/1",
			applySpy.updateCalls, applySpy.rebuildCalls, applySpy.networkCalls)
	}
}
