package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

// schemaSQL is the full DDL for the store, embedded at build time so the
// binary carries no external file dependency.
//
//go:embed schema.sql
var schemaSQL string

// migrate applies schemaSQL against db. Every statement in schema.sql is
// written as CREATE TABLE/INDEX IF NOT EXISTS, so running migrate against an
// already-current database is a no-op: Open can call it unconditionally on
// every process start (including a second process opening the same file)
// without needing a separate schema-version table.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	return nil
}
