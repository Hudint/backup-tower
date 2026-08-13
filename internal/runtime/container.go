package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// MountType distinguishes the kinds of storage attached to a container. We only
// ever archive volumes and binds; tmpfs is memory-backed and has nothing to save.
type MountType string

const (
	MountVolume MountType = "volume"
	MountBind   MountType = "bind"
	MountTmpfs  MountType = "tmpfs"
	MountOther  MountType = "other"
)

// Mount is one piece of storage attached to a container.
type Mount struct {
	Type MountType `json:"type"`
	// Name is the volume name; empty for bind mounts.
	Name string `json:"name,omitempty"`
	// Source is where the data lives as seen by the engine host: the volume's
	// mountpoint under /var/lib/docker/volumes, or the host path for a bind.
	Source string `json:"source"`
	// Destination is the path inside the container.
	Destination string `json:"destination"`
	ReadWrite   bool   `json:"read_write"`
}

// Container is the subset of engine state the rest of the tool works with. The
// full inspect payload is kept alongside it so nothing is lost on the way to
// spec.json.
type Container struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	ImageID string            `json:"image_id"`
	State   string            `json:"state"`
	Running bool              `json:"running"`
	Labels  map[string]string `json:"labels"`
	Mounts  []Mount           `json:"mounts"`

	// Inspect is the decoded engine response.
	Inspect container.InspectResponse `json:"-"`
	// Raw is the untouched JSON from the engine. This is what gets persisted,
	// so fields added by future engine versions survive a round trip.
	Raw json.RawMessage `json:"-"`
}

// VolumeMounts returns only the named-volume mounts, in stable order.
func (c *Container) VolumeMounts() []Mount {
	return c.mountsOfType(MountVolume)
}

// BindMounts returns only the bind mounts, in stable order.
func (c *Container) BindMounts() []Mount {
	return c.mountsOfType(MountBind)
}

func (c *Container) mountsOfType(t MountType) []Mount {
	var out []Mount
	for _, m := range c.Mounts {
		if m.Type == t {
			out = append(out, m)
		}
	}
	return out
}

// ComposeProject reports the compose project a container belongs to, empty if
// it was not created by compose.
func (c *Container) ComposeProject() string {
	return c.Labels["com.docker.compose.project"]
}

// ComposeService reports the compose service name, empty if not compose-managed.
func (c *Container) ComposeService() string {
	return c.Labels["com.docker.compose.service"]
}

// Inspect looks up a container by name or ID.
func (c *Client) Inspect(ctx context.Context, ref string) (*Container, error) {
	res, err := c.api.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("container %q: %w", ref, ErrNotFound)
		}
		return nil, fmt.Errorf("inspect container %q: %w", ref, err)
	}
	return fromInspect(res.Container, res.Raw), nil
}

// List returns all containers, optionally including stopped ones.
func (c *Client) List(ctx context.Context, includeStopped bool) ([]*Container, error) {
	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: includeStopped})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]*Container, 0, len(res.Items))
	for _, summary := range res.Items {
		// The summary lacks Config and HostConfig, and we need those for the
		// spec. Inspect each one rather than carry a half-populated struct.
		full, err := c.Inspect(ctx, summary.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // vanished between list and inspect
			}
			return nil, err
		}
		out = append(out, full)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Stop stops a container and waits for it to actually be down. A nil timeout
// leaves the decision to the engine and the container's own configuration.
func (c *Client) Stop(ctx context.Context, id string, timeout *int) error {
	if _, err := c.api.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: timeout}); err != nil {
		return fmt.Errorf("stop container %s: %w", id, err)
	}
	return c.waitForState(ctx, id, false)
}

// Start starts a container and waits until the engine reports it running.
func (c *Client) Start(ctx context.Context, id string) error {
	if _, err := c.api.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start container %s: %w", id, err)
	}
	return c.waitForState(ctx, id, true)
}

// waitForState polls until the container reaches the wanted running state. The
// engine returns from stop/start before the state settles, and acting on a
// half-stopped container is exactly how a snapshot ends up inconsistent.
func (c *Client) waitForState(ctx context.Context, id string, running bool) error {
	const (
		interval = 100 * time.Millisecond
		timeout  = 2 * time.Minute
	)
	deadline := time.Now().Add(timeout)
	for {
		cur, err := c.Inspect(ctx, id)
		if err != nil {
			return err
		}
		if cur.Running == running {
			return nil
		}
		if time.Now().After(deadline) {
			want := "stopped"
			if running {
				want = "running"
			}
			return fmt.Errorf("container %s did not reach state %s within %s (state: %s)", id, want, timeout, cur.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// ErrNotFound is returned when the engine does not know the requested object.
var ErrNotFound = errors.New("not found")

func fromInspect(in container.InspectResponse, raw json.RawMessage) *Container {
	c := &Container{
		ID:      in.ID,
		Name:    strings.TrimPrefix(in.Name, "/"),
		ImageID: in.Image,
		Labels:  map[string]string{},
		Inspect: in,
		Raw:     raw,
	}
	if in.Config != nil {
		c.Image = in.Config.Image
		for k, v := range in.Config.Labels {
			c.Labels[k] = v
		}
	}
	if in.State != nil {
		c.State = string(in.State.Status)
		c.Running = in.State.Running
	}
	for _, m := range in.Mounts {
		c.Mounts = append(c.Mounts, Mount{
			Type:        mountType(m.Type),
			Name:        m.Name,
			Source:      m.Source,
			Destination: m.Destination,
			ReadWrite:   m.RW,
		})
	}
	sort.Slice(c.Mounts, func(i, j int) bool { return c.Mounts[i].Destination < c.Mounts[j].Destination })
	return c
}

func mountType(t mount.Type) MountType {
	switch t {
	case mount.TypeVolume:
		return MountVolume
	case mount.TypeBind:
		return MountBind
	case mount.TypeTmpfs:
		return MountTmpfs
	default:
		return MountOther
	}
}
