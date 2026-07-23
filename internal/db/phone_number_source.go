package db

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

var (
	ErrPhoneIdentityRequired = errors.New("SIM identity is required")
	ErrInvalidPhoneNumber    = errors.New("invalid phone number")
)

// NormalizeSIMPhoneNumberCandidate applies the canonical database validation
// without writing. Callers can use it to avoid reporting rejected modem
// placeholders (including an IMSI echo) as an acquired phone number.
func NormalizeSIMPhoneNumberCandidate(phone, imsi string) string {
	normalized := normalizeSIMPhoneNumber(phone)
	if normalized == "" || (strings.TrimSpace(imsi) != "" && phoneDigitsEqualIMSI(normalized, imsi)) {
		return ""
	}
	return normalized
}

func resolvedPhoneNumberSource(phone, source, modem, vowifi string) string {
	phone = normalizeSIMPhoneNumber(phone)
	modem = normalizeSIMPhoneNumber(modem)
	vowifi = normalizeSIMPhoneNumber(vowifi)
	switch strings.TrimSpace(source) {
	case PhoneNumberSourceManual:
		if phone != "" {
			return PhoneNumberSourceManual
		}
	case PhoneNumberSourceLegacy:
		if phone != "" {
			return PhoneNumberSourceLegacy
		}
	}
	if phone != "" && vowifi != "" && phone == vowifi {
		return PhoneNumberSourceVoWiFi
	}
	if phone != "" && modem != "" && phone == modem {
		return PhoneNumberSourceModem
	}
	if phone != "" {
		return PhoneNumberSourceLegacy
	}
	return PhoneNumberSourceNone
}

func automaticPhoneNumber(modem, vowifi string) (string, string) {
	if phone := normalizeSIMPhoneNumber(vowifi); phone != "" {
		return phone, PhoneNumberSourceVoWiFi
	}
	if phone := normalizeSIMPhoneNumber(modem); phone != "" {
		return phone, PhoneNumberSourceModem
	}
	return "", PhoneNumberSourceNone
}

func snapshotFromSubscription(sub SIMSubscription) PhoneNumberSnapshot {
	return PhoneNumberSnapshot{
		PhoneNumber:       normalizeSIMPhoneNumber(sub.PhoneNumber),
		PhoneNumberSource: resolvedPhoneNumberSource(sub.PhoneNumber, sub.PhoneNumberSource, sub.ModemPhoneNumber, sub.VowifiPhoneNumber),
		ModemPhoneNumber:  normalizeSIMPhoneNumber(sub.ModemPhoneNumber),
		VowifiPhoneNumber: normalizeSIMPhoneNumber(sub.VowifiPhoneNumber),
	}
}

func snapshotFromPending(pending PendingPhoneNumber) PhoneNumberSnapshot {
	return PhoneNumberSnapshot{
		PhoneNumber:       normalizeSIMPhoneNumber(pending.PhoneNumber),
		PhoneNumberSource: resolvedPhoneNumberSource(pending.PhoneNumber, pending.PhoneNumberSource, pending.ModemPhoneNumber, pending.VowifiPhoneNumber),
		ModemPhoneNumber:  normalizeSIMPhoneNumber(pending.ModemPhoneNumber),
		VowifiPhoneNumber: normalizeSIMPhoneNumber(pending.VowifiPhoneNumber),
	}
}

