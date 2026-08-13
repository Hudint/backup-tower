package cli

import (
	"context"
	"fmt"

	"github.com/Hudint/backup-tower/internal/discover"
	"github.com/Hudint/backup-tower/internal/runtime"
)

// buildRegistryAuth collects credentials from every source available.
//
// Images belonging to Komodo-managed stacks are pulled by Komodo's periphery
// agent, which logs in inside its own container — none of that reaches this
// host's docker configuration, so those images look unreachable from here.
// Asking Komodo closes that gap without moving the credentials anywhere.
//
// Both sources are kept and tried in turn rather than ranked. Credentials are
// per-registry, not per-container, so any precedence rule would be a guess.
func buildRegistryAuth(ctx context.Context, e *env) (*runtime.RegistryAuth, []string, error) {
	auth, err := runtime.LoadRegistryAuth("")
	if err != nil {
		return nil, nil, err
	}

	var notes []string
	if !e.cfg.Komodo.Configured() {
		return auth, notes, nil
	}

	client := discover.NewKomodoClient(discover.KomodoConfig{
		URL:       e.cfg.Komodo.URL,
		APIKey:    e.cfg.Komodo.APIKey,
		APISecret: e.cfg.Komodo.APISecret,
		Tag:       e.cfg.Komodo.Tag,
		Server:    e.cfg.Komodo.Server,
	})

	accounts, err := client.RegistryAccounts(ctx)
	if err != nil {
		// Not fatal: the host may well have the credentials anyway, and losing
		// the ability to check every other registry over this would be worse.
		notes = append(notes, fmt.Sprintf("komodo: registry accounts could not be read (%v); falling back to this host's docker login", err))
		return auth, notes, nil
	}

	var usable, redacted int
	for _, a := range accounts {
		auth.AddCredentials("komodo", a.Domain, a.Username, a.Token)
		if a.Token == "" && a.Username == "" {
			redacted++
			continue
		}
		usable++
	}
	switch {
	case len(accounts) == 0:
		notes = append(notes, "komodo: no registry accounts configured")
	case redacted > 0:
		notes = append(notes, fmt.Sprintf(
			"komodo: %d of %d registry accounts came back without a token — the API redacts them, so those registries need a docker login on this host",
			redacted, len(accounts)))
	default:
		notes = append(notes, fmt.Sprintf("komodo: %d registry accounts available", usable))
	}
	return auth, notes, nil
}
