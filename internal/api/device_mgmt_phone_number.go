package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/internal/db"
	"github.com/zanescope/vohive/internal/device"
)

type setPhoneNumberRequest struct {
	ManualPhoneNumber *string `json:"manual_phone_number"`
}

func verifiedLivePhoneIdentity(ctx context.Context, worker *device.Worker, reason string) (device.LiveSIMIdentity, error) {
	identity, err := worker.RefreshIdentityLiveVerified(ctx, reason)
	if err != nil {
		return device.LiveSIMIdentity{}, err
	}
	identity.IMSI = strings.TrimSpace(identity.IMSI)
	identity.ICCID = strings.TrimSpace(identity.ICCID)
	if identity.IMSI == "" && identity.ICCID == "" {
		return device.LiveSIMIdentity{}, fmt.Errorf("live SIM identity is empty")
	}
	return identity, nil
}

func phoneNumberActionPayload(snapshot db.PhoneNumberSnapshot) gin.H {
	source := strings.TrimSpace(snapshot.PhoneNumberSource)
	if source == "" {
		source = db.PhoneNumberSourceNone
	}
	return gin.H{
		"status":             "ok",
		"local_phone":        strings.TrimSpace(snapshot.PhoneNumber),
		"local_phone_source": source,
	}
}

func (s *Server) rejectPhoneNumberActionDuringSwitch(c *gin.Context, deviceID string) bool {
	if s.pool != nil && s.pool.IsESIMSwitching(deviceID) {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "\u8bbe\u5907\u6b63\u5728\u5207\u5361\uff0c\u8bf7\u7a0d\u540e\u518d\u64cd\u4f5c\u672c\u673a\u53f7\u7801"})
		return true
	}
	return false
}

func (s *Server) handleDeviceMgmtSetPhoneNumber(c *gin.Context) {
	var request setPhoneNumberRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.ManualPhoneNumber == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "\u8bf7\u63d0\u4f9b manual_phone_number"})
		return
	}
	deviceID := deviceIDParam(c)
	worker := s.pool.GetWorker(deviceID)
	if worker == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "\u8bbe\u5907\u672a\u627e\u5230\u6216\u672a\u8fd0\u884c"})
		return
	}
	if s.rejectPhoneNumberActionDuringSwitch(c, deviceID) {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	identity, err := verifiedLivePhoneIdentity(ctx, worker, "manual_phone_number")
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "\u65e0\u6cd5\u786e\u8ba4\u5f53\u524d SIM \u8eab\u4efd\uff0c\u8bf7\u68c0\u67e5 SIM \u72b6\u6001\u540e\u91cd\u8bd5"})
		return
	}
	if s.rejectPhoneNumberActionDuringSwitch(c, deviceID) {
		return
	}

	snapshot, err := db.SetManualPhoneNumber(identity.IMSI, identity.ICCID, *request.ManualPhoneNumber)
	if errors.Is(err, db.ErrInvalidPhoneNumber) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "\u624b\u52a8\u53f7\u7801\u683c\u5f0f\u65e0\u6548"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "\u4fdd\u5b58\u672c\u673a\u53f7\u7801\u5931\u8d25"})
		return
	}
	s.pool.BroadcastOverviewStateChange(worker.ID)
	payload := phoneNumberActionPayload(snapshot)
	payload["message"] = "\u672c\u673a\u53f7\u7801\u5df2\u66f4\u65b0"
	c.JSON(http.StatusOK, payload)
}

func (s *Server) handleDeviceMgmtRefreshPhoneNumber(c *gin.Context) {
	deviceID := deviceIDParam(c)
	worker := s.pool.GetWorker(deviceID)
	if worker == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "\u8bbe\u5907\u672a\u627e\u5230\u6216\u672a\u8fd0\u884c"})
		return
	}
	if s.rejectPhoneNumberActionDuringSwitch(c, deviceID) {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	identity, err := verifiedLivePhoneIdentity(ctx, worker, "manual_phone_number_refresh")
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "\u65e0\u6cd5\u786e\u8ba4\u5f53\u524d SIM \u8eab\u4efd\uff0c\u8bf7\u68c0\u67e5 SIM \u72b6\u6001\u540e\u91cd\u8bd5"})
		return
	}
	if s.rejectPhoneNumberActionDuringSwitch(c, deviceID) {
		return
	}

	result := s.pool.PersistPhoneNumber(ctx, worker, identity.IMSI, identity.ICCID, true)
	if strings.TrimSpace(result.Number) != "" && result.Err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "\u4fdd\u5b58\u83b7\u53d6\u5230\u7684\u672c\u673a\u53f7\u7801\u5931\u8d25"})
		return
	}
	snapshot, err := db.GetPhoneNumberSnapshotByIMSIOrICCID(identity.IMSI, identity.ICCID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "\u8bfb\u53d6\u672c\u673a\u53f7\u7801\u5931\u8d25"})
		return
	}

	s.pool.BroadcastOverviewStateChange(worker.ID)
	payload := phoneNumberActionPayload(snapshot)
	acquired := strings.TrimSpace(result.Number) != ""
	payload["acquired"] = acquired
	payload["channel"] = result.Channel
	if acquired {
		payload["message"] = "\u672c\u673a\u53f7\u7801\u5df2\u5237\u65b0"
	} else {
		payload["message"] = "\u5f53\u524d SIM/\u8fd0\u8425\u5546\u672a\u8fd4\u56de\u672c\u673a\u53f7\u7801\uff0c\u53ef\u624b\u52a8\u586b\u5199"
	}
	c.JSON(http.StatusOK, payload)
}
