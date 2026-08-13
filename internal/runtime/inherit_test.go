package runtime

import (
	"testing"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/moby/moby/api/types/container"
)

// The engine's inspect response does not distinguish what the operator asked for
// from what the image supplied. Recreating a container on a *new* image with the
// old image's values would run the new release with the previous version's
// entrypoint and healthcheck — and, worst of all, let a broken release pass a
// health gate that was judging the old version's healthcheck.
func TestImageSuppliedValuesAreDroppedSoTheNewImageApplies(t *testing.T) {
	img := &dockerspec.DockerOCIImageConfig{
		ImageConfig: ocispec.ImageConfig{
			Cmd:        []string{"sh", "-c", "old-command"},
			Entrypoint: []string{"/old-entrypoint"},
			Env:        []string{"PATH=/usr/bin", "APP_VERSION=1"},
			WorkingDir: "/app",
			User:       "app",
			Labels:     map[string]string{"org.opencontainers.image.version": "1.0"},
		},
		DockerOCIImageConfigExt: dockerspec.DockerOCIImageConfigExt{
			Healthcheck: &dockerspec.HealthcheckConfig{Test: []string{"CMD", "old-check"}, Retries: 3},
		},
	}

	cfg := &container.Config{
		Cmd:        []string{"sh", "-c", "old-command"}, // came from the image
		Entrypoint: []string{"/old-entrypoint"},         // came from the image
		Env: []string{
			"PATH=/usr/bin",   // from the image
			"APP_VERSION=1",   // from the image
			"MARKER=operator", // set by the operator
		},
		WorkingDir: "/app",
		User:       "app",
		Labels: map[string]string{
			"org.opencontainers.image.version": "1.0",  // from the image
			"tower.enable":                     "true", // set by the operator
		},
		Healthcheck: &container.HealthConfig{Test: []string{"CMD", "old-check"}, Retries: 3},
	}

	StripImageDefaults(cfg, img)

	if cfg.Cmd != nil {
		t.Errorf("Cmd was kept, so the new image cannot supply its own: %v", cfg.Cmd)
	}
	if cfg.Entrypoint != nil {
		t.Errorf("Entrypoint was kept: %v", cfg.Entrypoint)
	}
	if cfg.Healthcheck != nil {
		t.Error("Healthcheck was kept, so a broken release could pass the old version's health gate")
	}
	if cfg.WorkingDir != "" || cfg.User != "" {
		t.Errorf("image-supplied WorkingDir/User were kept: %q %q", cfg.WorkingDir, cfg.User)
	}

	if len(cfg.Env) != 1 || cfg.Env[0] != "MARKER=operator" {
		t.Errorf("Env = %v, want only the operator's entry", cfg.Env)
	}
	if _, ok := cfg.Labels["org.opencontainers.image.version"]; ok {
		t.Error("an image-supplied label was kept")
	}
	if cfg.Labels["tower.enable"] != "true" {
		t.Error("an operator label was dropped — that would silently disable the container")
	}
}

func TestOperatorOverridesSurvive(t *testing.T) {
	img := &dockerspec.DockerOCIImageConfig{
		ImageConfig: ocispec.ImageConfig{
			Cmd:        []string{"default"},
			Entrypoint: []string{"/entrypoint"},
			User:       "app",
		},
		DockerOCIImageConfigExt: dockerspec.DockerOCIImageConfigExt{
			Healthcheck: &dockerspec.HealthcheckConfig{Test: []string{"CMD", "image-check"}},
		},
	}
	cfg := &container.Config{
		Cmd:         []string{"operator-command"},
		Entrypoint:  []string{"/entrypoint"},
		User:        "root",
		Healthcheck: &container.HealthConfig{Test: []string{"CMD", "operator-check"}},
	}

	StripImageDefaults(cfg, img)

	if len(cfg.Cmd) != 1 || cfg.Cmd[0] != "operator-command" {
		t.Errorf("an explicitly set Cmd was dropped: %v", cfg.Cmd)
	}
	if cfg.User != "root" {
		t.Errorf("an explicitly set User was dropped: %q", cfg.User)
	}
	if cfg.Healthcheck == nil || cfg.Healthcheck.Test[1] != "operator-check" {
		t.Error("an explicitly set healthcheck was dropped")
	}
	// The entrypoint matched the image's, so it was inherited, not chosen.
	if cfg.Entrypoint != nil {
		t.Errorf("an inherited Entrypoint was kept: %v", cfg.Entrypoint)
	}
}

func TestStripHandlesMissingInput(t *testing.T) {
	// Recreating without knowing the old image must not panic; it just means
	// nothing can be subtracted.
	StripImageDefaults(nil, nil)
	StripImageDefaults(&container.Config{}, nil)
	StripImageDefaults(nil, &dockerspec.DockerOCIImageConfig{})
}
