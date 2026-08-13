package snapshot

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/hudint/backup-tower/internal/runtime"
)

// Spec is the stored container configuration. It keeps the engine's inspect
// response verbatim rather than a distilled struct: recreating a container
// faithfully depends on fields that are easy to overlook — capabilities, sysctls,
// ulimits, device mappings, network aliases — and a lossy capture would only be
// discovered during a rollback, when it is far too late to go back for them.
type Spec struct {
	SchemaVersion int       `json:"schema_version"`
	CapturedAt    time.Time `json:"captured_at"`
	Tool          string    `json:"tool"`

	// Inspect is the untouched engine response.
	Inspect json.RawMessage `json:"inspect"`
}

// NewSpec captures a container's configuration.
func NewSpec(c *runtime.Container, tool string, at time.Time) *Spec {
	raw := c.Raw
	if len(raw) == 0 {
		// Should not happen with a live inspect, but re-encoding the decoded
		// struct is a better fallback than storing nothing.
		if b, err := json.Marshal(c.Inspect); err == nil {
			raw = b
		}
	}
	return &Spec{
		SchemaVersion: SchemaVersion,
		CapturedAt:    at.UTC(),
		Tool:          tool,
		Inspect:       raw,
	}
}

// Encode renders the spec as indented JSON.
func (s *Spec) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode spec: %w", err)
	}
	return append(b, '\n'), nil
}

// DecodeSpec parses a stored spec.
func DecodeSpec(b []byte) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode spec: %w", err)
	}
	if s.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("spec uses schema version %d, this build understands up to %d", s.SchemaVersion, SchemaVersion)
	}
	if len(s.Inspect) == 0 {
		return nil, fmt.Errorf("spec contains no inspect payload")
	}
	return &s, nil
}

// Container decodes the captured inspect response.
func (s *Spec) Container() (*container.InspectResponse, error) {
	var in container.InspectResponse
	if err := json.Unmarshal(s.Inspect, &in); err != nil {
		return nil, fmt.Errorf("decode captured container spec: %w", err)
	}
	return &in, nil
}
