package runtime

import (
	"context"
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// Volume describes a named volume as the engine sees it.
type Volume struct {
	Name string `json:"name"`
	// Mountpoint is the path on the engine host holding the data. It is only
	// usable directly when we run on that same host with sufficient rights;
	// otherwise the helper container is the way in.
	Mountpoint string            `json:"mountpoint"`
	Driver     string            `json:"driver"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// InspectVolume looks up a named volume.
func (c *Client) InspectVolume(ctx context.Context, name string) (*Volume, error) {
	res, err := c.api.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("volume %q: %w", name, ErrNotFound)
		}
		return nil, fmt.Errorf("inspect volume %q: %w", name, err)
	}
	return &Volume{
		Name:       res.Volume.Name,
		Mountpoint: res.Volume.Mountpoint,
		Driver:     res.Volume.Driver,
		Labels:     res.Volume.Labels,
	}, nil
}

// EnsureVolume returns the named volume, creating it if it is gone.
//
// A restore has to cope with a volume that was deleted along with its container.
// Recreating it empty is the right starting point: the archive is about to fill
// it anyway.
func (c *Client) EnsureVolume(ctx context.Context, name string) (*Volume, error) {
	v, err := c.InspectVolume(ctx, name)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	res, err := c.api.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name})
	if err != nil {
		return nil, fmt.Errorf("create volume %q: %w", name, err)
	}
	return &Volume{
		Name:       res.Volume.Name,
		Mountpoint: res.Volume.Mountpoint,
		Driver:     res.Volume.Driver,
		Labels:     res.Volume.Labels,
	}, nil
}
