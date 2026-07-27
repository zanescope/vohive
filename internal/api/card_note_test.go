package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/internal/db"
	"github.com/zanescope/vohive/internal/esim"
)

func TestPatchSIMNoteAndEnrichESIMProfiles(t *testing.T) {
	initDeviceMgmtPhoneTestDB(t)
	gin.SetMode(gin.TestMode)
	server := &Server{}

	request := httptest.NewRequest(http.MethodPatch, "/api/cards/8986001/note", strings.NewReader(`{"note":"  备用卡  "}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = request
	ctx.Params = gin.Params{{Key: "iccid", Value: "8986001"}}
	server.handlePatchSIMNote(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PATCH status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Note != "备用卡" {
		t.Fatalf("note=%q want 备用卡", response.Note)
	}

	groups := []esim.EUICCProfiles{{Profiles: []esim.ProfileItem{{ICCID: "8986001"}, {ICCID: "8986002"}}}}
	if err := enrichESIMProfileNotes(groups); err != nil {
		t.Fatal(err)
	}
	if groups[0].Profiles[0].SIMNote != "备用卡" || groups[0].Profiles[1].SIMNote != "" {
		t.Fatalf("unexpected enriched profiles: %+v", groups)
	}

	var card db.SIMCard
	if err := db.DB.Where("iccid = ?", "8986001").First(&card).Error; err != nil {
		t.Fatal(err)
	}
	if card.Note != "备用卡" {
		t.Fatalf("persisted note=%q", card.Note)
	}
}
