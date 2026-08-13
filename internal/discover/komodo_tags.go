package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// TagRule maps one Komodo tag to the settings a resource carrying it receives.
//
// A tag is only a name, so on its own it can express "this one is included" and
// nothing more. Mapping tags to settings is what lets the whole policy live in
// Komodo: tag a stack in the UI and it is configured, with no compose file to
// edit and no redeploy to schedule.
type TagRule struct {
	Tag string   `yaml:"tag"`
	Set Settings `yaml:"set"`
}

// tagSummary is one entry of Komodo's tag list.
type tagSummary struct {
	ID   any    `json:"_id"`
	Name string `json:"name"`
}

// ListTags returns Komodo's tags keyed by their id, since resources reference
// tags by id rather than by name.
func (c *KomodoClient) ListTags(ctx context.Context) (map[string]string, error) {
	body, err := c.post(ctx, "ListTags", map[string]any{})
	if err != nil {
		return nil, err
	}
	var tags []tagSummary
	if err := json.Unmarshal(body, &tags); err != nil {
		return nil, fmt.Errorf("decode Komodo tags: %w", err)
	}

	out := make(map[string]string, len(tags))
	for _, t := range tags {
		if id := mongoID(t.ID); id != "" {
			out[id] = t.Name
		}
	}
	return out, nil
}

// mongoID unwraps the {"$oid": "..."} form Komodo serialises ids in, and
// tolerates a plain string in case that ever changes.
func mongoID(v any) string {
	switch id := v.(type) {
	case string:
		return id
	case map[string]any:
		if s, ok := id["$oid"].(string); ok {
			return s
		}
	}
	return ""
}

// TaggedResources fetches every stack and deployment together with the tag
// names it carries.
//
// Everything is fetched once rather than queried per tag: the number of tags in
// a policy is unbounded, the number of resources is not, and one pass keeps the
// view internally consistent.
func (c *KomodoClient) TaggedResources(ctx context.Context) (*KomodoSelection, error) {
	names, err := c.ListTags(ctx)
	if err != nil {
		return nil, err
	}

	sel := &KomodoSelection{
		Projects:   map[string][]string{},
		Containers: map[string][]string{},
	}

	stacks, err := c.listAll(ctx, "ListStacks")
	if err != nil {
		return nil, err
	}
	deployments, err := c.listAll(ctx, "ListDeployments")
	if err != nil {
		return nil, err
	}

	servers := map[string]bool{}
	for _, r := range append(append([]KomodoResource{}, stacks...), deployments...) {
		if len(r.Tags) > 0 && r.Info.ServerName != "" {
			servers[r.Info.ServerName] = true
		}
	}

	for _, s := range stacks {
		if !c.onThisServer(s) || len(s.Tags) == 0 {
			continue
		}
		// A Komodo stack is an ordinary compose project on the host, and its
		// name is the compose project name.
		sel.Projects[s.Name] = tagNames(s.Tags, names)
	}
	for _, d := range deployments {
		if !c.onThisServer(d) || len(d.Tags) == 0 {
			continue
		}
		name := d.Info.CustomName
		if name == "" {
			name = d.Name
		}
		sel.Containers[name] = tagNames(d.Tags, names)
	}

	if c.cfg.Server == "" && len(servers) > 1 {
		list := make([]string, 0, len(servers))
		for n := range servers {
			list = append(list, n)
		}
		sort.Strings(list)
		sel.Warnings = append(sel.Warnings, fmt.Sprintf(
			"tagged resources exist on several Komodo servers (%v); set KOMODO_SERVER so only this host's are considered", list))
	}
	return sel, nil
}

func tagNames(ids []string, names map[string]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := names[id]; ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// listAll fetches resources without filtering by tag.
func (c *KomodoClient) listAll(ctx context.Context, requestType string) ([]KomodoResource, error) {
	body, err := c.post(ctx, requestType, map[string]any{})
	if err != nil {
		return nil, err
	}
	var out []KomodoResource
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode Komodo %s response: %w", requestType, err)
	}
	return out, nil
}