func updateAutomaticSubscriptionPhone(imsi, iccid, phone, source string) error {
	imsi = strings.TrimSpace(imsi)
	iccid = strings.TrimSpace(iccid)
	if imsi == "" || DB == nil {
		return nil
	}
	normalized := normalizeSIMPhoneNumber(phone)
	if normalized == "" || phoneDigitsEqualIMSI(normalized, imsi) {
		return nil
	}
	if source != PhoneNumberSourceModem && source != PhoneNumberSourceVoWiFi {
		return fmt.Errorf("unsupported phone number source %q", source)
	}

	return DB.Transaction(func(tx *gorm.DB) error {
		if iccid != "" {
			if err := migratePendingPhoneToSubscriptionTx(tx, imsi, iccid); err != nil {
				return err
			}
		}
		now := time.Now()
		var sub SIMSubscription
		err := tx.Where("imsi = ?", imsi).Limit(1).First(&sub).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			sub = SIMSubscription{
				IMSI:              imsi,
				PhoneNumberSource: PhoneNumberSourceNone,
				CreatedAt:         now,
			}
		}
		if iccid != "" {
			sub.CurrentICCID = iccid
		}
		if source == PhoneNumberSourceVoWiFi {
			sub.VowifiPhoneNumber = normalized
		} else {
			sub.ModemPhoneNumber = normalized
		}

		currentSource := resolvedPhoneNumberSource(sub.PhoneNumber, sub.PhoneNumberSource, sub.ModemPhoneNumber, sub.VowifiPhoneNumber)
		if currentSource != PhoneNumberSourceManual {
			sub.PhoneNumber, sub.PhoneNumberSource = automaticPhoneNumber(sub.ModemPhoneNumber, sub.VowifiPhoneNumber)
		} else {
			sub.PhoneNumber = normalizeSIMPhoneNumber(sub.PhoneNumber)
			sub.PhoneNumberSource = PhoneNumberSourceManual
		}
		sub.LastSeen = now
		sub.UpdatedAt = now
		return tx.Save(&sub).Error
	})
}

func updateAutomaticPendingPhone(iccid, phone, source string) error {
	iccid = strings.TrimSpace(iccid)
	if iccid == "" || DB == nil {
		return nil
	}
	normalized := normalizeSIMPhoneNumber(phone)
	if normalized == "" {
		return nil
	}
	if source != PhoneNumberSourceModem && source != PhoneNumberSourceVoWiFi {
		return fmt.Errorf("unsupported phone number source %q", source)
	}

	return DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		var pending PendingPhoneNumber
		err := tx.Where("iccid = ?", iccid).Limit(1).First(&pending).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			pending = PendingPhoneNumber{
				ICCID:             iccid,
				PhoneNumberSource: PhoneNumberSourceNone,
				CreatedAt:         now,
			}
		}
		if source == PhoneNumberSourceVoWiFi {
			pending.VowifiPhoneNumber = normalized
		} else {
			pending.ModemPhoneNumber = normalized
		}

		currentSource := resolvedPhoneNumberSource(pending.PhoneNumber, pending.PhoneNumberSource, pending.ModemPhoneNumber, pending.VowifiPhoneNumber)
		if currentSource != PhoneNumberSourceManual {
			pending.PhoneNumber, pending.PhoneNumberSource = automaticPhoneNumber(pending.ModemPhoneNumber, pending.VowifiPhoneNumber)
		} else {
			pending.PhoneNumber = normalizeSIMPhoneNumber(pending.PhoneNumber)
			pending.PhoneNumberSource = PhoneNumberSourceManual
		}
		pending.UpdatedAt = now
		return tx.Save(&pending).Error
	})
}

