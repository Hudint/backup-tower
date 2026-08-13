// Package store persists snapshots.
//
// The interface deliberately deals in named byte streams rather than manifests
// or archives. Keeping it that dumb is what lets a remote backend — object
// storage, SFTP, a deduplicating repository — slot in later without the
// snapshot logic knowing the difference.
package store

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Store persists and retrieves snapshot data.
type Store interface {
	// Begin starts writing a new snapshot. The returned transaction must be
	// either committed or aborted; until it is committed the snapshot must not
	// be visible to Snapshots, so an interrupted run cannot leave behind
	// something that looks restorable but is not.
	Begin(ctx context.Context, container, id string) (Tx, error)

	// Containers lists the containers that have at least one snapshot.
	Containers(ctx context.Context) ([]string, error)

	// Snapshots lists the snapshot IDs of a container in chronological order.
	Snapshots(ctx context.Context, container string) ([]string, error)

	// Open reads one file from within a snapshot, e.g. "manifest.json" or
	// "volumes/pgdata.tar.zst".
	Open(ctx context.Context, container, id, name string) (io.ReadCloser, error)

	// Remove deletes a snapshot and everything in it.
	Remove(ctx context.Context, container, id string) error

	// Location renders a human-readable address, for logs and messages.
	Location(container, id string) string

	// Close releases any resources held by the backend.
	Close() error
}

// Tx is an in-progress snapshot.
type Tx interface {
	// Create returns a writer for a file within the snapshot. The name uses
	// forward slashes and may contain one directory level, e.g. "volumes/x.tar.zst".
	Create(ctx context.Context, name string) (io.WriteCloser, error)

	// Commit publishes the snapshot.
	Commit(ctx context.Context) error

	// Abort discards everything written so far. It is safe to call after
	// Commit, where it does nothing.
	Abort() error
}

// Open resolves a destination URI to a store. A bare path is treated as a local
// directory, so the common case stays convenient.
func Open(uri string) (Store, error) {
	if uri == "" {
		return nil, fmt.Errorf("no backup destination configured")
	}

	// Anything without a scheme is a plain path.
	if !strings.Contains(uri, "://") {
		return NewFS(uri)
	}

	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("parse backup destination %q: %w", uri, err)
	}
	switch u.Scheme {
	case "file":
		path := u.Path
		if u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf("backup destination %q: file:// with a host is not supported", uri)
		}
		return NewFS(path)
	default:
		return nil, fmt.Errorf("backup destination %q: scheme %q is not supported by this build", uri, u.Scheme)
	}
}
