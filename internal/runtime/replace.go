package runtime

import (
	"context"
	"fmt"
	"log/slog"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
)

// ReplaceOptions configures swapping one container for another.
type ReplaceOptions struct {
	// Name the replacement should carry, normally the original name.
	Name string
	// Image the replacement runs.
	Image string
	// ParkSuffix makes the temporary name of the outgoing container unique.
	ParkSuffix string
	// InheritedFrom is passed through to Recreate; see RecreateOptions.
	InheritedFrom *dockerspec.DockerOCIImageConfig
	Log           *slog.Logger
}

// Replace swaps a container for a new one built from a captured spec.
//
// The outgoing container is renamed out of the way rather than deleted, and only
// removed once its replacement exists. If creating the replacement fails, the
// original is put back under its own name. Being left with no container at all
// would be a worse outcome than the update or rollback that was being attempted.
func (c *Client) Replace(ctx context.Context, old *Container, in *container.InspectResponse, opts ReplaceOptions) (string, []string, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	var parked string
	if old != nil {
		parked = fmt.Sprintf("%s-backup-tower-%s", opts.Name, opts.ParkSuffix)
		log.Info("moving the current container aside", "container", opts.Name, "parked_as", parked)
		if err := c.Rename(ctx, old.ID, parked); err != nil {
			return "", nil, err
		}
	}

	newID, warnings, err := c.Recreate(ctx, in, RecreateOptions{
		Name:          opts.Name,
		Image:         opts.Image,
		InheritedFrom: opts.InheritedFrom,
		Log:           log,
	})
	if err != nil {
		if parked != "" {
			// Use a context detached from the caller's: a cancelled update must
			// still put the original back.
			if undo := c.Rename(context.WithoutCancel(ctx), old.ID, opts.Name); undo != nil {
				return "", warnings, fmt.Errorf("%w — and the original container could not be renamed back from %s: %v", err, parked, undo)
			}
			log.Info("restored the original container after a failed replacement", "container", opts.Name)
		}
		return "", warnings, err
	}

	if parked != "" {
		if err := c.Remove(context.WithoutCancel(ctx), old.ID, true); err != nil {
			warnings = append(warnings, fmt.Sprintf("the replaced container is still present as %s: %v", parked, err))
		}
	}
	return newID, warnings, nil
}
