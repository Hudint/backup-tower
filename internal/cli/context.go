package cli

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/hudint/backup-tower/internal/config"
	"github.com/hudint/backup-tower/internal/runtime"
	"github.com/hudint/backup-tower/internal/snapshot/store"
)

// env bundles what most commands need: settings, a store and, when the command
// touches containers, an engine connection.
type env struct {
	cfg   config.Config
	store store.Store
	rt    *runtime.Client
	log   *slog.Logger
}

func (e *env) Close() {
	if e.rt != nil {
		_ = e.rt.Close()
	}
	if e.store != nil {
		_ = e.store.Close()
	}
}

// openEnv resolves configuration and opens the store. withEngine is false for
// commands that only read the backup directory, so listing snapshots keeps
// working when the engine is unreachable — which is exactly when someone is
// most likely to be looking.
func openEnv(cmd *cobra.Command, withEngine bool) (*env, error) {
	cfg, err := config.FromEnv()
	if err != nil {
		return nil, err
	}
	if dir, _ := cmd.Flags().GetString("backup-dir"); dir != "" {
		cfg.BackupDir = dir
	}

	e := &env{cfg: cfg, log: newLogger(cmd)}

	e.store, err = store.Open(cfg.BackupDir)
	if err != nil {
		return nil, err
	}

	if withEngine {
		e.rt, err = runtime.New(cmd.Context())
		if err != nil {
			e.Close()
			return nil, err
		}
	}
	return e, nil
}

func newLogger(cmd *cobra.Command) *slog.Logger {
	level := slog.LevelInfo
	if v, _ := cmd.Flags().GetBool("verbose"); v {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
