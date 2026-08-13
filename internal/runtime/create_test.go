package runtime

import (
	"net/netip"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

const deadID = "4648a6c4f9c3f0f7b8c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3"

func TestHostnameIsDroppedWhenItIsTheOldContainerID(t *testing.T) {
	// The engine defaults Hostname to the container's own short ID. Carrying it
	// over would pin the replacement to the identity of a container that no
	// longer exists.
	if !isShortIDOf(deadID[:12], deadID) {
		t.Error("a 12-character prefix of the container ID was not recognised as its short ID")
	}
	if !isShortIDOf(deadID, deadID) {
		t.Error("the full container ID was not recognised as its own")
	}
	// A hostname the operator chose must survive, even one that looks hex-ish.
	if isShortIDOf("db", deadID) {
		t.Error("a short user-chosen hostname was mistaken for a container ID")
	}
	if isShortIDOf("4648a6c4f9c4", deadID) {
		t.Error("a hostname that merely resembles the container ID was dropped")
	}
	if isShortIDOf("", deadID) {
		t.Error("an empty hostname was treated as a container ID")
	}
}

func TestAliasesDerivedFromTheContainerIDAreDropped(t *testing.T) {
	got := cleanAliases([]string{"web", deadID[:12], "api"}, deadID)
	want := []string{"web", "api"}
	if len(got) != len(want) {
		t.Fatalf("cleanAliases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cleanAliases = %v, want %v", got, want)
		}
	}
}

func TestNetworkingConfigKeepsRequestedSettingsAndDropsAssignedOnes(t *testing.T) {
	in := &container.InspectResponse{
		ID: deadID,
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"proxy": {
					// Requested by the operator — must survive.
					IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("172.20.0.5")},
					Aliases:    []string{"web", deadID[:12]},
					DriverOpts: map[string]string{"opt": "value"},
					Links:      []string{"db:db"},
					// Assigned by the engine — must not be replayed.
					NetworkID:  "abc123",
					EndpointID: "def456",
					Gateway:    netip.MustParseAddr("172.20.0.1"),
					IPAddress:  netip.MustParseAddr("172.20.0.5"),
					MacAddress: network.HardwareAddr{0x02, 0x42, 0xac, 0x14, 0x00, 0x05},
				},
			},
		},
	}

	cfg, _ := networkingConfig(in)
	if cfg == nil {
		t.Fatal("networkingConfig returned nothing for a container with a network")
	}
	ep := cfg.EndpointsConfig["proxy"]
	if ep == nil {
		t.Fatal("the proxy network is missing from the rebuilt config")
	}

	if ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address.String() != "172.20.0.5" {
		t.Error("the requested static address was lost")
	}
	if len(ep.Aliases) != 1 || ep.Aliases[0] != "web" {
		t.Errorf("aliases = %v, want just [web]", ep.Aliases)
	}
	if ep.DriverOpts["opt"] != "value" {
		t.Error("driver options were lost")
	}
	if len(ep.Links) != 1 {
		t.Error("links were lost")
	}

	// Replaying engine-assigned identifiers either gets ignored or collides with
	// whatever the engine has handed out since.
	if ep.NetworkID != "" || ep.EndpointID != "" || ep.Gateway.IsValid() || ep.IPAddress.IsValid() || len(ep.MacAddress) != 0 {
		t.Errorf("engine-assigned state was carried over: %+v", ep)
	}
}

func TestNetworkingConfigIsNilWithoutNetworks(t *testing.T) {
	if cfg, _ := networkingConfig(&container.InspectResponse{ID: deadID}); cfg != nil {
		t.Error("a container without networks produced a networking config")
	}
}

func TestKeepTagIsAValidReference(t *testing.T) {
	tag := KeepTag("tower-test-db-1", "2026-08-13T12-25-38Z")
	if want := "backup-tower/keep:tower-test-db-1-2026-08-13T12-25-38Z"; tag != want {
		t.Errorf("KeepTag = %q, want %q", tag, want)
	}
	// Container names can carry characters a tag cannot.
	if got := KeepTag("weird/name:with@stuff", "2026-08-13T12-25-38Z"); got != "backup-tower/keep:weird_name_with_stuff-2026-08-13T12-25-38Z" {
		t.Errorf("KeepTag did not sanitise the container name: %q", got)
	}
}
