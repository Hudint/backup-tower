package update

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Hudint/backup-tower/internal/runtime"
)

// HookPhase names when a hook runs.
type HookPhase string

const (
	// HookPreUpdate signals the application that a change is coming — the place
	// to enter maintenance mode.
	HookPreUpdate HookPhase = "pre-update"
	// HookPreSnapshot runs immediately before the snapshot, while the container
	// is still up. The natural home for a pg_dump.
	HookPreSnapshot HookPhase = "pre-snapshot"
	// HookPostUpdate runs once the replacement is up and healthy.
	HookPostUpdate HookPhase = "post-update"
)

// HookResult records what a hook did.
type HookResult struct {
	Phase    HookPhase
	Command  string
	ExitCode int
	Output   string
	Duration time.Duration
	Err      error
}

// runHook executes a command inside a container.
//
// Pre-hooks are allowed to veto: a dump that failed means the snapshot would be
// taken without it, and continuing regardless would produce a backup that looks
// complete and is not.
func runHook(ctx context.Context, rt *runtime.Client, containerID string, phase HookPhase, command string, timeout time.Duration) *HookResult {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	started := time.Now()
	res := &HookResult{Phase: phase, Command: command}

	hookCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		hookCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	exit, output, err := rt.Exec(hookCtx, containerID, []string{"/bin/sh", "-c", command})
	res.Duration = time.Since(started)
	res.ExitCode = exit
	res.Output = strings.TrimSpace(output)

	switch {
	case err != nil:
		res.Err = fmt.Errorf("%s hook could not be run: %w", phase, err)
	case exit != 0:
		res.Err = fmt.Errorf("%s hook exited with status %d: %s", phase, exit, truncate(res.Output, 500))
	}
	return res
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
