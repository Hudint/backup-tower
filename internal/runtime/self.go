package runtime

import (
	"bufio"
	"context"
	"os"
	"regexp"
	"strings"
)

// containerIDPattern matches the 64-character hex ids that appear in a
// container's own mount table.
var containerIDPattern = regexp.MustCompile(`\b[0-9a-f]{64}\b`)

// SelfImage returns the image backup-tower is itself running from, empty when
// it is not running in a container this engine knows about.
//
// This exists to remove the single worst first-run surprise. Reaching another
// container's volumes needs a helper container, and the only image guaranteed to
// contain the archiving code is this one — so requiring the operator to name it
// meant the very first snapshot failed on a fresh install, for a reason that has
// nothing to do with what they were trying to do.
func (c *Client) SelfImage(ctx context.Context) string {
	for _, id := range selfCandidates() {
		self, err := c.Inspect(ctx, id)
		if err != nil {
			continue
		}
		// Prefer the reference the container was created from. An image id
		// works too, but a name is far more legible in logs and in `plan`.
		if self.Image != "" {
			return self.Image
		}
		if self.ImageID != "" {
			return self.ImageID
		}
	}
	return ""
}

// selfCandidates lists the identifiers that might name this container.
func selfCandidates() []string {
	var out []string

	// The engine sets the hostname to the short container id unless the
	// operator overrode it, which makes it the cheapest thing to try.
	if host, err := os.Hostname(); err == nil && len(host) >= 12 {
		out = append(out, host)
	}

	// The mount table names the container id in the overlay paths, and survives
	// a custom hostname. It is also the only one of the two that still works
	// under cgroup v2, where /proc/self/cgroup no longer carries the id.
	if id := idFromMountinfo("/proc/self/mountinfo"); id != "" {
		out = append(out, id)
	}
	return out
}

func idFromMountinfo(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// Only the engine's own storage paths carry the id; matching anywhere
		// else would happily pick up a hex string from a bind-mounted volume.
		if !strings.Contains(line, "/docker/") && !strings.Contains(line, "/containers/") {
			continue
		}
		if m := containerIDPattern.FindString(line); m != "" {
			return m
		}
	}
	return ""
}
