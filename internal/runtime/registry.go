package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
)

// dockerHubConfigKey is how Docker Hub credentials are keyed in config.json,
// which is not the same string as the registry domain.
const dockerHubConfigKey = "https://index.docker.io/v1/"

// RegistryAuth resolves credentials for image references.
//
// The engine does not use its own stored credentials for manifest lookups —
// they have to be passed in per request, the same way the docker CLI does it.
// Without this, checking a private image would report "unauthorised" rather
// than "up to date", on every single run.
type RegistryAuth struct {
	// byRegistry maps a config.json key to an encoded auth header value.
	byRegistry map[string]string
	// extra holds credentials from other sources, keyed by domain. Several may
	// exist for one domain.
	extra map[string][]namedAuth
	// helperRegistries are registries whose credentials live in an external
	// credential helper we cannot call.
	helperRegistries []string
	// emptyTokens records sources that named a registry but supplied no secret,
	// which is what a redacted API response looks like.
	emptyTokens map[string]string

	// fallback is consulted only after a registry has refused everything
	// already known. See SetFallback.
	fallback     *fallbackSource
	fallbackOnce sync.Once

	// mu guards everything the fallback can write after this value is shared.
	// Checks run concurrently, so one goroutine may be loading credentials
	// while another is reading them; byRegistry is not covered because it is
	// filled before the value is handed out and never written again.
	mu           sync.RWMutex
	fallbackErr  error
	fallbackNote string
}

// Credential is one set of registry credentials from a fallback source.
type Credential struct {
	Domain   string
	Username string
	Token    string
}

type fallbackSource struct {
	name string
	load func(ctx context.Context) ([]Credential, string, error)
}

// SetFallback registers a source of credentials that is consulted lazily.
//
// The point of the laziness is that the source is remote. Fetching credentials
// from it up front means contacting it on every daemon start and on every run of
// every command, for hosts that may never pull a private image at all. Deferring
// it until a registry has actually refused us means it is contacted when — and
// only when — it is the one thing that can help.
func (a *RegistryAuth) SetFallback(name string, load func(ctx context.Context) ([]Credential, string, error)) {
	a.fallback = &fallbackSource{name: name, load: load}
}

// LoadFallback consults the fallback source, at most once for the lifetime of
// this RegistryAuth. It reports whether any new credentials became available.
//
// Failure is deliberately not retried: a source that is down stays down for the
// length of a run, and hammering it once per image check would turn one outage
// into a much slower one.
func (a *RegistryAuth) LoadFallback(ctx context.Context) (bool, error) {
	if a == nil || a.fallback == nil {
		return false, nil
	}
	var added bool
	a.fallbackOnce.Do(func() {
		creds, note, err := a.fallback.load(ctx)

		a.mu.Lock()
		a.fallbackNote = note
		a.fallbackErr = err
		a.mu.Unlock()

		if err != nil {
			return
		}
		for _, c := range creds {
			a.AddCredentials(a.fallback.name, c.Domain, c.Username, c.Token)
			added = true
		}
	})

	a.mu.RLock()
	defer a.mu.RUnlock()
	return added, a.fallbackErr
}

// FallbackName reports the name of the lazy source, empty when there is none.
func (a *RegistryAuth) FallbackName() string {
	if a == nil || a.fallback == nil {
		return ""
	}
	return a.fallback.name
}

// FallbackNote reports whatever the fallback source said about itself when it
// was consulted, empty when it never was.
func (a *RegistryAuth) FallbackNote() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.fallbackNote
}

type namedAuth struct {
	source  string
	encoded string
}

// AddCredentials registers credentials from a source other than the docker
// configuration, such as Komodo's registry accounts.
//
// Nothing here tries to decide which source outranks which. Credentials are
// per-registry, not per-container, so any ordering would be a guess; instead
// every candidate is tried in turn, which is both cheaper and more honest than
// being wrong about precedence.
func (a *RegistryAuth) AddCredentials(source, domain, username, token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.extra == nil {
		a.extra = map[string][]namedAuth{}
	}
	if a.emptyTokens == nil {
		a.emptyTokens = map[string]string{}
	}
	domain = normaliseDomain(domain)
	if domain == "" {
		return
	}
	if token == "" && username == "" {
		// A named account with nothing in it is almost always a redacted API
		// response, and saying so beats reporting a plain authentication failure.
		a.emptyTokens[domain] = source
		return
	}
	encoded, err := encodeAuth(registry.AuthConfig{
		Username:      username,
		Password:      token,
		ServerAddress: domain,
	})
	if err != nil {
		return
	}
	a.extra[domain] = append(a.extra[domain], namedAuth{source: source, encoded: encoded})
}

// RedactedSource names the source that knows about a reference's registry but
// handed over no usable secret, empty when there is none.
func (a *RegistryAuth) RedactedSource(ref string) string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.emptyTokens[domainOf(ref)]
}

// normaliseDomain reduces the many ways Docker Hub is written to the one form
// image references actually use. Credentials for it are conventionally stored
// under a URL, which matches no image reference at all.
func normaliseDomain(domain string) string {
	domain = strings.TrimPrefix(strings.TrimPrefix(domain, "https://"), "http://")
	domain = strings.Trim(domain, "/")
	switch domain {
	case "index.docker.io/v1", "index.docker.io", "registry-1.docker.io", "docker.io":
		return "docker.io"
	}
	return domain
}

