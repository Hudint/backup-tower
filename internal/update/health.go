package update

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Hudint/backup-tower/internal/runtime"
)

// HealthVerdict is the outcome of the gate.
type HealthVerdict string

const (
	// HealthPassed means the container came up and stayed up.
	HealthPassed HealthVerdict = "passed"
	// HealthFailed means it did not.
	HealthFailed HealthVerdict = "failed"
	// HealthUnchecked means the gate was disabled.
	HealthUnchecked HealthVerdict = "unchecked"
)

// HealthResult explains the verdict.
type HealthResult struct {
	Verdict HealthVerdict
	// Method records how the judgement was reached, since the two paths give
	// very different levels of confidence.
	Method   string
	Reason   string
	Duration time.Duration
}

// Passed reports whether the update may stand.
func (h *HealthResult) Passed() bool { return h.Verdict != HealthFailed }

// HealthOptions configures the gate.
type HealthOptions struct {
	// Timeout bounds how long to wait for a healthcheck to turn healthy.
	Timeout time.Duration
	// Settle is how long a container without a healthcheck must keep running
	// before the update counts as successful.
	Settle time.Duration
	// Disabled skips the gate entirely.
	Disabled bool
}

// DefaultHealthOptions are deliberately patient on the healthcheck path and
// brief on the other: waiting for a real healthcheck is informative, while
// waiting on a container that has no opinion about its own health only delays
// the run.
func DefaultHealthOptions() HealthOptions {
	return HealthOptions{Timeout: 2 * time.Minute, Settle: 15 * time.Second}
}

// checkHealth decides whether an updated container is working.
//
// A container that is merely running proves very little — a broken application
// can sit in a restart loop for a while, or start and fail on the first request.
// When the image declares a healthcheck, that is by far the better signal and is
// used in preference. Without one, the honest fallback is to watch for a crash
// loop and say plainly that this is all that was verified.
func checkHealth(ctx context.Context, rt *runtime.Client, containerID string, opts HealthOptions) *HealthResult {
	if opts.Disabled {
		return &HealthResult{Verdict: HealthUnchecked, Method: "disabled"}
	}

	started := time.Now()
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultHealthOptions().Timeout
	}
	settle := opts.Settle
	if settle <= 0 {
		settle = DefaultHealthOptions().Settle
	}

	deadline := time.Now().Add(timeout)
	const interval = time.Second

	var baselineRestarts int
	haveBaseline := false
	// Whether a healthcheck exists is decided once, from the configuration, not
	// from whether the engine has populated the health state yet. Right after a
	// start it has not, and treating that moment as "no healthcheck" would let
	// the weaker runtime check decide a case the healthcheck was there to judge.
	hasHealthcheck := false
	checkedConfig := false

	for {
		c, err := rt.Inspect(ctx, containerID)
		if err != nil {
			return &HealthResult{
				Verdict:  HealthFailed,
				Method:   "inspect",
				Reason:   fmt.Sprintf("the container could not be inspected: %v", err),
				Duration: time.Since(started),
			}
		}
		if !haveBaseline {
			baselineRestarts = c.Inspect.RestartCount
			haveBaseline = true
		}
		if !checkedConfig {
			hasHealthcheck = declaresHealthcheck(c)
			checkedConfig = true
		}

		state := c.Inspect.State
		if state == nil {
			return &HealthResult{Verdict: HealthFailed, Method: "inspect", Reason: "the engine reported no state", Duration: time.Since(started)}
		}

		// An exited container is a decided case, whatever the healthcheck says.
		if !state.Running {
			reason := fmt.Sprintf("the container is %s", state.Status)
			if state.ExitCode != 0 {
				reason = fmt.Sprintf("the container exited with status %d", state.ExitCode)
			}
			if state.Error != "" {
				reason += ": " + state.Error
			}
			return &HealthResult{Verdict: HealthFailed, Method: "runtime", Reason: reason, Duration: time.Since(started)}
		}

		if state.Health != nil {
			switch state.Health.Status {
			case "healthy":
				return &HealthResult{Verdict: HealthPassed, Method: "healthcheck", Duration: time.Since(started)}
			case "unhealthy":
				return &HealthResult{
					Verdict:  HealthFailed,
					Method:   "healthcheck",
					Reason:   "the image's healthcheck reports unhealthy: " + lastHealthOutput(c),
					Duration: time.Since(started),
				}
			}
			// "starting": keep waiting, that is what the timeout is for.
		} else if !hasHealthcheck && time.Since(started) >= settle {
			// No healthcheck to consult. Surviving the settle window without
			// restarting is the most that can honestly be claimed.
			if c.Inspect.RestartCount > baselineRestarts {
				return &HealthResult{
					Verdict:  HealthFailed,
					Method:   "restart-count",
					Reason:   fmt.Sprintf("the container restarted %d times within %s, which is a crash loop", c.Inspect.RestartCount-baselineRestarts, settle),
					Duration: time.Since(started),
				}
			}
			return &HealthResult{
				Verdict:  HealthPassed,
				Method:   "runtime",
				Reason:   fmt.Sprintf("no healthcheck in the image; verified only that it kept running for %s", settle),
				Duration: time.Since(started),
			}
		}

		if time.Now().After(deadline) {
			status := "starting"
			if state.Health != nil {
				status = string(state.Health.Status)
			} else if hasHealthcheck {
				status = "waiting for its first healthcheck"
			}
			return &HealthResult{
				Verdict:  HealthFailed,
				Method:   "healthcheck",
				Reason:   fmt.Sprintf("still %s after %s", status, timeout),
				Duration: time.Since(started),
			}
		}

		select {
		case <-ctx.Done():
			return &HealthResult{Verdict: HealthFailed, Method: "cancelled", Reason: ctx.Err().Error(), Duration: time.Since(started)}
		case <-time.After(interval):
		}
	}
}

// declaresHealthcheck reports whether the container has a healthcheck at all,
// distinguishing a real one from the explicit NONE that disables an inherited one.
func declaresHealthcheck(c *runtime.Container) bool {
	if c.Inspect.Config == nil || c.Inspect.Config.Healthcheck == nil {
		return false
	}
	test := c.Inspect.Config.Healthcheck.Test
	if len(test) == 0 {
		return false
	}
	return test[0] != "NONE"
}

func lastHealthOutput(c *runtime.Container) string {
	if c.Inspect.State == nil || c.Inspect.State.Health == nil {
		return ""
	}
	log := c.Inspect.State.Health.Log
	if len(log) == 0 {
		return "no output"
	}
	return truncate(strings.TrimSpace(log[len(log)-1].Output), 300)
}
