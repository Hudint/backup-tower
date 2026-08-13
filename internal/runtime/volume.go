package runtime

import (
	"context"
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
