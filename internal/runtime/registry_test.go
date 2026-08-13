package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func writeDockerConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHostCredentialsAreFoundForTheRightRegistry(t *testing.T) {
	// "dXNlcjpwYXNz" is base64 of "user:pass".
	path := writeDockerConfig(t, `{"auths":{
		"https://index.docker.io/v1/": {"auth":"dXNlcjpwYXNz"},
		"gitea.example.com": {"auth":"dXNlcjpwYXNz"}
	}}`)

	auth, err := LoadRegistryAuth(path)
	if err != nil {
		t.Fatal(err)
	}

	// Docker Hub is keyed by a URL that is not the image's domain.
	if auth.For("postgres:17") == "" {
		t.Error("Docker Hub credentials were not matched for a bare image name")
	}
	if auth.For("gitea.example.com/team/app:latest") == "" {
		t.Error("credentials for a private registry were not matched")
	}
	if auth.For("ghcr.io/someone/app:latest") != "" {
		t.Error("credentials were returned for a registry that has none")
	}
}

func TestCredentialsFromSeveralSourcesAreAllTried(t *testing.T) {
	// Which source holds the working secret for a registry is not knowable in
	// advance, so every candidate has to be offered rather than ranked.
	path := writeDockerConfig(t, `{"auths":{"ghcr.io":{"auth":"aG9zdDpzZWNyZXQ="}}}`)
	auth, err := LoadRegistryAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	auth.AddCredentials("komodo", "ghcr.io", "komodo-user", "komodo-token")

	attempts := auth.Attempts("ghcr.io/hudint/app:latest")
	if len(attempts) != 3 {
		t.Fatalf("got %d attempts, want host + komodo + anonymous: %v", len(attempts), attempts)
	}
	if attempts[len(attempts)-1] != "" {
		t.Error("the anonymous attempt is missing, so public images would need credentials")
	}
	if attempts[0] == attempts[1] {
		t.Error("both sources produced the same credentials")
	}

	// A registry neither source knows still gets the anonymous attempt.
	if got := auth.Attempts("docker.io/library/nginx"); len(got) != 1 || got[0] != "" {
		t.Errorf("Attempts for an unknown registry = %v, want one anonymous attempt", got)
	}
}

func TestRedactedCredentialsAreRecognisedRatherThanRetried(t *testing.T) {
	// An API that returns an account without a secret has redacted it. Reporting
	// that beats a bare "unauthorized", which sends people looking in the wrong
	// place entirely.
	auth := &RegistryAuth{}
	auth.AddCredentials("komodo", "ghcr.io", "", "")

	if got := auth.RedactedSource("ghcr.io/hudint/app:latest"); got != "komodo" {
		t.Errorf("RedactedSource = %q, want komodo", got)
	}
	if got := auth.RedactedSource("docker.io/library/nginx"); got != "" {
		t.Errorf("RedactedSource for an unrelated registry = %q", got)
	}
	if attempts := auth.Attempts("ghcr.io/hudint/app:latest"); len(attempts) != 1 {
		t.Errorf("an empty account produced usable credentials: %v", attempts)
	}
}

func TestDomainsAreNormalised(t *testing.T) {
	auth := &RegistryAuth{}
	auth.AddCredentials("komodo", "https://index.docker.io/v1/", "user", "token")
	if got := auth.Attempts("postgres:17"); len(got) != 2 {
		t.Errorf("Docker Hub credentials written in URL form were not matched: %v", got)
	}

	auth2 := &RegistryAuth{}
	auth2.AddCredentials("komodo", "https://gitea.example.com/", "user", "token")
	if got := auth2.Attempts("gitea.example.com/team/app"); len(got) != 2 {
		t.Errorf("a domain written with scheme and slash was not matched: %v", got)
	}
}

func TestDigestForRepoPicksTheMatchingRepository(t *testing.T) {
	// A local image can carry digests from several repositories. Comparing
	// against the wrong one reports an update that does not exist.
	digests := []string{
		"ghcr.io/other/app@sha256:aaaa",
		"ghcr.io/hudint/app@sha256:bbbb",
	}
	if got := DigestForRepo("ghcr.io/hudint/app:latest", digests); got != "sha256:bbbb" {
		t.Errorf("DigestForRepo = %q, want sha256:bbbb", got)
	}
	if got := DigestForRepo("ghcr.io/nobody/app:latest", digests); got != "" {
		t.Errorf("DigestForRepo returned %q for an unrelated repository", got)
	}
	// Familiar and normalised names must match each other.
	if got := DigestForRepo("postgres:17", []string{"postgres@sha256:cccc"}); got != "sha256:cccc" {
		t.Errorf("DigestForRepo = %q for a familiar name, want sha256:cccc", got)
	}
}
