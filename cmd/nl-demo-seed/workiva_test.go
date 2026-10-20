package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// errRealClientInTest is what the default newWorkiva returns during unit
// tests: no test can build the real client or reach the network, even when
// credentials are in the environment. A test that needs a Workiva replaces
// newWorkiva with a fake.
var errRealClientInTest = errors.New("real Workiva client is disabled in unit tests; replace newWorkiva with a fake")

func TestMain(m *testing.M) {
	newWorkiva = func(context.Context) (Workiva, error) { return nil, errRealClientInTest }
	os.Exit(m.Run())
}

// fakeSeam installs wk (or err) as the result of newWorkiva for one test.
func fakeSeam(t *testing.T, wk Workiva, err error) {
	t.Helper()
	old := newWorkiva
	newWorkiva = func(context.Context) (Workiva, error) { return wk, err }
	t.Cleanup(func() { newWorkiva = old })
}

func TestLiveWorkivaSections(t *testing.T) {
	var deletes []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/iam/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":3600}`))
		case r.Method == http.MethodGet && r.URL.Path == "/documents/doc-1/sections":
			if r.Header.Get("Authorization") != "Bearer t" {
				http.Error(w, "no token", http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("page") == "2" {
				_, _ = w.Write([]byte(`{"data":[{"id":"s3","name":"Social disclosures"}]}`))
				return
			}
			if r.URL.Query().Get("$maxpagesize") != "100" {
				http.Error(w, "missing $maxpagesize", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"s1","name":"Environmental disclosures"},{"id":"s2","name":"Other"}],"@nextLink":%q}`,
				srv.URL+"/documents/doc-1/sections?page=2")
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/documents/doc-1/sections/"):
			deletes = append(deletes, strings.TrimPrefix(r.URL.Path, "/documents/doc-1/sections/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusTeapot)
		}
	}))
	defer srv.Close()
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	lw := newLiveWorkivaAt(base, "id", "secret", srv.Client())

	got, err := lw.ListSections(context.Background(), "doc-1")
	if err != nil {
		t.Fatalf("ListSections: %v", err)
	}
	want := []Section{{"s1", "Environmental disclosures"}, {"s2", "Other"}, {"s3", "Social disclosures"}}
	if len(got) != len(want) {
		t.Fatalf("sections = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("section %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if err := lw.DeleteSection(context.Background(), "doc-1", "s1"); err != nil {
		t.Fatalf("DeleteSection: %v", err)
	}
	if len(deletes) != 1 || deletes[0] != "s1" {
		t.Errorf("deletes = %v", deletes)
	}
}

func TestLiveWorkivaRefusesForeignNextLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/iam/v1/oauth2/token" {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":3600}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[],"@nextLink":"https://evil.example/documents/d/sections?page=2"}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	lw := newLiveWorkivaAt(base, "id", "secret", srv.Client())
	if _, err := lw.ListSections(context.Background(), "d"); err == nil {
		t.Error("a nextLink on another host was followed")
	}
}

func TestLiveWorkivaDeleteRejectsOtherStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/iam/v1/oauth2/token" {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":3600}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "nope"})
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	lw := newLiveWorkivaAt(base, "id", "secret", srv.Client())
	if err := lw.DeleteSection(context.Background(), "d", "s"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want status 404", err)
	}
}

func TestNewWorkivaRequiresCredentials(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	_, err := newLiveWorkivaFromEnv(get)
	if err == nil || !strings.Contains(err.Error(), "NL_WORKIVA_CLIENT_ID") {
		t.Fatalf("err = %v, want it to name NL_WORKIVA_CLIENT_ID", err)
	}
	env["NL_WORKIVA_CLIENT_ID"] = "the-id"
	_, err = newLiveWorkivaFromEnv(get)
	if err == nil || !strings.Contains(err.Error(), "NL_WORKIVA_CLIENT_SECRET") {
		t.Fatalf("err = %v, want it to name NL_WORKIVA_CLIENT_SECRET", err)
	}
	env["NL_WORKIVA_CLIENT_SECRET"] = "the-secret-value"
	env["NL_REGION"] = "mars"
	_, err = newLiveWorkivaFromEnv(get)
	if err == nil || !strings.Contains(err.Error(), "NL_REGION") {
		t.Fatalf("err = %v, want it to name NL_REGION", err)
	}
	for _, e := range []error{err} {
		if strings.Contains(e.Error(), "the-secret-value") || strings.Contains(e.Error(), "the-id") {
			t.Errorf("error leaks a credential: %v", e)
		}
	}
	for region, host := range map[string]string{"": "api.eu.wdesk.com", "eu": "api.eu.wdesk.com", "us": "api.app.wdesk.com", "apac": "api.apac.wdesk.com"} {
		env["NL_REGION"] = region
		base, err := regionBaseURL(region)
		if err != nil || base.Host != host || base.Scheme != "https" {
			t.Errorf("region %q: %v %v", region, base, err)
		}
		if _, err := newLiveWorkivaFromEnv(get); err != nil {
			t.Errorf("region %q: %v", region, err)
		}
	}
}

// The default newWorkiva must never be the real one while tests run.
func TestNewWorkivaSeamIsFakedInTests(t *testing.T) {
	if _, err := newWorkiva(context.Background()); !errors.Is(err, errRealClientInTest) {
		t.Errorf("newWorkiva = %v, want the test guard", err)
	}
}
