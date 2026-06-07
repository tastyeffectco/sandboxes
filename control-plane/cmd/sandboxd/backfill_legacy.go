package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sandboxd/control-plane/internal/logging"
	"github.com/sandboxd/control-plane/internal/store"
)

// runBackfillLegacy implements the one-shot subcommand
//
//	sandboxd backfill-legacy --external-user-id=LEGACY
//
// roadmap/phase-8 §3. It is idempotent — a second run affects zero
// rows. The operator runs it with sandboxd stopped (the daemon and
// this subcommand both open the same SQLite file; the single-writer
// model assumes one process). It:
//
//  1. sets external_user_id = sentinel on every sandbox row where it
//     is currently NULL;
//  2. inserts a workspace_owner row for every workspace `.img` on disk
//     that does not already have one;
//  3. writes one audit_log row, action='backfill.legacy', with counts.
func runBackfillLegacy(args []string) int {
	fs := flag.NewFlagSet("backfill-legacy", flag.ContinueOnError)
	sentinel := fs.String("external-user-id", "LEGACY",
		"sentinel external_user_id to assign to pre-Phase-8 rows")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sentinel == "" {
		fmt.Fprintln(os.Stderr, "backfill-legacy: --external-user-id must not be empty")
		return 2
	}
	log := logging.NewLogger()
	ctx := context.Background()

	migrations := envDefault("SANDBOXD_MIGRATIONS", migrationsDir)
	if _, err := os.Stat(migrations); err != nil {
		if exe, e := os.Executable(); e == nil {
			alt := filepath.Join(filepath.Dir(exe), "..", "..", "migrations")
			if _, e2 := os.Stat(alt); e2 == nil {
				migrations = alt
			}
		}
	}
	dataDir := envDefault("SANDBOXD_DATA_DIR", defaultDataDir)
	workspacesRoot := filepath.Join(dataDir, "workspaces")
	dsn := fmt.Sprintf("file:%s?_journal=WAL&_busy_timeout=5000&_fk=1",
		envDefault("SANDBOXD_DB", filepath.Join(dataDir, "state", "sandboxd.db")))
	st, err := store.Open(ctx, dsn, migrations)
	if err != nil {
		log.Error("backfill-legacy: store open failed", "err", err.Error())
		return 1
	}
	defer func() { _ = st.Close() }()

	// 1. sentinel external_user_id on every NULL sandbox row.
	updated, err := st.BackfillLegacySandboxes(ctx, *sentinel)
	if err != nil {
		log.Error("backfill-legacy: sandbox backfill failed", "err", err.Error())
		return 1
	}

	// 2. workspace_owner for every .img on disk without one.
	owners := 0
	entries, err := os.ReadDir(workspacesRoot)
	if err != nil && !os.IsNotExist(err) {
		log.Error("backfill-legacy: read workspaces dir failed",
			"dir", workspacesRoot, "err", err.Error())
		return 1
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".img") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".img")
		inserted, err := st.EnsureWorkspaceOwner(ctx, id, *sentinel)
		if err != nil {
			log.Error("backfill-legacy: ensure workspace_owner failed",
				"sandbox_id", id, "err", err.Error())
			return 1
		}
		if inserted {
			owners++
		}
	}

	// 3. one audit row.
	if err := st.InsertAudit(ctx, time.Now().Unix(),
		"operator", "backfill-legacy", "", *sentinel,
		"backfill.legacy", *sentinel,
		fmt.Sprintf(`{"sandbox_rows_updated":%d,"workspace_owner_rows_inserted":%d}`, updated, owners),
	); err != nil {
		log.Warn("backfill-legacy: audit write failed", "err", err.Error())
	}

	log.Info("backfill-legacy: done",
		"external_user_id", *sentinel,
		"sandbox_rows_updated", updated,
		"workspace_owner_rows_inserted", owners)
	fmt.Printf("backfill-legacy: external_user_id=%s sandbox_rows_updated=%d workspace_owner_rows_inserted=%d\n",
		*sentinel, updated, owners)
	return 0
}
