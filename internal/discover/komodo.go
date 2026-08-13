package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Komodo is used purely as an additional selection source: it answers "which
// stacks did the operator tag for automatic updates", and nothing else. Updates
// themselves still go through the normal path.
//
// That keeps the coupling to a single read call. Driving Komodo's own redeploy
// would tie backup-tower to its API version and its idea of when a deployment is
// finished, for no gain over doing the update directly.
type KomodoConfig struct {
	URL       string
	APIKey    string
	APISecret string
	// Tag selects which stacks and deployments count as opted in.
	Tag string
	// Server restricts results to one Komodo server. Komodo manages several
	// hosts, and backup-tower only ever sees the containers on its own — acting
	// on a stack that lives elsewhere would do nothing at best.
	Server  string
	Timeout time.Duration
}

// Configured reports whether enough was supplied to talk to Komodo.
func (c KomodoConfig) Configured() bool {
	return c.URL != "" && c.APIKey != "" && c.APISecret != "" && c.Tag != ""
}

// KomodoResource is a tagged stack or deployment.
type KomodoResource struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
	Tags []string
	Info struct {
		ServerID   string `json:"server_id"`
		ServerName string `json:"server_name"`
		// CustomName is the container name of a deployment when it differs
		// from the deployment's own name.
		CustomName string `json:"custom_name"`
	} `json:"info"`
}

// KomodoClient talks to a Komodo Core instance.
type KomodoClient struct {
	cfg  KomodoConfig
	http *http.Client
}

// NewKomodoClient builds a client.
func NewKomodoClient(cfg KomodoConfig) *KomodoClient {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	return &KomodoClient{
		cfg:  cfg,
		http: &http.Client{Timeout: timeout},
	}
}

// KomodoSelection is what Komodo contributed.
type KomodoSelection struct {
	// Projects holds compose project names from tagged stacks.
	Projects map[string]string
	// Containers holds container names from tagged deployments.
	Containers map[string]string
	Warnings   []string
}

// Tagged fetches the stacks and deployments carrying the configured tag.
func (c *KomodoClient) Tagged(ctx context.Context) (*KomodoSelection, error) {
	sel := &KomodoSelection{
		Projects:   map[string]string{},
		Containers: map[string]string{},
	}

	stacks, err := c.list(ctx, "ListStacks")
	if err != nil {
		return nil, err
	}
	deployments, err := c.list(ctx, "ListDeployments")
	if err != nil {
		return nil, err
	}

	servers := map[string]bool{}
	for _, r := range append(stacks, deployments...) {
		if r.Info.ServerName != "" {
			servers[r.Info.ServerName] = true
		}
	}

	for _, s := range stacks {
		if !c.onThisServer(s) {
			continue
		}
		// A Komodo stack is an ordinary compose project on the host, and its
		// name is the compose project name.
		sel.Projects[s.Name] = s.Name
	}
	for _, d := range deployments {
		if !c.onThisServer(d) {
			continue
		}
		name := d.Info.CustomName
		if name == "" {
			name = d.Name
		}
		sel.Containers[name] = d.Name
	}

	// Warn rather than guess. Without a server filter, a stack of the same name
	// on another host would be treated as if it were ours.
	if c.cfg.Server == "" && len(servers) > 1 {
		names := make([]string, 0, len(servers))
		for n := range servers {
			names = append(names, n)
		}
		sort.Strings(names)
		sel.Warnings = append(sel.Warnings, fmt.Sprintf(
			"tag %q spans several Komodo servers (%s); set KOMODO_SERVER so only this host's resources are selected",
			c.cfg.Tag, strings.Join(names, ", ")))
	}

	if len(sel.Projects) == 0 && len(sel.Containers) == 0 {
		sel.Warnings = append(sel.Warnings, fmt.Sprintf("no Komodo stacks or deployments carry the tag %q", c.cfg.Tag))
	}
	return sel, nil
}

func (c *KomodoClient) onThisServer(r KomodoResource) bool {
	if c.cfg.Server == "" {
		return true
	}
	return r.Info.ServerName == c.cfg.Server || r.Info.ServerID == c.cfg.Server
}

// list issues one read request. Komodo puts the request type in the path and
// takes the parameters as the body.
func (c *KomodoClient) list(ctx context.Context, requestType string) ([]KomodoResource, error) {
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{
			"tags":         []string{c.cfg.Tag},
			"tag_behavior": "Any",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", requestType, err)
	}

	endpoint := strings.TrimRight(c.cfg.URL, "/") + "/read/" + requestType
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", requestType, err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", c.cfg.APIKey)
	req.Header.Set("x-api-secret", c.cfg.APISecret)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call Komodo %s at %s: %w", requestType, endpoint, err)
	}
	defer resp.Body.Close()

	// Cap the response: a misconfigured URL pointing at something else entirely
	// should fail, not stream into memory.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read Komodo %s response: %w", requestType, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Komodo %s returned %s: %s",
			requestType, resp.Status, strings.TrimSpace(truncate(string(payload), 300)))
	}

	var out []KomodoResource
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decode Komodo %s response: %w", requestType, err)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
