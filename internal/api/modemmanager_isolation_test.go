package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/internal/hostconfig"
	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
)

type fakeHostConfigCoordinator struct {
	status               hostconfig.Status
	statusErr            error
	applyStatus          hostconfig.Status
	applyErr             error
	statusCalls          int
	expectedPlanRevision string
	applyCalls           int
	action               hostconfig.Action
	targets              []hostconfig.Target
	expectedRevision     string
}

func (f *fakeHostConfigCoordinator) Status(_ context.Context, targets []hostconfig.Target) (hostconfig.Status, error) {
	f.statusCalls++
	f.targets = append([]hostconfig.Target(nil), targets...)
	return f.status, f.statusErr
}

func (f *fakeHostConfigCoordinator) Apply(_ context.Context, action hostconfig.Action, targets []hostconfig.Target, expectedRevision, expectedPlanRevision string) (hostconfig.Status, error) {
	f.applyCalls++
	f.action = action
	f.targets = append([]hostconfig.Target(nil), targets...)
	f.expectedRevision = expectedRevision
	f.expectedPlanRevision = expectedPlanRevision
	return f.applyStatus, f.applyErr
}

func modemManagerIsolationHandlerRouter(coordinator hostconfig.Coordinator) (*Server, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		auth:       config.WebConfig{Username: "admin", Password: "current-secret"},
		hostConfig: coordinator,
		hostConfigTargetProvider: func() []hostconfig.Target {
			return []hostconfig.Target{{ID: "qmi-1", Name: "QMI 1", USBPath: "/sys/bus/usb/devices/1-2"}}
		},
	}
	router := gin.New()
	router.GET("/status", server.handleModemManagerIsolationStatus)
	router.POST("/install", server.handleInstallModemManagerIsolation)
	router.POST("/uninstall", server.handleUninstallModemManagerIsolation)
	return server, router
}

func TestModemManagerIsolationStatusReturnsPreview(t *testing.T) {
	fake := &fakeHostConfigCoordinator{status: hostconfig.Status{
		Status:          hostconfig.StateStale,
		RulePath:        mmisolation.DefaultRulePath,
		Revision:        "sha256:old",
		ManagedByVoHive: true,
		CanInstall:      true,
		PlanRevision:    "plan-preview",
		CanUninstall:    true,
		TotalDevices:    1,
		Devices: []hostconfig.DeviceStatus{{
			DeviceID: "qmi-1", SelectorKind: "usb_serial", SelectorValue: "ABC", Covered: false,
		}},
	}}
	_, router := modemManagerIsolationHandlerRouter(fake)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var status hostconfig.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != hostconfig.StateStale || status.Revision != "sha256:old" ||
		status.PlanRevision != "plan-preview" || len(status.Devices) != 1 {
		t.Fatalf("unexpected status: %#v", status)
	}
	if fake.statusCalls != 1 || len(fake.targets) != 1 || fake.targets[0].ID != "qmi-1" {
		t.Fatalf("coordinator preview calls=%d targets=%#v", fake.statusCalls, fake.targets)
	}
}

func TestModemManagerIsolationInstallRequiresPasswordAndForwardsCAS(t *testing.T) {
	fake := &fakeHostConfigCoordinator{applyStatus: hostconfig.Status{
		Status:         hostconfig.StatePendingReplug,
		Revision:       "sha256:new",
		RequiresReplug: true,
	}}
	_, router := modemManagerIsolationHandlerRouter(fake)

	for _, test := range []struct {
		name     string
		password string
		status   int
		calls    int
	}{
		{name: "missing", status: http.StatusForbidden},
		{name: "incorrect", password: "wrong", status: http.StatusForbidden},
		{name: "current", password: "current-secret", status: http.StatusOK, calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"current_password":%q,"expected_revision":"sha256:old","expected_plan_revision":"plan-preview"}`, test.password)
			request := httptest.NewRequest(http.MethodPost, "/install", bytes.NewBufferString(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
	if fake.applyCalls != 1 || fake.action != hostconfig.ActionInstall ||
		fake.expectedRevision != "sha256:old" || fake.expectedPlanRevision != "plan-preview" ||
		len(fake.targets) != 1 {
		t.Fatalf("unexpected apply: calls=%d action=%q revision=%q targets=%#v",
			fake.applyCalls, fake.action, fake.expectedRevision, fake.targets)
	}
}

func TestModemManagerIsolationActionRejectsMissingRevisionBeforePassword(t *testing.T) {
	fake := &fakeHostConfigCoordinator{}
	server, router := modemManagerIsolationHandlerRouter(fake)
	for _, body := range []string{
		`{"current_password":"wrong"}`,
		`{"current_password":"wrong","expected_revision":"   "}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/install", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
		}
	}
	if fake.applyCalls != 0 {
		t.Fatalf("missing revision reached coordinator %d times", fake.applyCalls)
	}
	if len(server.hostConfigAuthAttempts) != 0 {
		t.Fatal("missing revision consumed a password authorization attempt")
	}
}

