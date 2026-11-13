package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadSchemaLedgerReadsWithoutMigrating(t *testing.T) {
	ctx := testContext(t)
	dir := privateTempDir(t)
	path := filepath.Join(dir, "ledger.db")
	database, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	want := database.Schema()
	if err := database.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	info, err := ReadSchemaLedger(ctx, path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if info.Version != want.Version || info.CompatibleDown != want.CompatibleDown {
		t.Fatalf("ledger = %+v, want version %d floor %d", info, want.Version, want.CompatibleDown)
	}
	if len(info.Applied) != len(want.Applied) {
		t.Fatalf("applied = %d, want %d", len(info.Applied), len(want.Applied))
	}
}

func TestReadSchemaLedgerRejectsMissingOrForeignFiles(t *testing.T) {
	ctx := testContext(t)
	dir := privateTempDir(t)
	if _, err := ReadSchemaLedger(ctx, filepath.Join(dir, "missing.db")); !errors.Is(err, ErrNoSchemaLedger) {
		t.Fatalf("missing db err = %v", err)
	}
	foreign := filepath.Join(dir, "foreign.db")
	if err := os.WriteFile(foreign, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSchemaLedger(ctx, foreign); !errors.Is(err, ErrNoSchemaLedger) {
		t.Fatalf("foreign file err = %v", err)
	}
}
