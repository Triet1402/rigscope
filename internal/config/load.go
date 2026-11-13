package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Triet1402/rigscope/internal/platform/fsutil"
)

// Load reads and migrates the configuration at path.
func Load(path string) (*Config, error) {
	return LoadWithMigrations(path, nil)
}

// LoadWithMigrations reads a configuration, applies the forward migration
// chain to a generic map when the file predates CurrentVersion, then strictly
// decodes the result into the current schema. A nil slice uses
// DefaultMigrations. Tests pass a custom chain to exercise the framework.
func LoadWithMigrations(path string, migrations []Migration) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read configuration %s: %w", path, err)
	}
	version, err := versionOf(data)
	if err != nil {
		return nil, fmt.Errorf("parse configuration %s: %w", path, err)
	}
	if version > CurrentVersion {
		return nil, fmt.Errorf("parse configuration %s: configuration version %d is newer than this binary supports (%d); upgrade rigscope first", path, version, CurrentVersion)
	}
	var cfg *Config
	if version == CurrentVersion {
		cfg, err = Decode(data)
	} else {
		cfg, err = migrateAndDecode(data, migrations)
	}
	if err != nil {
		return nil, fmt.Errorf("parse configuration %s: %w", path, err)
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration %s: %w", path, err)
	}
	return cfg, nil
}

// migrateAndDecode applies migrations to the raw JSON object. Migrations see a
// map so they can rename keys and change value types; only the final result is
// decoded into the strict current schema.
func migrateAndDecode(data []byte, migrations []Migration) (*Config, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var raw map[string]any
	if err := decoder.Decode(&raw); err != nil {
		return nil, err
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, err
	}
	if err := Migrate(raw, migrations); err != nil {
		return nil, err
	}
	migrated, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode migrated configuration: %w", err)
	}
	return Decode(migrated)
}

// Decode parses a configuration that is already at CurrentVersion, strictly.
// Unknown fields are an error so typos do not silently take effect, and the
// new policy and hardware sections must be present rather than defaulting to
// zero values.
func Decode(data []byte) (*Config, error) {
	version, err := versionOf(data)
	if err != nil {
		return nil, err
	}
	if version > CurrentVersion {
		return nil, fmt.Errorf("configuration version %d is newer than this binary supports (%d); upgrade rigscope first", version, CurrentVersion)
	}
	if version != CurrentVersion {
		return nil, fmt.Errorf("configuration version %d was not migrated to %d", version, CurrentVersion)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, err
	}
	if err := requireCurrentFields(data); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// versionOf reads only the schema version, so a newer file is refused before
// any field of the current schema is interpreted.
func versionOf(data []byte) (int, error) {
	var header struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return 0, err
	}
	if header.Version == nil {
		return 0, errors.New(`missing "version" field`)
	}
	if *header.Version < 0 {
		return 0, fmt.Errorf("invalid configuration version %d", *header.Version)
	}
	return *header.Version, nil
}

// requireCurrentFields rejects configurations that omit the fields introduced
// with the sharing policy and opt-in hardware access, instead of silently
// decoding them as false.
func requireCurrentFields(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	components, ok := objectValue(root, "components")
	if !ok {
		return nil
	}
	collector, ok := objectValue(components, "collector")
	if !ok {
		return nil
	}
	sharing, ok := objectValue(collector, "sharing")
	if !ok {
		return errors.New(`missing "components.collector.sharing"`)
	}
	for _, key := range []string{
		"cpu", "memory", "swap", "gpus", "disks", "network",
		"networkAddresses", "processes", "processArguments",
	} {
		if _, ok := sharing[key]; !ok {
			return fmt.Errorf(`missing "components.collector.sharing.%s"`, key)
		}
	}
	hardware, ok := objectValue(collector, "hardware")
	if !ok {
		return errors.New(`missing "components.collector.hardware"`)
	}
	if _, ok := hardware["gpuRegisters"]; !ok {
		return errors.New(`missing "components.collector.hardware.gpuRegisters"`)
	}
	return nil
}

func objectValue(raw map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	value, ok := raw[key]
	if !ok {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		return nil, false
	}
	return object, true
}

// Save validates, normalises and writes the configuration atomically with mode
// 0600. There is never a partially written file.
func Save(path string, cfg *Config) error {
	return SaveWithMode(path, cfg, 0o600)
}

// SaveWithMode writes the configuration atomically with an explicit mode.
// System installs use 0640 so the shared rigscope group can read it.
func SaveWithMode(path string, cfg *Config, perm os.FileMode) error {
	if cfg == nil {
		return errors.New("configuration is empty")
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	data = append(data, '\n')
	if err := fsutil.WriteFileAtomic(path, data, perm); err != nil {
		return fmt.Errorf("write configuration %s: %w", path, err)
	}
	return nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("configuration contains more than one JSON value")
	}
	return err
}
