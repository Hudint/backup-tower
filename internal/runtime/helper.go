package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// HelperLabel marks the short-lived containers we start, so a leftover one can
// be recognised and cleaned up rather than puzzled over.
const HelperLabel = "de.hudint.backup-tower.helper"

// HelperSpec describes a one-shot container run. It exists so backup-tower can
// reach volumes it cannot mount into itself at runtime: a container cannot add
// mounts after it has started, so the only way in is a second container that is
// created with them.
type HelperSpec struct {
	// Image to run. Normally backup-tower's own image, which carries the
	// archiving code — that keeps the helper free of shell pipes and external
	// tar/zstd binaries.
	Image string
	Cmd   []string
	// Binds are docker-style mount specs, e.g. "pgdata:/src:ro".
	Binds []string
	// Labels are merged with the helper marker.
	Labels map[string]string
	// Purpose is a short description used in error messages.
	Purpose string
}

// RunAndStream creates the helper, streams its stdout into out, and waits for
// it to exit. The captured stderr is returned so the caller can read the
// helper's own report from it; on failure it is folded into the error, so a
// problem explains itself instead of surfacing as a bare exit code.
func (c *Client) RunAndStream(ctx context.Context, spec HelperSpec, out io.Writer) ([]byte, error) {
	labels := map[string]string{HelperLabel: "true"}
	for k, v := range spec.Labels {
		labels[k] = v
	}

	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:        spec.Image,
			Cmd:          spec.Cmd,
			Labels:       labels,
			AttachStdout: true,
			AttachStderr: true,
			// A TTY would give us a single unmultiplexed stream, but it also
			// puts the output through line-ending translation, which corrupts
			// binary archives. Multiplexed it is.
			Tty: false,
		},
		HostConfig: &container.HostConfig{
			Binds:       spec.Binds,
			AutoRemove:  false, // we remove it ourselves, after reading the exit code
			NetworkMode: "none",
			// The helper only ever reads mounted data and writes to stdout.
			ReadonlyRootfs: true,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create helper container for %s: %w", spec.Purpose, err)
	}
	defer func() {
		_, _ = c.api.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	// Register the wait before starting, so a fast helper cannot exit before we
	// are listening.
	waitRes := c.api.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNextExit,
	})

	attached, err := c.api.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("attach to helper container for %s: %w", spec.Purpose, err)
	}
	defer attached.Close()

	if _, err := c.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("start helper container for %s: %w", spec.Purpose, err)
	}

	var stderr bytes.Buffer
	copyErr := make(chan error, 1)
	go func() {
		// Bound the captured stderr: a helper stuck in a log loop must not be
		// able to exhaust our memory.
		_, err := stdcopy.StdCopy(out, &limitedWriter{w: &stderr, remaining: 64 << 10}, attached.Reader)
		copyErr <- err
	}()

	var runErr error
	select {
	case err := <-waitRes.Error:
		if err != nil {
			runErr = fmt.Errorf("wait for helper container for %s: %w", spec.Purpose, err)
		}
	case res := <-waitRes.Result:
		switch {
		case res.Error != nil && res.Error.Message != "":
			runErr = fmt.Errorf("helper container for %s failed: %s", spec.Purpose, res.Error.Message)
		case res.StatusCode != 0:
			runErr = fmt.Errorf("helper container for %s exited with status %d", spec.Purpose, res.StatusCode)
		}
	case <-ctx.Done():
		runErr = ctx.Err()
		// Unblock the reader, which would otherwise wait on a connection that
		// is no longer going anywhere.
		attached.Close()
	}

	// The copy goroutine owns the stderr buffer until it returns, so nothing may
	// read it before then. Draining also matters for the happy path: without it
	// the tail of the archive can be lost.
	copyResult := <-copyErr
	if runErr == nil && copyResult != nil && copyResult != io.EOF {
		runErr = fmt.Errorf("read output of helper container for %s: %w", spec.Purpose, copyResult)
	}
	if runErr != nil && stderr.Len() > 0 {
		runErr = fmt.Errorf("%w: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	return stderr.Bytes(), runErr
}

// limitedWriter keeps at most remaining bytes and silently drops the rest. It
// always reports a full write so the caller does not treat the cap as a short
// write and abort the stream.
type limitedWriter struct {
	w         io.Writer
	remaining int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}
	chunk := p
	if len(chunk) > l.remaining {
		chunk = chunk[:l.remaining]
	}
	n, err := l.w.Write(chunk)
	l.remaining -= n
	if err != nil {
		return n, err
	}
	return len(p), nil
}
