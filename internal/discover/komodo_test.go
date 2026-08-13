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

func TestKomodoSendsTheDocumentedRequest(t *testing.T) {
	var seen []struct {
		path   string
		key    string
		secret string
		body   map[string]any
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		seen = append(seen, struct {
			path   string
			key    string
			secret string
			body   map[string]any
		}{r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("x-api-secret"), parsed})

		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	c := NewKomodoClient(KomodoConfig{
		URL: srv.URL, APIKey: "key", APISecret: "secret", Tag: "auto-update",
	})
	if _, err := c.Tagged(context.Background()); err != nil {
		t.Fatalf("Tagged: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("made %d requests, want 2 (stacks and deployments)", len(seen))
	}
	// The request type belongs in the path, not the body.
	if seen[0].path != "/read/ListStacks" {
		t.Errorf("first request path = %q, want /read/ListStacks", seen[0].path)
	}
	if seen[1].path != "/read/ListDeployments" {
		t.Errorf("second request path = %q, want /read/ListDeployments", seen[1].path)
	}
	if seen[0].key != "key" || seen[0].secret != "secret" {
		t.Error("api key headers were not sent")
	}

	query, ok := seen[0].body["query"].(map[string]any)
	if !ok {
		t.Fatalf("body has no query object: %v", seen[0].body)
	}
	tags, _ := query["tags"].([]any)
	if len(tags) != 1 || tags[0] != "auto-update" {
		t.Errorf("query tags = %v, want [auto-update]", tags)
	}
	if query["tag_behavior"] != "Any" {
		t.Errorf("tag_behavior = %v, want Any", query["tag_behavior"])
	}
}

func TestKomodoResolvesStacksAndDeployments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "ListStacks"):
			io.WriteString(w, `[
			  {"id":"1","type":"Stack","name":"vikunja","tags":["t"],"info":{"server_id":"s1","server_name":"prod-01"}},
			  {"id":"2","type":"Stack","name":"gitea","tags":["t"],"info":{"server_id":"s2","server_name":"prod-02"}}
			]`)
		default:
			io.WriteString(w, `[
			  {"id":"3","type":"Deployment","name":"ntfy","tags":["t"],"info":{"server_id":"s1","server_name":"prod-01","custom_name":"ntfy-prod"}}
			]`)
		}
	}))
	defer srv.Close()

	t.Run("without a server filter it warns about the ambiguity", func(t *testing.T) {
		c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "k", APISecret: "s", Tag: "t"})
		sel, err := c.Tagged(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(sel.Projects) != 2 {
			t.Errorf("projects = %v, want both stacks", sel.Projects)
		}
		// Acting on a stack that lives on another host is at best a no-op, so
		// the ambiguity has to be said out loud.
		if len(sel.Warnings) == 0 {
			t.Error("no warning although the tag spans two servers")
		}
	})

	t.Run("with a server filter only that host's resources are selected", func(t *testing.T) {
		c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "k", APISecret: "s", Tag: "t", Server: "prod-01"})
		sel, err := c.Tagged(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := sel.Projects["vikunja"]; !ok {
			t.Error("the stack on this server was not selected")
		}
		if _, ok := sel.Projects["gitea"]; ok {
			t.Error("a stack from another server was selected")
		}
		// A deployment's container name can differ from the deployment name.
		if _, ok := sel.Containers["ntfy-prod"]; !ok {
			t.Errorf("deployment was not resolved to its container name: %v", sel.Containers)
		}
	})
}

func TestKomodoReportsServerErrorsWithContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	defer srv.Close()

	c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "wrong", APISecret: "s", Tag: "t"})
	_, err := c.Tagged(context.Background())
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

	c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "k", APISecret: "s", Tag: "t"})
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

	c := NewKomodoClient(KomodoConfig{URL: srv.URL, APIKey: "k", APISecret: "s", Tag: "t"})
	if _, err := c.RegistryAccounts(context.Background()); err == nil {
		t.Fatal("an authentication failure was not reported")
	}
	if calls != 1 {
		t.Errorf("made %d calls, want 1 — an auth failure is not a reason to try another name", calls)
	}
}
