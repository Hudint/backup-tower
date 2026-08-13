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
	// helperRegistries are registries whose credentials live in an external
	// credential helper we cannot call.
	helperRegistries []string
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
