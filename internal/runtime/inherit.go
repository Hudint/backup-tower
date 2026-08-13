package runtime

import (
	"context"
	"fmt"
	"slices"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// ImageDefaults returns the container configuration an image bakes in.
func (c *Client) ImageDefaults(ctx context.Context, ref string) (*dockerspec.DockerOCIImageConfig, error) {
	res, err := c.api.ImageInspect(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("inspect image %s: %w", ref, err)
	}
	return res.Config, nil
}

// StripImageDefaults removes from a captured container configuration everything
// that merely came from the image it was created on.
//
// This matters only when recreating a container on a *different* image, and then
// it matters a great deal. The engine's inspect response does not distinguish
// what the operator asked for from what the image supplied: CMD, ENTRYPOINT,
// HEALTHCHECK, ENV and LABEL all appear as though they were requested. Passing
// that whole set back pins the new container to the old image's defaults, so a
// release that changes its entrypoint or its healthcheck runs with neither —
// and, worst of all, a broken release can pass a health gate it should fail,
// because the healthcheck being applied is the previous version's.
//
// Values that differ from the image were genuinely set by the operator and are
// kept untouched.
func StripImageDefaults(cfg *container.Config, img *dockerspec.DockerOCIImageConfig) []string {
	if cfg == nil || img == nil {
		return nil
	}
	var dropped []string

	if slices.Equal(cfg.Cmd, img.Cmd) {
		cfg.Cmd = nil
		dropped = append(dropped, "cmd")
	}
	if slices.Equal([]string(cfg.Entrypoint), img.Entrypoint) {
		cfg.Entrypoint = nil
		dropped = append(dropped, "entrypoint")
	}
	if sameHealthcheck(cfg.Healthcheck, img.Healthcheck) {
		cfg.Healthcheck = nil
		dropped = append(dropped, "healthcheck")
	}
	if cfg.WorkingDir == img.WorkingDir {
		cfg.WorkingDir = ""
	}
	if cfg.User == img.User {
		cfg.User = ""
	}
	if cfg.StopSignal == img.StopSignal {
		cfg.StopSignal = ""
	}

	// Env and Labels are unions of image and operator values, so they are
	// filtered entry by entry rather than compared as a whole.
	if kept := subtractSlice(cfg.Env, img.Env); len(kept) != len(cfg.Env) {
		dropped = append(dropped, fmt.Sprintf("%d env entries", len(cfg.Env)-len(kept)))
		cfg.Env = kept
	}
	if n := subtractMap(cfg.Labels, img.Labels); n > 0 {
		dropped = append(dropped, fmt.Sprintf("%d labels", n))
	}

	for port := range img.ExposedPorts {
		// The image spec keys ports as strings ("8080/tcp"); the container API
		// keys them as a parsed struct.
		if parsed, err := network.ParsePort(port); err == nil {
			delete(cfg.ExposedPorts, parsed)
		}
	}
	for vol := range img.Volumes {
		delete(cfg.Volumes, vol)
	}

	return dropped
}

func sameHealthcheck(a *container.HealthConfig, b *dockerspec.HealthcheckConfig) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return slices.Equal(a.Test, b.Test) &&
		a.Interval == b.Interval &&
		a.Timeout == b.Timeout &&
		a.StartPeriod == b.StartPeriod &&
		a.Retries == b.Retries
}

// subtractSlice returns the entries of got that are not in base.
func subtractSlice(got, base []string) []string {
	if len(base) == 0 {
		return got
	}
	inBase := make(map[string]bool, len(base))
	for _, v := range base {
		inBase[v] = true
	}
	out := make([]string, 0, len(got))
	for _, v := range got {
		if !inBase[v] {
			out = append(out, v)
		}
	}
	return out
}

// subtractMap deletes entries of got whose value matches base, returning how
// many were removed.
func subtractMap(got, base map[string]string) int {
	if len(base) == 0 {
		return 0
	}
	var n int
	for k, v := range base {
		if cur, ok := got[k]; ok && cur == v {
			delete(got, k)
			n++
		}
	}
	return n
}
