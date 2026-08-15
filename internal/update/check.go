// Package update applies container updates with a tested way back.
package update

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Hudint/backup-tower/internal/discover"
	"github.com/Hudint/backup-tower/internal/runtime"
)

// CheckState says what a registry lookup concluded.
type CheckState string

const (
	// StateUpToDate means the registry has the same image the container runs.
	StateUpToDate CheckState = "up-to-date"
	// StateUpdateAvailable means the registry has a different image.
	StateUpdateAvailable CheckState = "update-available"
	// StateSkipped means no lookup was possible or wanted.
	StateSkipped CheckState = "skipped"
	// StateFailed means the lookup itself went wrong.
	StateFailed CheckState = "failed"
)

// Check is the result of asking whether a container is behind its registry.
type Check struct {
	Container string
	Reference string
	// LocalDigest is the manifest digest the running image was pulled at.
	LocalDigest string
	// RemoteDigest is what the registry serves for the same reference now.
	RemoteDigest string
	State        CheckState
	// Reason explains a skip or a failure.
	Reason string
	Err    error
}

// Available reports whether an update should be applied.
func (c *Check) Available() bool { return c.State == StateUpdateAvailable }

// Checker asks registries what they currently serve.
type Checker struct {
	rt   *runtime.Client
	auth *runtime.RegistryAuth
}

// NewChecker builds a checker.
func NewChecker(rt *runtime.Client, auth *runtime.RegistryAuth) *Checker {
	return &Checker{rt: rt, auth: auth}
}

// Check compares the running image against its registry.
//
// The comparison is on manifest digests rather than tags: a moving tag like
// :latest says nothing about whether the content changed, and pulling every
// image just to find out is expensive enough that people turn updates off.
func (c *Checker) Check(ctx context.Context, cand *discover.Candidate) *Check {
	container := cand.Container
	out := &Check{Container: container.Name, Reference: runtime.ImageReference(container.Image)}

	if cand.Updatability != discover.Updatable {
		out.State = StateSkipped
		// Name the reason the check was skipped, not the first reason the
		// container is not being updated. Those are different questions, and
		// answering the wrong one sends people to change a setting that will
		// not help — "monitor only" reads as "turn that off and I will check",
		// when the truth is that the image has no registry to check against.
		out.Reason = cand.UpdatabilityReason()
		return out
	}

	img, err := c.rt.InspectImage(ctx, container.ImageID)
	if err != nil {
		out.State = StateFailed
		out.Reason = "the running image could not be inspected"
		out.Err = err
		return out
	}
	out.LocalDigest = runtime.DigestForRepo(out.Reference, img.RepoDigests)
	if out.LocalDigest == "" {
		out.State = StateSkipped
		out.Reason = "the running image carries no digest for this repository, so there is nothing to compare"
		return out
	}

	// A container pinned to a digest is pinned on purpose. Moving it would
	// undo a deliberate decision.
	if strings.Contains(container.Image, "@sha256:") {
		out.State = StateSkipped
		out.Reason = "pinned to a digest"
		return out
	}

	remote, err := c.remoteDigest(ctx, out.Reference)
	if err != nil {
		out.State = StateFailed
		out.Err = err
		switch {
		case isAuthError(err) && c.auth.RedactedSource(out.Reference) != "":
			out.Reason = fmt.Sprintf("%s knows this registry but returned no usable token; log in with docker login on this host instead",
				c.auth.RedactedSource(out.Reference))
		case isAuthError(err) && c.auth.HasHelperOnly(out.Reference):
			out.Reason = "the registry needs credentials that are stored in a credential helper this build cannot call"
		case isAuthError(err):
			out.Reason = "the registry needs credentials; log in with docker login, or let Komodo supply them"
		default:
			out.Reason = "the registry could not be reached"
		}
		return out
	}
	out.RemoteDigest = remote

	if remote == out.LocalDigest {
		out.State = StateUpToDate
		return out
	}
	out.State = StateUpdateAvailable
	return out
}

// remoteDigest asks the registry, trying each set of credentials in turn.
//
// Only once every credential already at hand has been refused is the lazy
// fallback source consulted — normally Komodo, which is a remote service. A host
// whose images are public or covered by its own docker login therefore never
// contacts it at all, and one that needs it contacts it once.
func (c *Checker) remoteDigest(ctx context.Context, ref string) (string, error) {
	digest, err := c.tryCredentials(ctx, ref)
	if err == nil || !isAuthError(err) {
		return digest, err
	}

	added, loadErr := c.auth.LoadFallback(ctx)
	if loadErr != nil || !added {
		// Nothing new to try. Report the registry's refusal rather than the
		// fallback's own trouble: the refusal is what the operator has to act on.
		return "", err
	}
	return c.tryCredentials(ctx, ref)
}

// tryCredentials walks the known credentials, stopping early on anything that is
// not an authentication problem — a registry that cannot be reached will not be
// reached with other credentials either. Only the last failure is worth reporting.
func (c *Checker) tryCredentials(ctx context.Context, ref string) (string, error) {
	var lastErr error
	for _, auth := range c.auth.Attempts(ref) {
		digest, err := c.rt.RemoteDigest(ctx, ref, auth)
		if err == nil {
			return digest, nil
		}
		lastErr = err
		if !isAuthError(err) {
			break
		}
	}
	return "", lastErr
}

// Auth exposes the resolved credentials so a pull can use the same ones.
func (c *Checker) Auth() *runtime.RegistryAuth { return c.auth }

// CheckAll asks about many containers at once, keyed by container name.
//
// Nearly all of a check is waiting: a manifest lookup is a TLS handshake, a
// token fetch and one request, and on this host that is around 0.6 seconds
// during which nothing is computed. Done one after another, twenty containers
// take twenty times that — long enough that it starts to dictate how short the
// daemon's interval can sensibly be, which is a strange thing to let latency
// decide. Waiting in parallel costs nothing but sockets.
//
// Checks are safe to overlap because they change nothing: they ask a registry a
// question. Applying an update is the opposite, and stays strictly sequential.
func (c *Checker) CheckAll(ctx context.Context, cands []*discover.Candidate, concurrency int) map[string]*Check {
	if concurrency < 1 {
		concurrency = 1
	}

	out := make(map[string]*Check, len(cands))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)

	for _, cand := range cands {
		wg.Add(1)
		go func(cand *discover.Candidate) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			res := c.Check(ctx, cand)
			mu.Lock()
			out[cand.Container.Name] = res
			mu.Unlock()
		}(cand)
	}
	wg.Wait()
	return out
}

// Describe renders a check result as one line.
func (c *Check) Describe() string {
	switch c.State {
	case StateUpToDate:
		return "up to date"
	case StateUpdateAvailable:
		return fmt.Sprintf("update available (%s → %s)", shortDigest(c.LocalDigest), shortDigest(c.RemoteDigest))
	case StateSkipped:
		return "skipped: " + c.Reason
	default:
		return "check failed: " + c.Reason
	}
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func isAuthError(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "authentication required") ||
		strings.Contains(s, "denied") ||
		strings.Contains(s, "forbidden")
}
