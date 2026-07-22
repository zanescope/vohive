package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zanescope/vohive/internal/updater"
)

func TestUpdateAPIReportsReleaseUpstreamAsBadGateway(t *testing.T) {
	fake := &fakeUpdateCoordinator{
		capabilities: updater.Capabilities{CanCheck: true},
		checkErr:     updater.ErrReleaseUpstream,
	}
	response := httptest.NewRecorder()
	updateHandlerRouter(fake).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/check", nil))

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadGateway, response.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "release_upstream_unavailable" {
		t.Fatalf("code = %q", body.Code)
	}
}
