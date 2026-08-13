package discover

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// komodoStub answers the three read requests TaggedResources makes.
func komodoStub(t *testing.T, tags, stacks, deployments string) (*httptest.Server, *[]string) {
	t.Helper()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/read/")
		asked = append(asked, name)
		w.Header().Set("content-type", "application/json")
		switch name {
		case "ListTags":
			io.WriteString(w, tags)
		case "ListStacks":
			io.WriteString(w, stacks)
		case "ListDeployments":
			io.WriteString(w, deployments)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

const stubTags = `[
  {"_id":{"$oid":"t1"},"name":"bt-update"},
  {"_id":{"$oid":"t2"},"name":"bt-stop"},
  {"_id":{"$oid":"t3"},"name":"unrelated"}
]`

func TestKomodoSendsTheDocumentedRequest(t *testing.T) {
	var seen []struct {
		path   string
		key    string
		secret string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, struct {
			path   string
			key    string
			secret string
		}{r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("x-api-secret")})
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "key", APISecret: "secret"})
	if _, err := c.TaggedResources(context.Background()); err != nil {
		t.Fatalf("TaggedResources: %v", err)
	}

	// The request type belongs in the path, not the body.
	want := []string{"/read/ListTags", "/read/ListStacks", "/read/ListDeployments"}
	if len(seen) != len(want) {
		t.Fatalf("made %d requests, want %d", len(seen), len(want))
	}
	for i, w := range want {
		if seen[i].path != w {
			t.Errorf("request %d path = %q, want %q", i, seen[i].path, w)
		}
	}
	if seen[0].key != "key" || seen[0].secret != "secret" {
		t.Error("api key headers were not sent")
	}
}

func TestTagsAreResolvedToNames(t *testing.T) {
	// Resources reference tags by id; a policy is written against names.
	srv, _ := komodoStub(t, stubTags,
		`[{"id":"1","type":"Stack","name":"the-list","tags":["t1","t2"],"info":{"server_name":"prod-01"}}]`,
		`[{"id":"2","type":"Deployment","name":"ntfy","tags":["t1"],"info":{"server_name":"prod-01","custom_name":"ntfy-prod"}}]`)

	c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "k", APISecret: "s"})
	sel, err := c.TaggedResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	got := sel.Projects["the-list"]
	if len(got) != 2 || got[0] != "bt-stop" || got[1] != "bt-update" {
		t.Errorf("stack tags = %v, want [bt-stop bt-update]", got)
	}
	// A deployment's container name can differ from the deployment name.
	if got := sel.Containers["ntfy-prod"]; len(got) != 1 || got[0] != "bt-update" {
		t.Errorf("deployment tags = %v, want [bt-update]", got)
	}
}

func TestUntaggedResourcesAreIgnored(t *testing.T) {
	srv, _ := komodoStub(t, stubTags,
		`[{"id":"1","type":"Stack","name":"plain","tags":[],"info":{"server_name":"prod-01"}}]`, `[]`)

	c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "k", APISecret: "s"})
	sel, err := c.TaggedResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Projects) != 0 {
		t.Errorf("an untagged stack was included: %v", sel.Projects)
	}
}

func TestServerFilterAndAmbiguityWarning(t *testing.T) {
	stacks := `[
	  {"id":"1","type":"Stack","name":"here","tags":["t1"],"info":{"server_id":"s1","server_name":"prod-01"}},
	  {"id":"2","type":"Stack","name":"elsewhere","tags":["t1"],"info":{"server_id":"s2","server_name":"prod-02"}}
	]`

	t.Run("without a filter it warns rather than guesses", func(t *testing.T) {
		srv, _ := komodoStub(t, stubTags, stacks, `[]`)
		c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "k", APISecret: "s"})
		sel, err := c.TaggedResources(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Acting on a stack that lives on another host is at best a no-op.
		if len(sel.Warnings) == 0 {
			t.Error("no warning although tagged resources span two servers")
		}
	})

	t.Run("with a filter only this host's resources are kept", func(t *testing.T) {
		srv, _ := komodoStub(t, stubTags, stacks, `[]`)
		c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "k", APISecret: "s", Server: "prod-01"})
		sel, err := c.TaggedResources(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := sel.Projects["here"]; !ok {
			t.Error("the stack on this server was dropped")
		}
		if _, ok := sel.Projects["elsewhere"]; ok {
			t.Error("a stack from another server was included")
		}
	})
}

func TestKomodoReportsServerErrorsWithContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	defer srv.Close()

	c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "wrong", APISecret: "s"})
	_, err := c.TaggedResources(context.Background())
	if err == nil {
		t.Fatal("an unauthorised response was not reported as an error")
	}
	// The response body carries the actual reason; dropping it would leave the
	// operator guessing.
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error does not mention what the server said: %v", err)
	}
}

func TestRegistryAccountsFallBackToTheOlderRequestName(t *testing.T) {
	// Komodo renamed this request in v2.3.0. Pinning either name means the tool
	// silently stops finding credentials on one side of that boundary — which is
	// exactly how this was found: against an instance older than the docs.
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/read/")
		asked = append(asked, name)
		w.Header().Set("content-type", "application/json")
		if name == "ListImageRegistryAccounts" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"unknown variant `+"`"+`ListImageRegistryAccounts`+"`"+`, expected one of ..."}`)
			return
		}
		io.WriteString(w, `[{"domain":"ghcr.io","username":"someone","token":"secret"}]`)
	}))
	defer srv.Close()

	c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "k", APISecret: "s"})
	accounts, err := c.RegistryAccounts(context.Background())
	if err != nil {
		t.Fatalf("RegistryAccounts: %v", err)
	}
	if len(asked) != 2 || asked[0] != "ListImageRegistryAccounts" || asked[1] != "ListDockerRegistryAccounts" {
		t.Errorf("request sequence = %v, want the new name then the old one", asked)
	}
	if len(accounts) != 1 || accounts[0].Domain != "ghcr.io" || accounts[0].Token != "secret" {
		t.Errorf("accounts = %+v", accounts)
	}
}

func TestUnrelatedErrorsAreNotRetried(t *testing.T) {
	// Only an unknown request name justifies a second attempt; retrying an
	// authentication failure under a different name would just obscure it.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	defer srv.Close()

	c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "k", APISecret: "s"})
	if _, err := c.RegistryAccounts(context.Background()); err == nil {
		t.Fatal("an authentication failure was not reported")
	}
	if calls != 1 {
		t.Errorf("made %d calls, want 1 — an auth failure is not a reason to try another name", calls)
	}
}

func TestMongoIDHandlesBothForms(t *testing.T) {
	var wrapped any
	json.Unmarshal([]byte(`{"$oid":"abc123"}`), &wrapped)
	if got := mongoID(wrapped); got != "abc123" {
		t.Errorf("mongoID(wrapped) = %q", got)
	}
	if got := mongoID("plain"); got != "plain" {
		t.Errorf("mongoID(plain) = %q", got)
	}
	if got := mongoID(42); got != "" {
		t.Errorf("mongoID(unexpected) = %q, want empty", got)
	}
}
