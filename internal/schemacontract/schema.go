// Package schemacontract defines the schema capabilities shared by the
// runtime, signed release manifests, and the updater.
package schemacontract

// Range describes the on-disk schemas a binary can consume and the schema it
// writes after migration.
type Range struct {
	Min    int
	Target int
	Max    int
}

const (
	ConfigMin    = 0
	ConfigTarget = 1
	ConfigMax    = ConfigTarget

	DatabaseMin    = 0
	DatabaseTarget = 2
	DatabaseMax    = DatabaseTarget
)

// Config returns the config-file compatibility compiled into this binary.
func Config() Range {
	return Range{Min: ConfigMin, Target: ConfigTarget, Max: ConfigMax}
}

// Database returns the database compatibility compiled into this binary.
func Database() Range {
	return Range{Min: DatabaseMin, Target: DatabaseTarget, Max: DatabaseMax}
}
