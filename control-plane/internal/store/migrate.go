package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Migrate applies any *.sql files in migrationsDir whose numeric prefix
// isn't recorded in the migration table. No down migrations in v1.
// Filenames must be NNNN_name.sql.
func Migrate(ctx context.Context, db *sql.DB, migrationsDir string) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS migration (
			id         INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("ensure migration table: %w", err)
	}

	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, `SELECT id FROM migration`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		applied[id] = true
	}
	rows.Close()

	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations dir %s: %w", migrationsDir, err)
	}
	type mig struct {
		id   int
		name string
		path string
	}
	var migs []mig
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		under := strings.IndexByte(e.Name(), '_')
		if under <= 0 {
			return fmt.Errorf("migration filename must be NNNN_name.sql: %s", e.Name())
		}
		id, err := strconv.Atoi(e.Name()[:under])
		if err != nil {
			return fmt.Errorf("migration id parse %s: %w", e.Name(), err)
		}
		migs = append(migs, mig{id: id, name: e.Name(), path: filepath.Join(migrationsDir, e.Name())})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].id < migs[j].id })

	for _, m := range migs {
		if applied[m.id] {
			continue
		}
		body, err := os.ReadFile(m.path)
		if err != nil {
			return fmt.Errorf("read %s: %w", m.path, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO migration (id, name, applied_at) VALUES (?, ?, ?)`,
			m.id, m.name, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", m.name, err)
		}
	}
	return nil
}
