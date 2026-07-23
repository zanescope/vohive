package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/internal/updater"
)

type fakeUpdateCoordinator struct {
	capabilities updater.Capabilities
	candidate    updater.Candidate
	startState   updater.TransactionState
	state        updater.TransactionState
	startRequest updater.UpdateRequest
	capErr       error
	checkErr     error
	startErr     error
	stateErr     error
}

func (f *fakeUpdateCoordinator) Capabilities(context.Context) (updater.Capabilities, error) {
	return f.capabilities, f.capErr
}

func (f *fakeUpdateCoordinator) Check(context.Context, updater.CheckRequest) (updater.Candidate, error) {
	return f.candidate, f.checkErr
}

func (f *fakeUpdateCoordinator) Start(_ context.Context, request updater.UpdateRequest) (updater.TransactionState, error) {
	f.startRequest = request
	return f.startState, f.startErr
}

func (f *fakeUpdateCoordinator) State(context.Context, string) (updater.TransactionState, error) {
	return f.state, f.stateErr
}

func updateHandlerRouter(coordinator updater.Coordinator) *gin.Engine {
	gin.SetMode(gin.TestMode)
	server := &Server{
		auth:    config.WebConfig{Username: "admin", Password: "current-secret"},
		updates: coordinator,
	}
	router := gin.New()
	router.GET("/capabilities", server.handleUpdateCapabilities)
	router.GET("/check", server.handleUpdateCheck)
	router.POST("/jobs", server.handleStartUpdateJob)
	router.GET("/jobs/:job_id", server.handleUpdateJobState)
	return router
}

func TestUpdateCheckReturnsCapabilitiesAndSignedCandidate(t *testing.T) {
	fake := &fakeUpdateCoordinator{
		capabilities: updater.Capabilities{InstallType: updater.InstallSystemd, CanCheck: true, CanUpdate: true},
		candidate: updater.Candidate{
			HasUpdate: true, CurrentVer: "v1.5.5", LatestVer: "v1.6.0", ReleaseNote: "notes",
		},
	}
	response := httptest.NewRecorder()
	updateHandlerRouter(fake).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/check?channel=stable", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var body updateCheckResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Candidate == nil || body.Candidate.LatestVer != "v1.6.0" {
		t.Fatalf("candidate = %#v, want v1.6.0", body.Candidate)
	}
	if !body.Capabilities.CanUpdate {
		t.Fatal("expected native update capability")
	}
}

func TestUpdateCheckExplainsUnsupportedDeploymentWithoutResolving(t *testing.T) {
	fake := &fakeUpdateCoordinator{
		capabilities: updater.Capabilities{
			InstallType: updater.InstallPortable,
			CanCheck:    false,
			CanUpdate:   false,
			Reason:      "external restart hook required",
		},
		checkErr: errors.New("check must not be called"),
	}
	response := httptest.NewRecorder()
	updateHandlerRouter(fake).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/check", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var body updateCheckResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Candidate != nil || body.Capabilities.Reason == "" {
		t.Fatalf("unexpected response: %#v", body)
	}
}

func TestStartUpdateJobUsesExactVersionWithoutChangingChannel(t *testing.T) {
	fake := &fakeUpdateCoordinator{
		startState: updater.TransactionState{Schema: 1, ID: "job-1", Phase: updater.PhaseChecking},
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(`{"channel":"stable","version":"v1.6.0","current_password":"current-secret"}`))
	request.Header.Set("Content-Type", "application/json")
	updateHandlerRouter(fake).ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusAccepted, response.Body.String())
	}
	if fake.startRequest.Channel != updater.ChannelStable || fake.startRequest.Version != "v1.6.0" {
		t.Fatalf("request = %#v", fake.startRequest)
	}
}

func TestStartUpdateJobReportsConcurrentTransaction(t *testing.T) {
	fake := &fakeUpdateCoordinator{startErr: updater.ErrUpdateLocked}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(`{"channel":"stable","version":"v1.6.0","current_password":"current-secret"}`))
	request.Header.Set("Content-Type", "application/json")
	updateHandlerRouter(fake).ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
}

func TestUpdateJobStateNotFound(t *testing.T) {
	fake := &fakeUpdateCoordinator{stateErr: updater.ErrJobNotFound}
	response := httptest.NewRecorder()
	updateHandlerRouter(fake).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/jobs/job-404", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestRouterDoesNotExposeRemoteUninstall(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range (&Server{}).newRouter().Routes() {
		if route.Path == "/api/system/uninstall" {
			t.Fatalf("dangerous remote uninstall route is still registered: %#v", route)
		}
	}
}
