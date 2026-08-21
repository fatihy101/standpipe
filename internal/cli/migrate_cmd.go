package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/store"
)

// runMigrate implements `standpipe migrate` and `standpipe migrate --dry-run`.
//
// It is in this slice for one reason: the gateway REFUSE to start
// against an outdated schema, and a freshly created database is at version 0.
// Without this command a new installation cannot be brought up at all, so the
// trigger path it is a precondition for could never be exercised.
func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("config", "standpipe.yaml", "path to standpipe.yaml")
	dryRun := fs.Bool("dry-run", false, "list pending migrations and apply nothing")
	snapshotDir := fs.String("snapshot-dir", "", "where to write the pre-migration VACUUM INTO snapshot")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	// A VACUUM INTO snapshot is taken before anything is applied, and
	// MigrateFile owns that. A version 0 database has nothing to snapshot.
	steps, snapshot, err := store.MigrateFile(context.Background(), store.Config{
		Path:         cfg.Storage.SQLitePath,
		MinFreeBytes: cfg.Storage.MinFreeBytes,
	}, *snapshotDir, *dryRun)
	if err != nil {
		return err
	}

	if snapshot != "" {
		fmt.Printf("snapshot: %s\n", snapshot)
	}
	if len(steps) == 0 {
		fmt.Printf("schema is current (version %d)\n", store.LatestSchemaVersion())
		return nil
	}
	verb := "applied"
	if *dryRun {
		verb = "pending"
	}
	for _, s := range steps {
		fmt.Printf("%s: %03d %s (%s)\n", verb, s.Version, s.Name, s.Checksum[:12])
	}
	return nil
}
