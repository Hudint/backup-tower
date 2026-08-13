package update

import (
	"sort"
	"strings"

	"github.com/Hudint/backup-tower/internal/discover"
)

// Order arranges candidates so that within a compose project, a service is
// updated before the services that depend on it.
//
// Handling each container in isolation means a web front end can be replaced
// while the database it needs is still being restarted, which looks exactly like
// a failed update and triggers a rollback of something that was never broken.
// Ordering does not eliminate the race, but it stops us from causing it.
func Order(candidates []*discover.Candidate) []*discover.Candidate {
	// Group by compose project; containers outside any project have no declared
	// relationships to honour.
	byProject := map[string][]*discover.Candidate{}
	var loose []*discover.Candidate

	for _, c := range candidates {
		if p := c.Container.ComposeProject(); p != "" {
			byProject[p] = append(byProject[p], c)
			continue
		}
		loose = append(loose, c)
	}

	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sort.Strings(projects)

	out := make([]*discover.Candidate, 0, len(candidates))
	for _, p := range projects {
		out = append(out, orderProject(byProject[p])...)
	}
	return append(out, loose...)
}

// orderProject topologically sorts one compose project's containers.
func orderProject(candidates []*discover.Candidate) []*discover.Candidate {
	byService := map[string]*discover.Candidate{}
	for _, c := range candidates {
		if svc := c.Container.ComposeService(); svc != "" {
			byService[svc] = c
		}
	}

	// Depth-first with a visiting set, so a dependency cycle degrades to the
	// original order instead of hanging or dropping containers.
	var out []*discover.Candidate
	state := map[string]int{} // 0 unseen, 1 visiting, 2 done

	var visit func(c *discover.Candidate)
	visit = func(c *discover.Candidate) {
		svc := c.Container.ComposeService()
		if svc == "" {
			out = append(out, c)
			return
		}
		switch state[svc] {
		case 2:
			return
		case 1:
			// Part of a cycle; compose itself would refuse this, but a stale
			// label can still produce one.
			return
		}
		state[svc] = 1
		for _, dep := range dependsOn(c) {
			if d, ok := byService[dep]; ok && d != c {
				visit(d)
			}
		}
		state[svc] = 2
		out = append(out, c)
	}

	// Sort first so the result is deterministic regardless of map iteration.
	sorted := append([]*discover.Candidate(nil), candidates...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Container.Name < sorted[j].Container.Name
	})
	for _, c := range sorted {
		visit(c)
	}
	return out
}

// dependsOn reads the service names a container declares a dependency on.
// Compose writes them as "service:condition:required", comma separated.
func dependsOn(c *discover.Candidate) []string {
	raw := c.Container.Labels["com.docker.compose.depends_on"]
	if raw == "" {
		return nil
	}
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, _, _ := strings.Cut(entry, ":")
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}
