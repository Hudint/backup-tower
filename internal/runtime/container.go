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

	// Detailed reports whether Inspect and Raw are populated. A container that
	// came from a listing carries only what the engine puts in a summary, which
	// is everything needed to decide *whether* to act on it and nothing like
	// enough to recreate it. Call Detail before using Inspect or Raw.
	Detailed bool `json:"-"`
	// Inspect is the decoded engine response. Only set when Detailed.
	Inspect container.InspectResponse `json:"-"`
	// Raw is the untouched JSON from the engine. This is what gets persisted,
	// so fields added by future engine versions survive a round trip. Only set
	// when Detailed.
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
//
// This is one API call, not one per container. The summary carries names,
// labels, image and mounts — everything the selection engine reads — and none of
// the configuration needed to recreate a container. The containers it returns
// are therefore not Detailed; whoever needs more calls Detail on the few that
// were actually selected. The difference is not academic: the daemon evaluates
// every container on the host once a minute.
func (c *Client) List(ctx context.Context, includeStopped bool) ([]*Container, error) {
	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: includeStopped})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]*Container, 0, len(res.Items))
	for _, summary := range res.Items {
		item := fromSummary(summary)
		// A listing reports the image the container resolves to, not the
		// reference it was created from, and when that image has lost its tags
		// the engine fills the field with the id instead. Both the rule file's
		// image globs and the update check read this as a reference, so the id
		// would silently stop matching and turn a checkable container into an
		// unnameable one. Inspecting recovers what the operator actually asked
		// for — and only for the few containers where it is missing.
		if summaryGaveTheImageID(item.Image, item.ImageID) {
			if full, err := c.Inspect(ctx, item.ID); err == nil {
				out = append(out, full)
				continue
			}
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// summaryGaveTheImageID reports whether a listing handed back the image id in
// place of a reference, which is what happens once an image has no repo tags
// left.
func summaryGaveTheImageID(image, imageID string) bool {
	if image == "" {
		return true
	}
	if len(image) < 12 {
		return false
	}
	return strings.HasPrefix(strings.TrimPrefix(imageID, "sha256:"), strings.TrimPrefix(image, "sha256:"))
}

// Detail returns the container with its full inspect payload, fetching it when
// the container came from a listing. The result may be the same pointer.
func (c *Client) Detail(ctx context.Context, in *Container) (*Container, error) {
	if in == nil {
		return nil, fmt.Errorf("no container to inspect")
	}
	if in.Detailed {
		return in, nil
	}
	return c.Inspect(ctx, in.ID)
}

// FindComposeService locates the container belonging to one compose service.
//
// After compose recreates a service the container has a new ID, and the name is
// compose's to choose — so it is looked up by the labels that identify it,
// which are the only stable handle.
func (c *Client) FindComposeService(ctx context.Context, project, service string) (*Container, error) {
	filters := client.Filters{}
	filters.Add("label", "com.docker.compose.project="+project)
	filters.Add("label", "com.docker.compose.service="+service)

	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("list containers of compose service %s/%s: %w", project, service, err)
	}
	switch len(res.Items) {
	case 0:
		return nil, fmt.Errorf("no container found for compose service %s/%s: %w", project, service, ErrNotFound)
	case 1:
		return c.Inspect(ctx, res.Items[0].ID)
	}

	// Scaled services have several containers. The newest is the one just
	// created; anything else would be a guess.
	newest := res.Items[0]
	for _, item := range res.Items[1:] {
		if item.Created > newest.Created {
			newest = item
		}
	}
	return c.Inspect(ctx, newest.ID)
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

// fromSummary builds a container from a listing entry. Everything the selection
// engine reads is present; Config, HostConfig and NetworkSettings are not, which
// is why the result is not marked Detailed.
func fromSummary(s container.Summary) *Container {
	c := &Container{
		ID:       s.ID,
		Image:    s.Image,
		ImageID:  s.ImageID,
		State:    string(s.State),
		Running:  s.State == container.StateRunning,
		Labels:   map[string]string{},
		Detailed: false,
	}
	if len(s.Names) > 0 {
		c.Name = strings.TrimPrefix(s.Names[0], "/")
	}
	for k, v := range s.Labels {
		c.Labels[k] = v
	}
	c.Mounts = mountsOf(s.Mounts)
	return c
}

func fromInspect(in container.InspectResponse, raw json.RawMessage) *Container {
	c := &Container{
		ID:       in.ID,
		Name:     strings.TrimPrefix(in.Name, "/"),
		ImageID:  in.Image,
		Labels:   map[string]string{},
		Detailed: true,
		Inspect:  in,
		Raw:      raw,
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
	c.Mounts = mountsOf(in.Mounts)
	return c
}

// mountsOf converts the engine's mount points, in stable order. Listings and
// inspect responses carry the same type here, so both go through this.
func mountsOf(in []container.MountPoint) []Mount {
	var out []Mount
	for _, m := range in {
		out = append(out, Mount{
			Type:        mountType(m.Type),
			Name:        m.Name,
			Source:      m.Source,
			Destination: m.Destination,
			ReadWrite:   m.RW,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Destination < out[j].Destination })
	return out
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
