package runtime

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// KeepRepo is the repository under which superseded images are pinned.
//
// After an update the previous image loses its tag, which makes it fair game
// for `docker image prune`. Pinning it here keeps it alive for exactly as long
// as the snapshot that depends on it. Without this the rollback path works right
// up until someone tidies up, and then fails at the worst possible moment.
const KeepRepo = "backup-tower/keep"

var unsafeTagChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// KeepTag builds the pin reference for a container's snapshot.
func KeepTag(container, snapshotID string) string {
	return KeepRepo + ":" + sanitizeTag(container) + "-" + sanitizeTag(snapshotID)
}

func sanitizeTag(s string) string {
	s = unsafeTagChars.ReplaceAllString(s, "_")
	s = strings.TrimLeft(s, ".-")
	if s == "" {
		s = "unnamed"
	}
	// Docker tags are limited to 128 characters; leave room for the other half.
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// PinImage tags an image so it survives pruning.
func (c *Client) PinImage(ctx context.Context, imageID, tag string) error {
	if _, err := c.api.ImageTag(ctx, client.ImageTagOptions{Source: imageID, Target: tag}); err != nil {
		return fmt.Errorf("pin image %s as %s: %w", imageID, tag, err)
	}
	return nil
}

// UnpinImage removes a pin. The image itself is only deleted if nothing else
// references it, which is exactly the intent: release it, do not force it away.
func (c *Client) UnpinImage(ctx context.Context, tag string) error {
	_, err := c.api.ImageRemove(ctx, tag, client.ImageRemoveOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil // already gone
		}
		return fmt.Errorf("release image pin %s: %w", tag, err)
	}
	return nil
}

// ListPins returns every image tag backup-tower is holding on to.
func (c *Client) ListPins(ctx context.Context) ([]string, error) {
	filters := client.Filters{}
	filters.Add("reference", KeepRepo+":*")

	res, err := c.api.ImageList(ctx, client.ImageListOptions{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("list pinned images: %w", err)
	}
	var out []string
	for _, img := range res.Items {
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(tag, KeepRepo+":") {
				out = append(out, tag)
			}
		}
	}
	return out, nil
}

// HasImage reports whether a reference resolves locally.
func (c *Client) HasImage(ctx context.Context, ref string) bool {
	_, err := c.api.ImageInspect(ctx, ref)
	return err == nil
}

// ResolveImage picks a usable image reference for recreating a container.
//
// The exact image ID is preferred because it is what the container actually ran.
// A pin is checked next, then any registry digest recorded at snapshot time. If
// none of them resolve, the caller is told plainly rather than being handed a
// container built on some other version of the image.
func (c *Client) ResolveImage(ctx context.Context, candidates ...string) (string, error) {
	var tried []string
	for _, ref := range candidates {
		if ref == "" {
			continue
		}
		tried = append(tried, ref)
		if c.HasImage(ctx, ref) {
			return ref, nil
		}
	}
	if len(tried) == 0 {
		return "", fmt.Errorf("no image reference was recorded for this snapshot")
	}
	return "", fmt.Errorf("none of the recorded images are available locally (tried %s); pull one of them first",
		strings.Join(tried, ", "))
}
