package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// RecreateOptions describes how to rebuild a container from a captured spec.
type RecreateOptions struct {
	// Name the new container should carry. Usually the original name, which
	// means the old container must be gone first.
	Name string
	// Image reference to run. This is the point of the whole exercise during a
	// rollback: same configuration, different image.
	Image string
}

// Recreate builds a container from a previously captured inspect response.
//
// The inspect response mixes configuration with runtime state, and handing the
// state back to the engine is how recreated containers end up subtly wrong —
// a hostname frozen to a dead container's ID, DNS aliases pointing at an ID that
// no longer exists, IP addresses that are already taken. Everything below is
// about separating the two.
func (c *Client) Recreate(ctx context.Context, in *container.InspectResponse, opts RecreateOptions) (string, []string, error) {
	if in.Config == nil {
		return "", nil, fmt.Errorf("captured spec has no container configuration")
	}

	cfg := *in.Config
	cfg.Image = opts.Image

	// The engine defaults Hostname to the container's own short ID. Carrying
	// that over would pin the new container to the identity of the old one.
	if isShortIDOf(cfg.Hostname, in.ID) {
		cfg.Hostname = ""
	}

	var hostCfg container.HostConfig
	if in.HostConfig != nil {
		hostCfg = *in.HostConfig
	}

	netCfg, warnings := networkingConfig(in)

	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:             opts.Name,
		Config:           &cfg,
		HostConfig:       &hostCfg,
		NetworkingConfig: netCfg,
	})
	if err != nil {
		return "", warnings, fmt.Errorf("create container %s: %w", opts.Name, err)
	}
	warnings = append(warnings, created.Warnings...)
	return created.ID, warnings, nil
}

// networkingConfig rebuilds the network attachments, keeping the parts that were
// configured and dropping the parts the engine assigned at runtime.
func networkingConfig(in *container.InspectResponse) (*network.NetworkingConfig, []string) {
	if in.NetworkSettings == nil || len(in.NetworkSettings.Networks) == 0 {
		return nil, nil
	}

	var warnings []string
	endpoints := make(map[string]*network.EndpointSettings, len(in.NetworkSettings.Networks))

	names := make([]string, 0, len(in.NetworkSettings.Networks))
	for name := range in.NetworkSettings.Networks {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		ep := in.NetworkSettings.Networks[name]
		if ep == nil {
			continue
		}
		// Take only what was asked for, never what was handed out. Copying the
		// assigned IPAddress, Gateway or EndpointID back would either be ignored
		// or collide with an address the engine has since given to someone else.
		rebuilt := &network.EndpointSettings{
			IPAMConfig: ep.IPAMConfig,
			Links:      ep.Links,
			DriverOpts: ep.DriverOpts,
			Aliases:    cleanAliases(ep.Aliases, in.ID),
		}
		endpoints[name] = rebuilt
	}

	if len(endpoints) == 0 {
		return nil, warnings
	}
	return &network.NetworkingConfig{EndpointsConfig: endpoints}, warnings
}

// cleanAliases drops the aliases the engine derives from the container ID. They
// would otherwise resolve to a container that no longer exists.
func cleanAliases(aliases []string, containerID string) []string {
	var out []string
	for _, a := range aliases {
		if isShortIDOf(a, containerID) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func isShortIDOf(candidate, containerID string) bool {
	if candidate == "" || containerID == "" {
		return false
	}
	return strings.HasPrefix(containerID, candidate) && len(candidate) >= 12
}

// Remove deletes a container. Volumes are never removed with it: they hold the
// data this whole tool exists to protect.
func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	_, err := c.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{
		Force:         force,
		RemoveVolumes: false,
	})
	if err != nil {
		return fmt.Errorf("remove container %s: %w", id, err)
	}
	return nil
}

// Rename renames a container, which is how the previous one is moved aside
// before its replacement takes the original name.
func (c *Client) Rename(ctx context.Context, id, newName string) error {
	_, err := c.api.ContainerRename(ctx, id, client.ContainerRenameOptions{NewName: newName})
	if err != nil {
		return fmt.Errorf("rename container %s to %s: %w", id, newName, err)
	}
	return nil
}