// SetManualPhoneNumber sets a SIM-scoped override. An empty value clears only a
// manual override and immediately falls back to the best automatic candidate.
func SetManualPhoneNumber(imsi, iccid, phone string) (PhoneNumberSnapshot, error) {
	imsi = strings.TrimSpace(imsi)
	iccid = strings.TrimSpace(iccid)
	if imsi == "" && iccid == "" {
		return PhoneNumberSnapshot{PhoneNumberSource: PhoneNumberSourceNone}, ErrPhoneIdentityRequired
	}
	if DB == nil {
		return PhoneNumberSnapshot{PhoneNumberSource: PhoneNumberSourceNone}, errors.New("database is not initialized")
	}

	rawPhone := strings.TrimSpace(phone)
	normalized := ""
	if rawPhone != "" {
		normalized = normalizeSIMPhoneNumber(rawPhone)
		if normalized == "" || (imsi != "" && phoneDigitsEqualIMSI(normalized, imsi)) {
			return PhoneNumberSnapshot{PhoneNumberSource: PhoneNumberSourceNone}, ErrInvalidPhoneNumber
		}
	}

	var snapshot PhoneNumberSnapshot
	err := DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		if imsi != "" {
			if iccid != "" {
				if err := migratePendingPhoneToSubscriptionTx(tx, imsi, iccid); err != nil {
					return err
				}
			}
			var sub SIMSubscription
			err := tx.Where("imsi = ?", imsi).Limit(1).First(&sub).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if errors.Is(err, gorm.ErrRecordNotFound) {
				sub = SIMSubscription{
					IMSI:              imsi,
					PhoneNumberSource: PhoneNumberSourceNone,
					CreatedAt:         now,
				}
			}
			if iccid != "" {
				sub.CurrentICCID = iccid
			}
			if normalized != "" {
				sub.PhoneNumber = normalized
				sub.PhoneNumberSource = PhoneNumberSourceManual
			} else if resolvedPhoneNumberSource(sub.PhoneNumber, sub.PhoneNumberSource, sub.ModemPhoneNumber, sub.VowifiPhoneNumber) == PhoneNumberSourceManual {
				sub.PhoneNumber, sub.PhoneNumberSource = automaticPhoneNumber(sub.ModemPhoneNumber, sub.VowifiPhoneNumber)
			}
			sub.LastSeen = now
			sub.UpdatedAt = now
			if err := tx.Save(&sub).Error; err != nil {
				return err
			}
			snapshot = snapshotFromSubscription(sub)
			return nil
		}

		var pending PendingPhoneNumber
		err := tx.Where("iccid = ?", iccid).Limit(1).First(&pending).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			pending = PendingPhoneNumber{
				ICCID:             iccid,
				PhoneNumberSource: PhoneNumberSourceNone,
				CreatedAt:         now,
			}
		}
		if normalized != "" {
			pending.PhoneNumber = normalized
			pending.PhoneNumberSource = PhoneNumberSourceManual
		} else if resolvedPhoneNumberSource(pending.PhoneNumber, pending.PhoneNumberSource, pending.ModemPhoneNumber, pending.VowifiPhoneNumber) == PhoneNumberSourceManual {
			pending.PhoneNumber, pending.PhoneNumberSource = automaticPhoneNumber(pending.ModemPhoneNumber, pending.VowifiPhoneNumber)
		}
		pending.UpdatedAt = now
		if err := tx.Save(&pending).Error; err != nil {
			return err
		}
		snapshot = snapshotFromPending(pending)
		return nil
	})
	return snapshot, err
}

func migratePendingPhoneToSubscriptionAtomic(imsi, iccid string) error {
	imsi = strings.TrimSpace(imsi)
	iccid = strings.TrimSpace(iccid)
	if imsi == "" || iccid == "" || DB == nil {
		return nil
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		return migratePendingPhoneToSubscriptionTx(tx, imsi, iccid)
	})
}

func migratePendingPhoneToSubscriptionTx(tx *gorm.DB, imsi, iccid string) error {
	var pending PendingPhoneNumber
	err := tx.Where("iccid = ?", iccid).Limit(1).First(&pending).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	now := time.Now()
	var sub SIMSubscription
	err = tx.Where("imsi = ?", imsi).Limit(1).First(&sub).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		sub = SIMSubscription{
			IMSI:              imsi,
			PhoneNumberSource: PhoneNumberSourceNone,
			CreatedAt:         now,
		}
	}
	sub.CurrentICCID = iccid
	if modem := normalizeSIMPhoneNumber(pending.ModemPhoneNumber); modem != "" && !phoneDigitsEqualIMSI(modem, imsi) {
		sub.ModemPhoneNumber = modem
	}
	if vowifi := normalizeSIMPhoneNumber(pending.VowifiPhoneNumber); vowifi != "" && !phoneDigitsEqualIMSI(vowifi, imsi) {
		sub.VowifiPhoneNumber = vowifi
	}

	subSource := resolvedPhoneNumberSource(sub.PhoneNumber, sub.PhoneNumberSource, sub.ModemPhoneNumber, sub.VowifiPhoneNumber)
	pendingSource := resolvedPhoneNumberSource(pending.PhoneNumber, pending.PhoneNumberSource, pending.ModemPhoneNumber, pending.VowifiPhoneNumber)
	pendingEffective := normalizeSIMPhoneNumber(pending.PhoneNumber)
	subEffective := normalizeSIMPhoneNumber(sub.PhoneNumber)
	switch {
	case pendingSource == PhoneNumberSourceManual && pendingEffective != "" && !phoneDigitsEqualIMSI(pendingEffective, imsi) &&
		(subSource != PhoneNumberSourceManual || pending.UpdatedAt.After(sub.UpdatedAt)):
		sub.PhoneNumber = pendingEffective
		sub.PhoneNumberSource = PhoneNumberSourceManual
	case subSource == PhoneNumberSourceManual && subEffective != "":
		sub.PhoneNumber = subEffective
		sub.PhoneNumberSource = PhoneNumberSourceManual
	default:
		sub.PhoneNumber, sub.PhoneNumberSource = automaticPhoneNumber(sub.ModemPhoneNumber, sub.VowifiPhoneNumber)
		if sub.PhoneNumber == "" {
			switch {
			case pendingSource == PhoneNumberSourceLegacy && pendingEffective != "" && !phoneDigitsEqualIMSI(pendingEffective, imsi):
				sub.PhoneNumber = pendingEffective
				sub.PhoneNumberSource = PhoneNumberSourceLegacy
			case subSource == PhoneNumberSourceLegacy && subEffective != "":
				sub.PhoneNumber = subEffective
				sub.PhoneNumberSource = PhoneNumberSourceLegacy
			}
		}
	}
	sub.LastSeen = now
	sub.UpdatedAt = now
	if err := tx.Save(&sub).Error; err != nil {
		return err
	}
	return tx.Where("iccid = ?", iccid).Delete(&PendingPhoneNumber{}).Error
}