func domainOf(ref string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ""
	}
	return reference.Domain(named)
}

// Attempts returns the credentials to try for a reference, in order, ending with
// the anonymous attempt so a public image still works.
func (a *RegistryAuth) Attempts(ref string) []string {
	if a == nil {
		return []string{""}
	}
	var out []string
	if host := a.For(ref); host != "" {
		out = append(out, host)
	}
	a.mu.RLock()
	for _, extra := range a.extra[domainOf(ref)] {
		out = append(out, extra.encoded)
	}
	a.mu.RUnlock()
	return append(out, "")
}

// LoadRegistryAuth reads the docker CLI configuration. A missing file is not an
// error: public images need no credentials.
func LoadRegistryAuth(path string) (*RegistryAuth, error) {
	if path == "" {
		if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
			path = filepath.Join(dir, "config.json")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return &RegistryAuth{}, nil
			}
			path = filepath.Join(home, ".docker", "config.json")
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &RegistryAuth{}, nil
		}
		return nil, fmt.Errorf("read docker config %q: %w", path, err)
	}

	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse docker config %q: %w", path, err)
	}

	a := &RegistryAuth{byRegistry: map[string]string{}}
	for host, entry := range cfg.Auths {
		user, pass := entry.Username, entry.Password
		if entry.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				continue
			}
			if name, secret, ok := strings.Cut(string(decoded), ":"); ok {
				user, pass = name, secret
			}
		}
		if user == "" && pass == "" {
			// Credentials live in a helper; record it so the reason for a
			// failure can be named rather than guessed at.
			if cfg.CredsStore != "" || cfg.CredHelpers[host] != "" {
				a.helperRegistries = append(a.helperRegistries, host)
			}
			continue
		}
		encoded, err := encodeAuth(registry.AuthConfig{
			Username:      user,
			Password:      pass,
			ServerAddress: host,
		})
		if err != nil {
			continue
		}
		a.byRegistry[host] = encoded
	}
	return a, nil
}

func encodeAuth(cfg registry.AuthConfig) (string, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("encode registry auth: %w", err)
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// For returns the encoded credentials for an image reference, empty when there
// are none — which is the normal case for public images.
func (a *RegistryAuth) For(ref string) string {
	if a == nil || len(a.byRegistry) == 0 {
		return ""
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ""
	}
	domain := reference.Domain(named)
	if domain == "docker.io" {
		if v, ok := a.byRegistry[dockerHubConfigKey]; ok {
			return v
		}
	}
	if v, ok := a.byRegistry[domain]; ok {
		return v
	}
	// config.json entries are sometimes written with a scheme.
	for _, prefix := range []string{"https://", "http://"} {
		if v, ok := a.byRegistry[prefix+domain]; ok {
			return v
		}
	}
	return ""
}

// HasHelperOnly reports whether the only credentials for this reference live in
// a credential helper this build cannot call.
func (a *RegistryAuth) HasHelperOnly(ref string) bool {
	if a == nil || len(a.helperRegistries) == 0 {
		return false
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return false
	}
	domain := reference.Domain(named)
	for _, h := range a.helperRegistries {
		if h == domain || strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://") == domain {
			return true
		}
	}
	return false
}

// RemoteDigest asks the registry for the current manifest digest of a reference
// without downloading the image.
//
// This is the whole point of checking before pulling: on a host with a hundred
// containers, pulling everything to find out that nothing changed is expensive
// enough that people turn updates off.
func (c *Client) RemoteDigest(ctx context.Context, ref, encodedAuth string) (string, error) {
	res, err := c.api.DistributionInspect(ctx, ref, client.DistributionInspectOptions{
		EncodedRegistryAuth: encodedAuth,
	})
	if err != nil {
		return "", fmt.Errorf("look up %s in its registry: %w", ref, err)
	}
	return res.Descriptor.Digest.String(), nil
}

// DigestForRepo picks the digest belonging to the same repository as ref out of
// a list of repo digests.
//
// A local image can carry digests from several repositories — the same content
// pulled under different names, or retagged after a push. Comparing against the
// wrong one reports an update that does not exist.
func DigestForRepo(ref string, repoDigests []string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		if len(repoDigests) == 1 {
			return digestPart(repoDigests[0])
		}
		return ""
	}
	want := reference.FamiliarName(named)

	for _, rd := range repoDigests {
		repo, digest, ok := strings.Cut(rd, "@")
		if !ok {
			continue
		}
		if repo == want {
			return digest
		}
		// Compare in normalised form too, so "nginx" matches
		// "docker.io/library/nginx".
		if parsed, err := reference.ParseNormalizedNamed(repo); err == nil {
			if reference.FamiliarName(parsed) == want {
				return digest
			}
		}
	}
	return ""
}

func digestPart(repoDigest string) string {
	if _, digest, ok := strings.Cut(repoDigest, "@"); ok {
		return digest
	}
	return ""
}
