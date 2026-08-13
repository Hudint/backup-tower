// Package runtime wraps the container engine API. It speaks the Docker Engine
// API, which Podman also serves, so both runtimes go through the same code path.
package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/client"

	"github.com/hudint/backup-tower/internal/version"
)

// Flavor identifies which engine we are talking to. Some behaviour differs
// between them, most notably where volume data lives on disk.
type Flavor string

const (
	FlavorDocker  Flavor = "docker"
	FlavorPodman  Flavor = "podman"
	FlavorUnknown Flavor = "unknown"
)

// Client is a thin, opinionated wrapper around the engine API.
type Client struct {
	api    *client.Client
	flavor Flavor
	server string // engine version string, for logging
}

// New connects to the engine. The host is taken from DOCKER_HOST when set and
// falls back to the platform default socket otherwise. The API version is
// negotiated so we neither fail on older engines nor lose newer fields.
func New(ctx context.Context) (*Client, error) {
	api, err := client.New(
		client.WithHostFromEnv(),
		client.WithTLSClientConfigFromEnv(),
		client.WithAPIVersionNegotiation(),
		client.WithUserAgent(version.UserAgent()),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to container engine: %w", err)
	}

	c := &Client{api: api, flavor: FlavorUnknown}
	if err := c.probe(ctx); err != nil {
		api.Close()
		return nil, err
	}
	return c, nil
}

// probe pings the engine and works out which implementation answered.
func (c *Client) probe(ctx context.Context) error {
	if _, err := c.api.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		return fmt.Errorf("ping container engine at %s: %w", c.api.DaemonHost(), err)
	}

	// Podman serves the Docker API but names itself in the platform or component
	// version info. Failing to identify it is not fatal — it only costs us the
	// engine-specific fast paths.
	c.flavor = FlavorDocker
	info, err := c.api.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return nil
	}
	c.server = info.Version
	if isPodman(info.Platform.Name) {
		c.flavor = FlavorPodman
	}
	for _, comp := range info.Components {
		if isPodman(comp.Name) {
			c.flavor = FlavorPodman
		}
	}
	return nil
}

func isPodman(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "podman") || strings.Contains(s, "libpod")
}

// Flavor reports the detected engine implementation.
func (c *Client) Flavor() Flavor { return c.flavor }

// ServerVersion reports the engine version string, empty if it could not be read.
func (c *Client) ServerVersion() string { return c.server }

// APIVersion reports the negotiated API version.
func (c *Client) APIVersion() string { return c.api.ClientVersion() }

// Host reports the endpoint we are connected to.
func (c *Client) Host() string { return c.api.DaemonHost() }

// Close releases the underlying HTTP client.
func (c *Client) Close() error { return c.api.Close() }
