package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/hudint/backup-tower/internal/snapshot/store"
)

// LoadManifest reads and parses the manifest of one snapshot.
func LoadManifest(ctx context.Context, st store.Store, container, id string) (*Manifest, error) {
	r, err := st.Open(ctx, container, id, ManifestFile)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read manifest of %s/%s: %w", container, id, err)
	}
	return DecodeManifest(b)
}

// LoadSpec reads and parses the captured container spec of one snapshot.
func LoadSpec(ctx context.Context, st store.Store, container, id string) (*Spec, error) {
	r, err := st.Open(ctx, container, id, SpecFile)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read spec of %s/%s: %w", container, id, err)
	}
	return DecodeSpec(b)
}

// Latest returns the most recent snapshot ID of a container.
func Latest(ctx context.Context, st store.Store, container string) (string, error) {
	ids, err := st.Snapshots(ctx, container)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("no snapshots for container %q", container)
	}
	return ids[len(ids)-1], nil
}

// Resolve turns an optional snapshot ID into a concrete one, defaulting to the
// most recent.
func Resolve(ctx context.Context, st store.Store, container, id string) (string, error) {
	if id != "" {
		return id, nil
	}
	return Latest(ctx, st, container)
}

// VerifyResult reports the outcome for one archive.
type VerifyResult struct {
	Archive Archive
	// Got is the checksum computed now.
	Got string
	// OK is true when the stored archive still matches its recorded checksum.
	OK bool
	// Err is set when the archive could not be read at all.
	Err error
}

// Verify recomputes the checksum of every archive in a snapshot.
//
// A backup that is never read is only a hypothesis. This turns it into a
// statement that can be checked before an incident rather than during one.
func Verify(ctx context.Context, st store.Store, container, id string) (*Manifest, []VerifyResult, error) {
	m, err := LoadManifest(ctx, st, container, id)
	if err != nil {
		return nil, nil, err
	}

	results := make([]VerifyResult, 0, len(m.Archives))
	for _, a := range m.Archives {
		res := VerifyResult{Archive: a}
		sum, err := checksum(ctx, st, container, id, a.Path)
		switch {
		case err != nil:
			res.Err = err
		default:
			res.Got = sum
			res.OK = sum == a.SHA256
		}
		results = append(results, res)
	}
	return m, results, nil
}

func checksum(ctx context.Context, st store.Store, container, id, path string) (string, error) {
	r, err := st.Open(ctx, container, id, path)
	if err != nil {
		return "", err
	}
	defer r.Close()

	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
