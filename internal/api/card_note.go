package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/internal/db"
	"github.com/zanescope/vohive/internal/esim"
)

type patchSIMNoteRequest struct {
	Note *string `json:"note"`
}

func (s *Server) handlePatchSIMNote(c *gin.Context) {
	iccid := strings.TrimSpace(c.Param("iccid"))
	var request patchSIMNoteRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.Note == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "请提供 note"})
		return
	}

	note, err := db.SetSIMCardNote(iccid, *request.Note)
	if errors.Is(err, db.ErrInvalidICCID) || errors.Is(err, db.ErrInvalidSIMNote) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "保存 SIM 备注失败"})
		return
	}

	if s.pool != nil {
		for _, worker := range s.pool.GetAllWorkers() {
			if strings.TrimSpace(worker.GetCachedDeviceStatus().ICCID) == iccid {
				s.pool.BroadcastOverviewStateChange(worker.ID)
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"iccid":   iccid,
		"note":    note,
		"message": "SIM 备注已更新",
	})
}

func enrichESIMProfileNotes(groups []esim.EUICCProfiles) error {
	iccids := make([]string, 0)
	for _, group := range groups {
		for _, profile := range group.Profiles {
			if iccid := strings.TrimSpace(profile.ICCID); iccid != "" {
				iccids = append(iccids, iccid)
			}
		}
	}
	if len(iccids) == 0 {
		return nil
	}
	notes, err := db.GetSIMCardNotes(iccids)
	if err != nil {
		return err
	}
	for groupIndex := range groups {
		for profileIndex := range groups[groupIndex].Profiles {
			profile := &groups[groupIndex].Profiles[profileIndex]
			profile.SIMNote = notes[strings.TrimSpace(profile.ICCID)]
		}
	}
	return nil
}
