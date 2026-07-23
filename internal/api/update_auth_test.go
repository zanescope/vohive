package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/internal/updater"
)

type countingUpdateCoordinator struct {
	calls int
}

func (f *countingUpdateCoordinator) Capabilities(context.Context) (updater.Capabilities, error) {
	f.calls++
	return updater.Capabilities{}, nil
}

func (f *countingUpdateCoordinator) Check(context.Context, updater.CheckRequest) (updater.Candidate, error) {
	f.calls++
	return updater.Candidate{}, nil
}

func (f *countingUpdateCoordinator) Start(context.Context, updater.UpdateRequest) (updater.TransactionState, error) {
	f.calls++
	return updater.TransactionState{}, nil
}

func (f *countingUpdateCoordinator) State(context.Context, string) (updater.TransactionState, error) {
	f.calls++
	return updater.TransactionState{}, nil
}

func TestUpdateRoutesRequireAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	coordinator := &countingUpdateCoordinator{}
	router := (&Server{updates: coordinator}).newRouter()

	tests := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/system/update/capabilities"},
		{method: http.MethodGet, path: "/api/system/update/check"},
		{method: http.MethodPost, path: "/api/system/update/jobs", body: `{"channel":"stable","version":"v1.6.0"}`},
		{method: http.MethodGet, path: "/api/system/update/jobs/job-1"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, bytes.NewBufferString(test.body))
		if test.body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want %d", test.method, test.path, response.Code, http.StatusUnauthorized)
		}
	}
	if coordinator.calls != 0 {
		t.Fatalf("unauthenticated requests reached update coordinator %d times", coordinator.calls)
	}
}

func TestUpdateStartRequiresCurrentPasswordWithAValidSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	coordinator := &countingUpdateCoordinator{}
	server := &Server{
		auth:    config.WebConfig{Username: "admin", Password: "current-secret"},
		updates: coordinator,
	}
	router := server.newRouter()
	token, _, err := server.issueSessionToken()
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		password string
		status   int
	}{
		{name: "missing", status: http.StatusForbidden},
		{name: "incorrect", password: "wrong-secret", status: http.StatusForbidden},
		{name: "current", password: "current-secret", status: http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"channel":"stable","version":"v1.6.0","current_password":"` + test.password + `"}`
			request := httptest.NewRequest(http.MethodPost, "/api/system/update/jobs", bytes.NewBufferString(body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
	if coordinator.calls != 1 {
		t.Fatalf("update coordinator calls = %d, want only the reauthenticated request", coordinator.calls)
	}
}

func TestUpdateReauthenticationFailuresAreRateLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	coordinator := &countingUpdateCoordinator{}
	server := &Server{
		auth:    config.WebConfig{Username: "admin", Password: "current-secret"},
		updates: coordinator,
	}
	router := server.newRouter()
	token, _, err := server.issueSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= updateAuthorizationLimit+1; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/api/system/update/jobs", bytes.NewBufferString(`{"channel":"stable","version":"v1.6.0","current_password":"wrong"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		want := http.StatusForbidden
		if attempt > updateAuthorizationLimit {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("attempt %d status = %d, want %d", attempt, response.Code, want)
		}
	}
	if coordinator.calls != 0 {
		t.Fatalf("rate-limited requests reached update coordinator %d times", coordinator.calls)
	}
}
