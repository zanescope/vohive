package db

import (
	"errors"
	"strings"
	"testing"
)

func TestSIMCardNoteRoundTripPreservesIdentity(t *testing.T) {
	database := openSchemaTestDatabase(t, "sim-note.db")
	if err := database.AutoMigrate(&SIMCard{}); err != nil {
		t.Fatal(err)
	}
	previousDB := DB
	DB = database
	t.Cleanup(func() { DB = previousDB })

	card := SIMCard{ICCID: "8986000000000000001", IMSI: "460001234567890", Operator: "China Mobile"}
	if err := database.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	got, err := SetSIMCardNote(card.ICCID, "  主卡  ")
	if err != nil || got != "主卡" {
		t.Fatalf("SetSIMCardNote()=(%q,%v), want (主卡,nil)", got, err)
	}
	got, err = GetSIMCardNote(card.ICCID)
	if err != nil || got != "主卡" {
		t.Fatalf("GetSIMCardNote()=(%q,%v), want (主卡,nil)", got, err)
	}

	var persisted SIMCard
	if err := database.Where("iccid = ?", card.ICCID).First(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.IMSI != card.IMSI || persisted.Operator != card.Operator {
		t.Fatalf("identity fields changed: %+v", persisted)
	}

	notes, err := GetSIMCardNotes([]string{card.ICCID, card.ICCID, ""})
	if err != nil || notes[card.ICCID] != "主卡" {
		t.Fatalf("GetSIMCardNotes()=(%v,%v)", notes, err)
	}
	if cleared, err := SetSIMCardNote(card.ICCID, "  "); err != nil || cleared != "" {
		t.Fatalf("clear note=(%q,%v)", cleared, err)
	}
}

func TestNormalizeSIMNoteValidation(t *testing.T) {
	if _, err := NormalizeSIMNote("line one\nline two"); !errors.Is(err, ErrInvalidSIMNote) {
		t.Fatalf("newline error=%v, want ErrInvalidSIMNote", err)
	}
	if _, err := NormalizeSIMNote(strings.Repeat("备", MaxSIMNoteRunes+1)); !errors.Is(err, ErrInvalidSIMNote) {
		t.Fatalf("length error=%v, want ErrInvalidSIMNote", err)
	}
}