func TestModemManagerIsolationActionRejectsUnknownRuleFields(t *testing.T) {
	fake := &fakeHostConfigCoordinator{}
	_, router := modemManagerIsolationHandlerRouter(fake)
	request := httptest.NewRequest(http.MethodPost, "/install", bytes.NewBufferString(
		`{"current_password":"current-secret","expected_revision":"absent","expected_plan_revision":"plan-preview","rule_path":"/tmp/owned","rule_text":"ENV{ID_MM_DEVICE_IGNORE}=1"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
	if fake.applyCalls != 0 {
		t.Fatalf("unknown rule fields reached coordinator %d times", fake.applyCalls)
	}
}

func TestModemManagerIsolationRevisionConflictIs409(t *testing.T) {
	fake := &fakeHostConfigCoordinator{applyErr: mmisolation.ErrRevisionConflict}
	_, router := modemManagerIsolationHandlerRouter(fake)
	request := httptest.NewRequest(http.MethodPost, "/uninstall", bytes.NewBufferString(
		`{"current_password":"current-secret","expected_revision":"sha256:stale","expected_plan_revision":"plan-preview"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", response.Code, response.Body.String())
	}
}

func TestModemManagerIsolationPlanConflictIs409(t *testing.T) {
	fake := &fakeHostConfigCoordinator{applyErr: hostconfig.ErrPlanConflict}
	_, router := modemManagerIsolationHandlerRouter(fake)
	request := httptest.NewRequest(http.MethodPost, "/install", bytes.NewBufferString(
		`{"current_password":"current-secret","expected_revision":"absent","expected_plan_revision":"plan-stale"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if fake.applyCalls != 1 || fake.expectedPlanRevision != "plan-stale" {
		t.Fatalf("plan conflict apply calls=%d plan=%q", fake.applyCalls, fake.expectedPlanRevision)
	}
}

func TestModemManagerIsolationRoutesRequireAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeHostConfigCoordinator{}
	server := &Server{hostConfig: fake}
	router := server.newRouter()
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/system/modemmanager/isolation"},
		{method: http.MethodPost, path: "/api/system/modemmanager/isolation/actions/install", body: `{}`},
		{method: http.MethodPost, path: "/api/system/modemmanager/isolation/actions/uninstall", body: `{}`},
	} {
		request := httptest.NewRequest(test.method, test.path, bytes.NewBufferString(test.body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 401", test.method, test.path, response.Code)
		}
	}
	if fake.statusCalls != 0 || fake.applyCalls != 0 {
		t.Fatalf("unauthenticated requests reached coordinator: status=%d apply=%d", fake.statusCalls, fake.applyCalls)
	}
}

func TestModemManagerIsolationReauthenticationIsRateLimited(t *testing.T) {
	fake := &fakeHostConfigCoordinator{}
	server, router := modemManagerIsolationHandlerRouter(fake)
	for attempt := 1; attempt <= hostConfigAuthorizationLimit+1; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/install", bytes.NewBufferString(
			`{"current_password":"wrong","expected_revision":"absent","expected_plan_revision":"plan-preview"}`,
		))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		want := http.StatusForbidden
		if attempt > hostConfigAuthorizationLimit {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("attempt %d status = %d, want %d", attempt, response.Code, want)
		}
	}
	if fake.applyCalls != 0 {
		t.Fatalf("failed authorization reached coordinator %d times", fake.applyCalls)
	}
	if len(server.hostConfigAuthAttempts) == 0 {
		t.Fatal("expected rate limiter state")
	}
}
func TestModemManagerIsolationCommittedCleanupWarningStaysSuccessfulAndVisible(t *testing.T) {
	const publicWarning = "配置已生效且 udev 规则已重新加载，但收尾清理未完整结束；请先使用当前安装器执行 --repair，再进行下一次宿主机配置变更。"
	fake := &fakeHostConfigCoordinator{applyStatus: hostconfig.Status{
		Status: hostconfig.StatePendingReplug, RequiresReplug: true, Warning: publicWarning,
	}}
	_, router := modemManagerIsolationHandlerRouter(fake)
	request := httptest.NewRequest(http.MethodPost, "/install", bytes.NewBufferString(
		`{"current_password":"current-secret","expected_revision":"absent","expected_plan_revision":"plan-preview"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var body struct {
		Warning string `json:"warning"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Warning != publicWarning || !strings.Contains(body.Message, publicWarning) {
		t.Fatalf("warning response = %#v", body)
	}
	if strings.Contains(body.Message, "private-token") || strings.Contains(body.Message, "/var/lib") {
		t.Fatalf("response leaked privileged cleanup detail: %#v", body)
	}
}

func TestModemManagerIsolationIndeterminateResultIs409(t *testing.T) {
	fake := &fakeHostConfigCoordinator{applyErr: hostconfig.ErrOperationIndeterminate}
	_, router := modemManagerIsolationHandlerRouter(fake)
	request := httptest.NewRequest(http.MethodPost, "/install", bytes.NewBufferString(
		`{"current_password":"current-secret","expected_revision":"absent","expected_plan_revision":"plan-preview"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", response.Code, response.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "host_config_indeterminate" {
		t.Fatalf("code = %q, want host_config_indeterminate", body["code"])
	}
}
