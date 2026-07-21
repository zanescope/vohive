package device

import (
	"context"
	"fmt"
	"strings"

	"github.com/zanescope/vohive/internal/db"
	"github.com/zanescope/vohive/pkg/logger"
)

// PersistIdentityOnly stores identity metadata without coupling it to phone
// acquisition. Its normal Device/SIM relationship still requires an IMEI.
func (p *Pool) PersistIdentityOnly(worker *Worker) {
	if worker == nil {
		return
	}
	status := worker.ProjectDeviceStatus()
	iccid := strings.TrimSpace(status.ICCID)
	imsi := strings.TrimSpace(status.IMSI)
	imei := strings.TrimSpace(status.IMEI)
	if iccid == "" {
		return
	}
	if imei == "" {
		logger.Warn(fmt.Sprintf("[%s] \u65e0\u6cd5\u540c\u6b65\u8bbe\u5907 SIM \u8eab\u4efd\uff1aIMEI \u4e3a\u7a7a", worker.ID))
		return
	}
	operator := strings.TrimSpace(status.Operator)
	if err := db.UpsertSIMCard(iccid, imsi, "", operator, &imei); err != nil {
		logger.Warn(fmt.Sprintf("[%s] \u66f4\u65b0 SIM \u5361\u4fe1\u606f\u5931\u8d25", worker.ID), "err", err)
	}
	if err := db.UpdateDeviceCurrentSIM(imei, &iccid); err != nil {
		logger.Warn(fmt.Sprintf("[%s] \u66f4\u65b0\u8bbe\u5907 SIM \u5173\u8054\u5931\u8d25", worker.ID), "err", err)
	}
}

func (p *Pool) PersistPhoneNumber(ctx context.Context, worker *Worker, imsi, iccid string, allowTransientAT bool) PhoneNumberAcquisitionResult {
	result := worker.acquirePhoneNumberForSIM(ctx, imsi, allowTransientAT)
	if strings.TrimSpace(result.Number) == "" {
		return result
	}
	if err := db.RecordModemPhoneNumber(imsi, iccid, result.Number); err != nil {
		result.Err = err
	}
	return result
}
