package update

import (
	"strings"
	"testing"

	"github.com/Hudint/backup-tower/internal/discover"
	"github.com/Hudint/backup-tower/internal/runtime"
)

func candidate(name, project, service, dependsOn string) *discover.Candidate {
	labels := map[string]string{}
	if project != "" {
		labels["com.docker.compose.project"] = project
		labels["com.docker.compose.service"] = service
	}
	if dependsOn != "" {
		labels["com.docker.compose.depends_on"] = dependsOn
	}
	return &discover.Candidate{
		Container: &runtime.Container{Name: name, Labels: labels},
	}
}

func names(candidates []*discover.Candidate) []string {
	out := make([]string, len(candidates))
	for i, c := range candidates {
		out[i] = c.Container.Name
	}
	return out
}

func TestDependenciesAreUpdatedFirst(t *testing.T) {
	// Replacing a web front end while the database it needs is still restarting
	// looks exactly like a failed update, and would trigger a rollback of
	// something that was never broken.
	in := []*discover.Candidate{
		candidate("app-web-1", "app", "web", "db:service_healthy:false,cache:service_started:false"),
		candidate("app-db-1", "app", "db", ""),
		candidate("app-cache-1", "app", "cache", ""),
	}

	got := names(Order(in))
	pos := map[string]int{}
	for i, n := range got {
		pos[n] = i
	}
	if pos["app-db-1"] > pos["app-web-1"] {
		t.Errorf("the database was updated after the service depending on it: %v", got)
	}
	if pos["app-cache-1"] > pos["app-web-1"] {
		t.Errorf("the cache was updated after the service depending on it: %v", got)
	}
	if len(got) != 3 {
		t.Errorf("Order dropped containers: %v", got)
	}
}

func TestTransitiveDependenciesAreRespected(t *testing.T) {
	in := []*discover.Candidate{
		candidate("s-c-1", "s", "c", "b:service_started:false"),
		candidate("s-b-1", "s", "b", "a:service_started:false"),
		candidate("s-a-1", "s", "a", ""),
	}
	got := names(Order(in))
	want := "s-a-1 s-b-1 s-c-1"
	if strings.Join(got, " ") != want {
		t.Errorf("Order = %v, want %s", got, want)
	}
}

func TestDependencyCycleDoesNotLoseContainers(t *testing.T) {
	// Compose itself refuses cycles, but a stale label can still describe one.
	// Degrading to some order is fine; dropping a container or hanging is not.
	in := []*discover.Candidate{
		candidate("s-a-1", "s", "a", "b:service_started:false"),
		candidate("s-b-1", "s", "b", "a:service_started:false"),
	}
	if got := names(Order(in)); len(got) != 2 {
		t.Errorf("a dependency cycle lost containers: %v", got)
	}
}

func TestContainersWithoutComposeKeepTheirPlace(t *testing.T) {
	in := []*discover.Candidate{
		candidate("standalone", "", "", ""),
		candidate("app-db-1", "app", "db", ""),
	}
	got := names(Order(in))
	if len(got) != 2 {
		t.Fatalf("Order = %v", got)
	}
	// Compose projects are ordered among themselves; loose containers follow.
	if got[len(got)-1] != "standalone" {
		t.Errorf("Order = %v, want the standalone container last", got)
	}
}

func TestDependsOnParsesComposeFormat(t *testing.T) {
	c := candidate("x", "p", "s", "db:service_healthy:false, cache:service_started:true")
	got := dependsOn(c)
	if len(got) != 2 || got[0] != "db" || got[1] != "cache" {
		t.Errorf("dependsOn = %v, want [db cache]", got)
	}
	if got := dependsOn(candidate("x", "p", "s", "")); got != nil {
		t.Errorf("dependsOn without the label = %v, want nil", got)
	}
}
