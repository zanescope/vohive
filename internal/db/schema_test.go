package db

import (
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openSchemaTestDatabase(t *testing.T, name string) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	dialector, err := openSQLiteDialector("modernc", path)
	if err != nil {
		t.Fatalf("openSQLiteDialector() error=%v", err)
	}
	database, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("gorm.Open() error=%v", err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatalf("database.DB() error=%v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return database
}

func TestDatabaseMigrationRecordsSchemaAndIsIdempotent(t *testing.T) {
	database := openSchemaTestDatabase(t, "schema.db")
	plan, err := MigrateDatabase(database)
	if err != nil {
		t.Fatalf("MigrateDatabase() error=%v", err)
	}
	if plan.CurrentSchema != 0 || plan.TargetSchema != 1 || len(plan.Steps) != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	current, err := InspectDatabaseSchema(database)
	if err != nil || current != CurrentDatabaseSchema {
		t.Fatalf("InspectDatabaseSchema()=(%d,%v), want (%d,nil)", current, err, CurrentDatabaseSchema)
	}
	var count int64
	if err := database.Model(&SchemaMigration{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("schema migration count=(%d,%v), want (1,nil)", count, err)
	}
	second, err := MigrateDatabase(database)
	if err != nil || second.NeedsMigration() {
		t.Fatalf("second MigrateDatabase()=(%+v,%v), want no-op", second, err)
	}
	if err := database.Model(&SchemaMigration{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("schema migration count after no-op=(%d,%v)", count, err)
	}
}

func TestDatabaseMigrationRejectsTooNewSchema(t *testing.T) {
	database := openSchemaTestDatabase(t, "too-new.db")
	if err := database.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&SchemaMigration{Version: CurrentDatabaseSchema + 1}).Error; err != nil {
		t.Fatal(err)
	}
	plan, err := MigrateDatabase(database)
	if !errors.Is(err, ErrDatabaseSchemaTooNew) {
		t.Fatalf("MigrateDatabase() error=%v want ErrDatabaseSchemaTooNew", err)
	}
	if plan.CurrentSchema != CurrentDatabaseSchema+1 || plan.NeedsMigration() {
		t.Fatalf("unexpected too-new plan: %+v", plan)
	}
	if database.Migrator().HasTable(&Device{}) {
		t.Fatal("too-new database was structurally modified")
	}
}

func TestLegacyDatabaseMigrationPreservesExistingRows(t *testing.T) {
	database := openSchemaTestDatabase(t, "legacy.db")
	if err := database.AutoMigrate(&Device{}); err != nil {
		t.Fatal(err)
	}
	want := Device{IMEI: "867530900000001", Alias: "legacy-device"}
	if err := database.Create(&want).Error; err != nil {
		t.Fatal(err)
	}
	if current, err := InspectDatabaseSchema(database); err != nil || current != 0 {
		t.Fatalf("legacy schema=(%d,%v), want (0,nil)", current, err)
	}
	if _, err := MigrateDatabase(database); err != nil {
		t.Fatalf("MigrateDatabase() error=%v", err)
	}
	var got Device
	if err := database.Where("imei = ?", want.IMEI).First(&got).Error; err != nil {
		t.Fatalf("legacy row missing after migration: %v", err)
	}
	if got.Alias != want.Alias {
		t.Fatalf("Alias=%q want %q", got.Alias, want.Alias)
	}
}
