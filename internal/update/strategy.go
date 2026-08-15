package update

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/Hudint/backup-tower/internal/discover"
	"github.com/Hudint/backup-tower/internal/runtime"
	"github.com/Hudint/backup-tower/internal/snapshot"
)

// applier puts a new image into service.
type applier interface {
	// apply replaces the container and returns the ID of its replacement.
	apply(ctx context.Context, req *applyRequest) (string, []string, error)
	name() discover.Strategy
}

type applyRequest struct {
	Container *runtime.Container
	// Spec is the configuration captured by the pre-update snapshot, used by the
	// recreate path. Nil when no snapshot was taken.
	Spec *snapshot.Spec
	// Image is the reference to run.
	Image string
	// Suffix makes temporary names unique.
	Suffix string
}

// recreateApplier rebuilds the container through the engine API.
//
// This is the universal path: it needs nothing but the engine, and works for
// containers that were started by hand as much as for those from a compose file.
type recreateApplier struct {
	rt *runtime.Client
	up *Updater
}

func (r *recreateApplier) name() discover.Strategy { return discover.StrategyRecreate }

func (r *recreateApplier) apply(ctx context.Context, req *applyRequest) (string, []string, error) {
	var in container.InspectResponse
	if req.Spec != nil {
		// Prefer the captured spec: it was taken before anything was touched,
		// so it describes the container as it normally runs.
		captured, err := req.Spec.Container()
		if err != nil {
			return "", nil, err
		}
		in = *captured
	} else {
		// No snapshot was taken, so there is no captured spec. The candidate
		// came from a listing and carries only the summary; recreating needs the
		// whole configuration, so fetch it now.
		full, err := r.rt.Detail(ctx, req.Container)
		if err != nil {
			return "", nil, fmt.Errorf("read the current configuration of %s: %w", req.Container.Name, err)
		}
		in = full.Inspect
	}

	// The captured configuration cannot tell what the operator asked for apart
	// from what the old image supplied, so hand the old image's own defaults
	// along and let them be subtracted. Skipping this would run the new image
	// with the previous version's entrypoint, environment and healthcheck.
	inherited, err := r.rt.ImageDefaults(ctx, req.Container.ImageID)
	if err != nil {
		r.up.log.Warn("could not read the previous image's defaults; the new image may not get to apply its own",
			"container", req.Container.Name, "error", err)
	}

	newID, warnings, err := r.rt.Replace(ctx, req.Container, &in, runtime.ReplaceOptions{
		Name:          req.Container.Name,
		Image:         req.Image,
		ParkSuffix:    req.Suffix,
		InheritedFrom: inherited,
		Log:           r.up.log,
	})
	if err != nil {
		return "", warnings, err
	}
	if err := r.rt.Start(ctx, newID); err != nil {
		return newID, warnings, fmt.Errorf("start the replacement container: %w", err)
	}
	return newID, warnings, nil
}

// composeApplier hands the work to docker compose.
//
// For a container that came from a compose file, this is what its owner would
// do by hand, and it is what Komodo does when it redeploys. Recreating such a
// container through the API instead leaves the running state and the compose
// file describing two different things, which the next `compose up` silently
// resolves in favour of the file.
type composeApplier struct {
	rt      *runtime.Client
	up      *Updater
	binary  string
	timeout time.Duration
}

func (c *composeApplier) name() discover.Strategy { return discover.StrategyCompose }

func (c *composeApplier) apply(ctx context.Context, req *applyRequest) (string, []string, error) {
	labels := req.Container.Labels
	project := labels["com.docker.compose.project"]
	service := labels["com.docker.compose.service"]
	if project == "" || service == "" {
		return "", nil, fmt.Errorf("the container carries no compose project or service label")
	}

	args := []string{"compose", "--project-name", project}
	if dir := labels["com.docker.compose.project.working_dir"]; dir != "" {
		args = append(args, "--project-directory", dir)
	}
	for _, f := range splitList(labels["com.docker.compose.project.config_files"]) {
		args = append(args, "-f", f)
	}
	for _, f := range splitList(labels["com.docker.compose.project.environment_file"]) {
		args = append(args, "--env-file", f)
	}
	// --no-deps keeps the blast radius to this one service; without it, compose
	// would happily restart every dependency alongside it.
	args = append(args, "up", "-d", "--no-deps", service)

	timeout := c.timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, c.binary, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	c.up.log.Info("running compose", "container", req.Container.Name, "project", project, "service", service)
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("docker %s: %w: %s",
			strings.Join(args, " "), err, truncate(strings.TrimSpace(out.String()), 800))
	}

	// Compose creates a new container; find it by project and service rather
	// than by name, because compose decides the name itself.
	replacement, err := c.rt.FindComposeService(ctx, project, service)
	if err != nil {
		return "", nil, fmt.Errorf("compose reported success but the new container could not be found: %w", err)
	}
	return replacement.ID, nil, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// pickApplier resolves the strategy for one container and explains any downgrade.
//
// A strategy that cannot run is worse than one that was never chosen, so an
// unavailable compose binary falls back rather than failing — but says so.
func (u *Updater) pickApplier(cand *discover.Candidate) (applier, string) {
	want := cand.Strategy
	if want == discover.StrategyCompose {
		if u.composeBinary == "" {
			return &recreateApplier{rt: u.rt, up: u}, "the compose plugin is not available here, so the container is being recreated through the API instead"
		}
		if cand.ComposeFile == "" {
			return &recreateApplier{rt: u.rt, up: u}, "the compose file is not readable from here, so the container is being recreated through the API instead"
		}
		return &composeApplier{rt: u.rt, up: u, binary: u.composeBinary, timeout: u.composeTimeout}, ""
	}
	return &recreateApplier{rt: u.rt, up: u}, ""
}

// findComposeBinary locates a docker CLI that has the compose plugin.
func findComposeBinary() string {
	path, err := exec.LookPath("docker")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, path, "compose", "version").Run(); err != nil {
		return ""
	}
	return path
}
