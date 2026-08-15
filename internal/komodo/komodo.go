// Package komodo reads registry credentials from a Komodo Core instance.
//
// That is the whole of the relationship. Komodo was once a selection source as
// well — it could say which stacks had been tagged for automatic updates — and
// that is deliberately gone: it made an external service a prerequisite for
// deciding anything, so an unreachable Komodo meant no backups at all, even for
// containers configured entirely by label. Selection now lives on the container
// and in the rule file, where it can be read without a network.
//
// What remains is a gap only Komodo can close. Images belonging to Komodo
// managed stacks are pulled by its periphery agent, which logs in inside its own
// container; none of that reaches this host's docker configuration, so a private
// image simply looks unreachable from here. Asking Komodo is the only way to see
// it, and it is asked only once a registry has actually refused us.
package komodo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config is what is needed to talk to a Komodo Core instance.
type Config struct {
	URL       string
	APIKey    string
	APISecret string
	Timeout   time.Duration
}

// Configured reports whether enough was supplied to talk to Komodo.
func (c Config) Configured() bool {
	return c.URL != "" && c.APIKey != "" && c.APISecret != ""
}

// Client talks to a Komodo Core instance.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client.
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: timeout}}
}

// RegistryAccount is a set of registry credentials Komodo holds.
type RegistryAccount struct {
	Domain   string `json:"domain"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

// RegistryAccounts fetches the registry credentials Komodo manages.
func (c *Client) RegistryAccounts(ctx context.Context) ([]RegistryAccount, error) {
	// Komodo renamed this request in v2.3.0. Both names are tried rather than
	// pinning a version, because the alternative is a tool that silently stops
	// finding credentials the day someone upgrades — or, as here, one that never
	// found them because the instance was older than the docs.
	body, err := c.post(ctx, "ListImageRegistryAccounts", map[string]any{})
	if err != nil && isUnknownRequest(err) {
		body, err = c.post(ctx, "ListDockerRegistryAccounts", map[string]any{})
	}
	if err != nil {
		return nil, err
	}
	var out []RegistryAccount
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode Komodo registry accounts: %w", err)
	}
	return out, nil
}

// isUnknownRequest recognises Komodo rejecting a request name it does not know,
// which is how a version difference shows up.
func isUnknownRequest(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown variant")
}

// Version reports the Komodo version, for diagnostics.
func (c *Client) Version(ctx context.Context) (string, error) {
	body, err := c.post(ctx, "GetVersion", map[string]any{})
	if err != nil {
		return "", err
	}
	var out struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode Komodo version: %w", err)
	}
	return out.Version, nil
}

// post performs one read request and returns the raw response body.
func (c *Client) post(ctx context.Context, requestType string, params map[string]any) ([]byte, error) {
	body, err := json.Marshal(params)
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
	return payload, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