// GetPhoneNumberSnapshotByIMSIOrICCID resolves ICCID first so a stale IMSI
// cannot leak the previous SIM's number during a card transition.
func GetPhoneNumberSnapshotByIMSIOrICCID(imsi, iccid string) (PhoneNumberSnapshot, error) {
	empty := PhoneNumberSnapshot{PhoneNumberSource: PhoneNumberSourceNone}
	imsi = strings.TrimSpace(imsi)
	iccid = strings.TrimSpace(iccid)
	if DB == nil {
		return empty, nil
	}

	if iccid != "" {
		var canonicalSnapshot PhoneNumberSnapshot
		canonicalFound := false

		if imsi != "" {
			var sub SIMSubscription
			subErr := DB.Where("imsi = ? AND current_iccid = ?", imsi, iccid).Limit(1).First(&sub).Error
			if subErr != nil && !errors.Is(subErr, gorm.ErrRecordNotFound) {
				return empty, subErr
			}
			if subErr == nil {
				canonicalSnapshot = snapshotFromSubscription(sub)
				canonicalFound = true
				if canonicalSnapshot.PhoneNumber != "" {
					return canonicalSnapshot, nil
				}
			}
		}

		if !canonicalFound {
			var sim SIMCard
			err := DB.Select("imsi").Where("iccid = ?", iccid).Limit(1).First(&sim).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return empty, err
			}
			if err == nil && strings.TrimSpace(sim.IMSI) != "" {
				var sub SIMSubscription
				subErr := DB.Where("imsi = ?", strings.TrimSpace(sim.IMSI)).Limit(1).First(&sub).Error
				if subErr != nil && !errors.Is(subErr, gorm.ErrRecordNotFound) {
					return empty, subErr
				}
				if subErr == nil {
					canonicalSnapshot = snapshotFromSubscription(sub)
					canonicalFound = true
					if canonicalSnapshot.PhoneNumber != "" {
						return canonicalSnapshot, nil
					}
				}
			}
		}

		if !canonicalFound {
			var sub SIMSubscription
			err := DB.Where("current_iccid = ?", iccid).Order("updated_at DESC").Limit(1).First(&sub).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return empty, err
			}
			if err == nil {
				canonicalSnapshot = snapshotFromSubscription(sub)
				canonicalFound = true
				if canonicalSnapshot.PhoneNumber != "" {
					return canonicalSnapshot, nil
				}
			}
		}

		var pending PendingPhoneNumber
		err := DB.Where("iccid = ?", iccid).Limit(1).First(&pending).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return empty, err
		}
		if err == nil {
			pendingSnapshot := snapshotFromPending(pending)
			if pendingSnapshot.PhoneNumber != "" || !canonicalFound {
				return pendingSnapshot, nil
			}
		}
		if canonicalFound {
			return canonicalSnapshot, nil
		}
	}

	if imsi != "" {
		var sub SIMSubscription
		err := DB.Where("imsi = ?", imsi).Limit(1).First(&sub).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return empty, nil
		}
		if err != nil {
			return empty, err
		}
		return snapshotFromSubscription(sub), nil
	}
	return empty, nil
}
