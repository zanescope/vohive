package db

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const MaxSIMNoteRunes = 100

var (
	ErrInvalidSIMNote = errors.New("invalid SIM note")
	ErrInvalidICCID   = errors.New("invalid ICCID")
)

func NormalizeSIMNote(note string) (string, error) {
	note = strings.TrimSpace(note)
	if strings.ContainsAny(note, "\r\n") {
		return "", fmt.Errorf("%w: line breaks are not allowed", ErrInvalidSIMNote)
	}
	if utf8.RuneCountInString(note) > MaxSIMNoteRunes {
		return "", fmt.Errorf("%w: maximum length is %d characters", ErrInvalidSIMNote, MaxSIMNoteRunes)
	}
	return note, nil
}

func SetSIMCardNote(iccid, note string) (string, error) {
	if DB == nil {
		return "", fmt.Errorf("set SIM note: database is not initialized")
	}
	iccid = strings.TrimSpace(iccid)
	if iccid == "" {
		return "", ErrInvalidICCID
	}
	normalized, err := NormalizeSIMNote(note)
	if err != nil {
		return "", err
	}
	card := SIMCard{ICCID: iccid, Note: normalized}
	if err := DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "iccid"}},
		DoUpdates: clause.AssignmentColumns([]string{"note", "updated_at"}),
	}).Create(&card).Error; err != nil {
		return "", fmt.Errorf("set SIM note: %w", err)
	}
	return normalized, nil
}

func GetSIMCardNote(iccid string) (string, error) {
	if DB == nil {
		return "", fmt.Errorf("get SIM note: database is not initialized")
	}
	iccid = strings.TrimSpace(iccid)
	if iccid == "" {
		return "", nil
	}
	var card SIMCard
	err := DB.Select("note").Where("iccid = ?", iccid).First(&card).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get SIM note: %w", err)
	}
	return strings.TrimSpace(card.Note), nil
}

func GetSIMCardNotes(iccids []string) (map[string]string, error) {
	result := make(map[string]string)
	if DB == nil {
		return result, fmt.Errorf("get SIM notes: database is not initialized")
	}
	unique := make([]string, 0, len(iccids))
	seen := make(map[string]struct{}, len(iccids))
	for _, raw := range iccids {
		iccid := strings.TrimSpace(raw)
		if iccid == "" {
			continue
		}
		if _, ok := seen[iccid]; ok {
			continue
		}
		seen[iccid] = struct{}{}
		unique = append(unique, iccid)
	}
	if len(unique) == 0 {
		return result, nil
	}
	var cards []SIMCard
	if err := DB.Select("iccid", "note").Where("iccid IN ?", unique).Find(&cards).Error; err != nil {
		return result, fmt.Errorf("get SIM notes: %w", err)
	}
	for _, card := range cards {
		result[card.ICCID] = strings.TrimSpace(card.Note)
	}
	return result, nil
}
