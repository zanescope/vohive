package db

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

const (
	LegacyDatabaseSchema           = 0
	MinimumSupportedDatabaseSchema = LegacyDatabaseSchema
	CurrentDatabaseSchema          = 1
)

var (
	ErrDatabaseSchemaTooNew      = errors.New("database schema is newer than this binary")
	ErrDatabaseSchemaUnsupported = errors.New("database schema is unsupported")
)

type SchemaMigration struct {
	Version   int       `gorm:"primaryKey;column:version" json:"version"`
	AppliedAt time.Time `gorm:"column:applied_at;not null" json:"applied_at"`
}

func (SchemaMigration) TableName() string { return "schema_migrations" }

type DatabaseMigrationStep struct {
	From        int    `json:"from"`
	To          int    `json:"to"`
	Description string `json:"description"`
}

type DatabaseMigrationPlan struct {
	CurrentSchema int                     `json:"current_schema"`
	TargetSchema  int                     `json:"target_schema"`
	Steps         []DatabaseMigrationStep `json:"steps"`
}

func (p DatabaseMigrationPlan) NeedsMigration() bool { return len(p.Steps) > 0 }

type DatabaseSchemaCompatibility struct {
	MinimumSupported int `json:"minimum_supported"`
	Target           int `json:"target"`
}

func DatabaseCompatibility() DatabaseSchemaCompatibility {
	return DatabaseSchemaCompatibility{
		MinimumSupported: MinimumSupportedDatabaseSchema,
		Target:           CurrentDatabaseSchema,
	}
}

// InspectDatabaseSchema treats databases without schema_migrations as legacy
// schema 0. It never changes the database.
func InspectDatabaseSchema(database *gorm.DB) (int, error) {
	if database == nil {
		return 0, fmt.Errorf("inspect database schema: nil database")
	}
	if !database.Migrator().HasTable(&SchemaMigration{}) {
		return LegacyDatabaseSchema, nil
	}
	var version int
	if err := database.Model(&SchemaMigration{}).Select("COALESCE(MAX(version), 0)").Scan(&version).Error; err != nil {
		return 0, fmt.Errorf("inspect database schema: %w", err)
	}
	if version < 0 {
		return 0, fmt.Errorf("%w: schema=%d", ErrDatabaseSchemaUnsupported, version)
	}
	return version, nil
}

func PlanDatabaseMigration(database *gorm.DB) (DatabaseMigrationPlan, error) {
	current, err := InspectDatabaseSchema(database)
	if err != nil {
		return DatabaseMigrationPlan{}, err
	}
	plan := DatabaseMigrationPlan{
		CurrentSchema: current,
		TargetSchema:  CurrentDatabaseSchema,
	}
	if current > CurrentDatabaseSchema {
		return plan, fmt.Errorf("%w: database=%d binary=%d", ErrDatabaseSchemaTooNew, current, CurrentDatabaseSchema)
	}
	if current < MinimumSupportedDatabaseSchema {
		return plan, fmt.Errorf("%w: database=%d minimum=%d", ErrDatabaseSchemaUnsupported, current, MinimumSupportedDatabaseSchema)
	}
	for version := current; version < CurrentDatabaseSchema; version++ {
		switch version {
		case 0:
			plan.Steps = append(plan.Steps, DatabaseMigrationStep{
				From:        0,
				To:          1,
				Description: "create the explicit VoHive schema and migrate legacy tables",
			})
		default:
			return plan, fmt.Errorf("%w: no migration from schema %d", ErrDatabaseSchemaUnsupported, version)
		}
	}
	return plan, nil
}

// MigrateDatabase runs every required step in order. AutoMigrate is contained
// inside schema 0 -> 1 instead of running unversioned on every startup.
func MigrateDatabase(database *gorm.DB) (DatabaseMigrationPlan, error) {
	plan, err := PlanDatabaseMigration(database)
	if err != nil || !plan.NeedsMigration() {
		return plan, err
	}
	err = database.Transaction(func(tx *gorm.DB) error {
		for _, step := range plan.Steps {
			switch step.From {
			case 0:
				if err := migrateDatabaseSchema0To1(tx); err != nil {
					return fmt.Errorf("migrate database schema 0 to 1: %w", err)
				}
			default:
				return fmt.Errorf("%w: no migration from schema %d", ErrDatabaseSchemaUnsupported, step.From)
			}
			if err := tx.Create(&SchemaMigration{Version: step.To, AppliedAt: time.Now().UTC()}).Error; err != nil {
				return fmt.Errorf("record database schema %d: %w", step.To, err)
			}
		}
		return nil
	})
	return plan, err
}

// ReconcileCurrentDatabase reruns only the idempotent data cleanup steps that
// may have been interrupted in legacy builds. Structural AutoMigrate remains
// exclusively versioned above.
func ReconcileCurrentDatabase(database *gorm.DB) error {
	if database == nil {
		return fmt.Errorf("reconcile database: nil database")
	}
	return database.Transaction(func(tx *gorm.DB) error {
		if err := migrateSIMCardsToSubscriptions(tx); err != nil {
			return err
		}
		if err := backfillPhoneNumberSources(tx); err != nil {
			return err
		}
		if err := migrateSIMCardIdentityColumnsOnly(tx); err != nil {
			return err
		}
		if err := RunICCIDReKeyMigration(tx); err != nil {
			return err
		}
		return nil
	})
}

func migrateDatabaseSchema0To1(tx *gorm.DB) error {
	if err := tx.AutoMigrate(
		&SchemaMigration{},
		&Device{},
		&CardPolicy{},
		&SIMCard{},
		&SIMSubscription{},
		&PendingPhoneNumber{},
		&ProxyInstance{},
		&UpstreamProxy{},
		&UpstreamProxyCountryRule{},
		&SMS{},
		&SMSContact{},
		&SMSDelivery{},
		&SMSDeliveryPart{},
		&TrafficMinute{},
		&TrafficHour{},
		&TrafficDay{},
		&TrafficWeek{},
		&TrafficMonth{},
	); err != nil {
		return err
	}
	if err := migrateSIMCardsToSubscriptions(tx); err != nil {
		return err
	}
	if err := backfillPhoneNumberSources(tx); err != nil {
		return err
	}
	if err := migrateSIMCardIdentityColumnsOnly(tx); err != nil {
		return err
	}
	return RunICCIDReKeyMigration(tx)
}
