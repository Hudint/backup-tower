package runtime

import (
	"bytes"
	"context"
	"fmt"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// Exec runs a command inside a running container and returns its exit code
// together with its combined output.
//
// Output is captured rather than streamed because it is only interesting when
// something went wrong, and then it belongs in the error rather than scattered
// across the log.
func (c *Client) Exec(ctx context.Context, containerID string, cmd []string) (int, string, error) {
	created, err := c.api.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return -1, "", fmt.Errorf("create exec in %s: %w", containerID, err)
	}

	attached, err := c.api.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return -1, "", fmt.Errorf("attach to exec in %s: %w", containerID, err)
	}
	defer attached.Close()

	var out bytes.Buffer
	// Cap the capture: a hook that prints endlessly must not exhaust memory.
	limited := &limitedWriter{w: &out, remaining: 256 << 10}
	if _, err := stdcopy.StdCopy(limited, limited, attached.Reader); err != nil {
		return -1, out.String(), fmt.Errorf("read exec output from %s: %w", containerID, err)
	}

	inspect, err := c.api.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return -1, out.String(), fmt.Errorf("inspect exec in %s: %w", containerID, err)
	}
	return inspect.ExitCode, out.String(), nil
}
