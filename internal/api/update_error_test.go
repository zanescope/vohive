package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zanescope/vohive/internal/updater"
)

func TestUpdateAPIReportsManualRecoveryConflict(t *testing.T) {
	fake := &fakeUpdateCoordinator{
		capabilities: updater.Capabilities{CanCheck: true, CanUpdate: true},
		checkErr:     updater.ErrManualRecoveryRequired,
	}
	response := httptest.NewRecorder()
	updateHandlerRouter(fake).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/check", nil))

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "manual_recovery_required" {
		t.Fatalf("code = %q, want manual_recovery_required", body.Code)
	}
}
