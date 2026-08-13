package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// Image is the identity of a local image. The distinction between ID and
// RepoDigests matters for rollback: the ID addresses the local content, while a
// repo digest is what a registry can hand back if the local copy is gone.
type Image struct {
	ID          string   `json:"id"`
	RepoTags    []string `json:"repo_tags,omitempty"`
	RepoDigests []string `json:"repo_digests,omitempty"`
}

// PrimaryDigest returns the first repo digest, empty when the image was built
// locally and never pushed. An empty value is the signal that this image cannot
// be re-fetched from a registry — and therefore cannot be updated either.
func (i *Image) PrimaryDigest() string {
	if len(i.RepoDigests) == 0 {
		return ""
	}
	return i.RepoDigests[0]
}

// FromRegistry reports whether the image can be resolved against a registry.
// Locally built images without a repo digest cannot, so update checks must skip
// them instead of failing on every run.
func (i *Image) FromRegistry() bool {
	return len(i.RepoDigests) > 0
}

// InspectImage looks up a local image by reference or ID.
func (c *Client) InspectImage(ctx context.Context, ref string) (*Image, error) {
	res, err := c.api.ImageInspect(ctx, ref)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("image %q: %w", ref, ErrNotFound)
		}
		return nil, fmt.Errorf("inspect image %q: %w", ref, err)
	}
	return &Image{
		ID:          res.ID,
		RepoTags:    res.RepoTags,
		RepoDigests: res.RepoDigests,
	}, nil
}

// PullImage fetches an image reference, waiting for the transfer to finish.
//
// The engine streams progress as JSON and only reports failures inside that
// stream, so the body has to be read to the end — returning early would treat a
// failed pull as a successful one.
func (c *Client) PullImage(ctx context.Context, ref, encodedAuth string) error {
	resp, err := c.api.ImagePull(ctx, ref, client.ImagePullOptions{RegistryAuth: encodedAuth})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer resp.Close()

	dec := json.NewDecoder(resp)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read progress while pulling %s: %w", ref, err)
		}
		if msg.Error != "" {
			return fmt.Errorf("pull %s: %s", ref, msg.Error)
		}
	}
}

// ImageReference is the reference a container was created from, normalised so
// it is comparable. An empty tag is expanded to :latest, matching how the
// engine resolves it.
func ImageReference(ref string) string {
	if ref == "" {
		return ""
	}
	// A digest reference is already exact.
	if strings.Contains(ref, "@") {
		return ref
	}
	// Distinguish a tag from a registry port: the last colon must come after
	// the last slash to be a tag.
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref
	}
	return ref + ":latest"
}
