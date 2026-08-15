package cli

import (
	"context"
	"fmt"

	"github.com/Hudint/backup-tower/internal/komodo"
	"github.com/Hudint/backup-tower/internal/runtime"
)

// buildRegistryAuth collects credentials from every source available.
//
// The host's own docker configuration is read immediately: it is a local file.
// Komodo is registered but not called. Images belonging to Komodo-managed stacks
// are pulled by its periphery agent, which logs in inside its own container, so
// none of those credentials reach this host — but that only matters for the
// images it actually holds credentials for. Asking on every start, for every
// command, on hosts that may only ever pull public images, is a network call for
// nothing. It is made when a registry has refused everything else, and not before.
func buildRegistryAuth(_ context.Context, e *env) (*runtime.RegistryAuth, []string, error) {
	auth, err := runtime.LoadRegistryAuth("")
	if err != nil {
		return nil, nil, err
	}

	var notes []string
	switch {
	case e.cfg.Komodo.Configured():
		auth.SetFallback("komodo", func(ctx context.Context) ([]runtime.Credential, string, error) {
			return komodoCredentials(ctx, e)
		})
		notes = append(notes, "komodo: configured, consulted only if a registry refuses this host's credentials")
	case e.cfg.Komodo.Partial():
		notes = append(notes, fmt.Sprintf("komodo: not configured, still missing %s",
			joinEnv(e.cfg.Komodo.Missing())))
	}
	return auth, notes, nil
}

// komodoCredentials fetches Komodo's registry accounts. It returns a note
// describing what came back, because a redacted account and a missing one fail
// in exactly the same way at the registry and need different fixes.
func komodoCredentials(ctx context.Context, e *env) ([]runtime.Credential, string, error) {
	client := komodo.New(komodo.Config{
		URL:       e.cfg.Komodo.URL,
		APIKey:    e.cfg.Komodo.APIKey,
		APISecret: e.cfg.Komodo.APISecret,
	})

	accounts, err := client.RegistryAccounts(ctx)
	if err != nil {
		// Not fatal: the registry refusal the caller already has is the more
		// useful thing to report.
		return nil, fmt.Sprintf("komodo: registry accounts could not be read (%v)", err), err
	}

	creds := make([]runtime.Credential, 0, len(accounts))
	var redacted int
	for _, a := range accounts {
		creds = append(creds, runtime.Credential{Domain: a.Domain, Username: a.Username, Token: a.Token})
		if a.Token == "" && a.Username == "" {
			redacted++
		}
	}

	var note string
	switch {
	case len(accounts) == 0:
		note = "komodo: no registry accounts configured"
	case redacted > 0:
		note = fmt.Sprintf(
			"komodo: %d of %d registry accounts came back without a token — the API redacts them, so those registries need a docker login on this host",
			redacted, len(accounts))
	default:
		note = fmt.Sprintf("komodo: %d registry accounts available", len(accounts))
	}
	return creds, note, nil
}

func joinEnv(missing []string) string {
	out := ""
	for i, m := range missing {
		if i > 0 {
			out += ", "
		}
		out += m
	}
	return out
}
