package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Migration upgrades one schema version to the next. Apply receives the raw
// JSON object so it can rename keys and change value types; it must be
// deterministic, must not read the environment and must not discard user
// values it does not understand.
type Migration struct {
	From  int
	To    int
	Apply func(map[string]any) error
}

// DefaultMigrations is the ordered chain shipped by this binary. It is empty
// while 1.0 is the first schema; future releases append entries here.
func DefaultMigrations() []Migration {
	return nil
}

// Migrate applies migrations in order until raw["version"] reaches
// CurrentVersion. A nil migrations slice selects DefaultMigrations. Missing
// steps and malformed chains are errors, never silently skipped.
func Migrate(raw map[string]any, migrations []Migration) error {
	if raw == nil {
		return errors.New("configuration is empty")
	}
	version, err := rawVersion(raw)
	if err != nil {
		return err
	}
	if version > CurrentVersion {
		return fmt.Errorf("configuration version %d is newer than this binary supports (%d); upgrade rigscope first", version, CurrentVersion)
	}
	if version == CurrentVersion {
		return nil
	}
	if migrations == nil {
		migrations = DefaultMigrations()
	}
	index := make(map[int]Migration, len(migrations))
	for _, migration := range migrations {
		if migration.Apply == nil {
			return fmt.Errorf("migration %d->%d has no apply function", migration.From, migration.To)
		}
		if migration.To != migration.From+1 {
			return fmt.Errorf("migration %d->%d does not advance exactly one version", migration.From, migration.To)
		}
		if _, exists := index[migration.From]; exists {
			return fmt.Errorf("duplicate migration from version %d", migration.From)
		}
		index[migration.From] = migration
	}
	for version < CurrentVersion {
		migration, ok := index[version]
		if !ok {
			return fmt.Errorf("no migration from version %d to %d", version, version+1)
		}
		if err := migration.Apply(raw); err != nil {
			return fmt.Errorf("migration %d->%d: %w", migration.From, migration.To, err)
		}
		version = migration.To
		raw["version"] = version
	}
	return nil
}

func rawVersion(raw map[string]any) (int, error) {
	value, ok := raw["version"]
	if !ok {
		return 0, errors.New(`missing "version" field`)
	}
	var version int
	switch typed := value.(type) {
	case int:
		version = typed
	case int64:
		version = int(typed)
	case float64:
		if typed != math.Trunc(typed) {
			return 0, fmt.Errorf("invalid configuration version %v", typed)
		}
		version = int(typed)
	case json.Number:
		parsed, err := strconv.Atoi(typed.String())
		if err != nil {
			return 0, fmt.Errorf("invalid configuration version %q", typed)
		}
		version = parsed
	default:
		return 0, fmt.Errorf("invalid configuration version %v", value)
	}
	if version < 0 {
		return 0, fmt.Errorf("invalid configuration version %d", version)
	}
	return version, nil
}
