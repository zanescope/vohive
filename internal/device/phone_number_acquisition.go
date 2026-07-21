package device

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zanescope/vohive/internal/backend"
	"github.com/zanescope/vohive/internal/db"
	"github.com/zanescope/vohive/internal/modem"
)

type PhoneNumberAcquisitionResult struct {
	Number    string
	Channel   string
	Attempted bool
	Err       error
}

var queryMSISDNOnTransientATPort = func(port string) (string, error) {
	session, err := modem.NewSerialAT(port, 115200, 8, 1, "N")
	if err != nil {
		return "", fmt.Errorf("open AT port %s: %w", port, err)
	}
	defer session.Close()
	response, err := session.Execute("AT+CNUM", 2*time.Second)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(modem.ParseCNUMResponse(response)), nil
}

// AcquirePhoneNumber queries the active backend first. Direct AT fallback is
// reserved for explicit user refreshes on QMI/MBIM workers.
func (w *Worker) AcquirePhoneNumber(ctx context.Context, allowTransientAT bool) PhoneNumberAcquisitionResult {
	return w.acquirePhoneNumberForSIM(ctx, "", allowTransientAT)
}

func (w *Worker) acquirePhoneNumberForSIM(ctx context.Context, imsi string, allowTransientAT bool) PhoneNumberAcquisitionResult {
	if w == nil {
		return PhoneNumberAcquisitionResult{Err: errors.New("worker is nil")}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PhoneNumberAcquisitionResult{Err: err}
	}

	var result PhoneNumberAcquisitionResult
	var primaryErr error
	mode := ""
	if w.Backend != nil {
		mode = strings.TrimSpace(w.Backend.Mode())
		result.Channel = mode
		result.Attempted = true
		number, err := w.Backend.GetMSISDN(ctx)
		number = db.NormalizeSIMPhoneNumberCandidate(number, imsi)
		if err == nil && number != "" {
			result.Number = number
			return result
		}
		primaryErr = err
	} else if w.Modem != nil {
		result.Channel = backend.BackendAT
		result.Attempted = true
		number, err := w.Modem.QueryMSISDN()
		number = db.NormalizeSIMPhoneNumberCandidate(number, imsi)
		if err == nil && number != "" {
			result.Number = number
			return result
		}
		result.Err = err
		return result
	}

	if !allowTransientAT || (mode != backend.BackendQMI && mode != backend.BackendMBIM) {
		result.Err = primaryErr
		return result
	}
	if err := ctx.Err(); err != nil {
		result.Err = errors.Join(primaryErr, err)
		return result
	}

	result.Channel = "at_cnum"
	number, fallbackErr := w.WithTransientATPortContext(ctx, queryMSISDNOnTransientATPort)
	number = db.NormalizeSIMPhoneNumberCandidate(number, imsi)
	if fallbackErr == nil && number != "" {
		result.Number = number
		result.Err = nil
		return result
	}
	result.Err = errors.Join(primaryErr, fallbackErr)
	return result
}
