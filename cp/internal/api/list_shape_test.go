package api

// The JSON shape of an EMPTY collection, asserted once for every list route.
//
// Commit 54c2314 fixed the compose endpoint by coercing nil to `[]` at the
// handler and noted that every other list handler was safe because "every store
// method behind them declares `out := []T{}` and cannot return nil". That is
// true today and it is also the entire guarantee: roughly twenty handlers
// marshal the store's slice straight to the client, so the wire contract rests
// on a convention in a different package with nothing checking either end.
//
// The convention is one idiomatic tidy-up away from breaking. Change
// `out := []Resource{}` to `var out []Resource` in ListResources — a change
// reviewers wave through — and an org with no resources yet answers
// `{"resources": null}`; cpListResources returns null; cp-sync's
// `cpResources.map(...)` throws inside the dashboard layout's mirror sync, and
// every page in a brand-new org renders an error. Both halves look correct in
// isolation and the whole Go suite stays green: that was measured, not assumed.
//
// So the assertion is made where the bug is observable — on the bytes — and the
// stores behind these handlers are made to return nil, which is precisely the
// state the convention promises never to produce. A stub that returned `[]T{}`
// like the ordinary fakes do would move the promise into the test and prove
// nothing.
//
// Asserted on RAW bytes: `null` and `[]` both decode into a nil Go slice, so a
// decoded assertion passes against the exact payload that crashes the page
// (SIGMA-337).

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// nilLists is the ordinary fake domain with every list method returning a nil
// slice. Embedding rather than editing fakeDomain keeps the nil answers out of
// every other handler test in this package.
type nilLists struct{ *fakeDomain }

func (nilLists) ListProjects(context.Context, string) ([]store.Project, error) { return nil, nil }
func (nilLists) ListEnvironments(context.Context, string, string) ([]store.Environment, error) {
	return nil, nil
}
func (nilLists) EnvServerIDs(context.Context, string, string) ([]string, error) { return nil, nil }
func (nilLists) ListResources(context.Context, string, string, string) ([]store.Resource, error) {
	return nil, nil
}
func (nilLists) ListAudit(context.Context, string, int) ([]store.AuditEntry, error) {
	return nil, nil
}
func (nilLists) ListServiceTokens(context.Context, string) ([]store.ServiceTokenInfo, error) {
	return nil, nil
}
func (nilLists) ListSecrets(context.Context, string, string, string) ([]store.Secret, error) {
	return nil, nil
}
func (nilLists) ListBackupTargets(context.Context, string) ([]store.BackupTarget, error) {
	return nil, nil
}
func (nilLists) ListBackupRuns(context.Context, string, string, int) ([]store.BackupRun, error) {
	return nil, nil
}
func (nilLists) ListArchivedRepoKeys(context.Context, string) ([]store.ArchivedRepoKey, error) {
	return nil, nil
}
func (nilLists) ListDomainsForResource(context.Context, string, string) ([]store.Domain, error) {
	return nil, nil
}
func (nilLists) ListDeployments(context.Context, string, string, int) ([]store.Deployment, error) {
	return nil, nil
}
func (nilLists) RollbackTargets(context.Context, string, string, int) ([]store.Deployment, error) {
	return nil, nil
}
func (nilLists) ListBuckets(context.Context, string, string) ([]store.Bucket, error) {
	return nil, nil
}
func (nilLists) ListAlertChannels(context.Context, string) ([]store.AlertChannel, error) {
	return nil, nil
}

// nilListStore is the same trick for the one list route served by StoreAPI.
type nilListStore struct{ *fakeStore }

func (nilListStore) ListServers(context.Context, string) ([]store.Server, error) { return nil, nil }

// nilListClusters answers the clusters route with a nil slice.
type nilListClusters struct{ *fakeClusters }

func (nilListClusters) ListClusters(context.Context, string, string) ([]store.Cluster, error) {
	return nil, nil
}

func TestListEndpointsEncodeEmptyAsArray(t *testing.T) {
	s := New(slog.Default(), fakePinger{}, nilListStore{&fakeStore{}}, nilLists{&fakeDomain{}}, Options{
		DevServiceToken: testServiceToken,
		Clusters:        nilListClusters{&fakeClusters{}},
	})

	cases := []struct {
		name string
		path string
		// keys that must each encode as [] in the response object.
		keys []string
	}{
		{"servers", "/v1/orgs/org_1/servers", []string{"servers"}},
		{"projects", "/v1/orgs/org_1/projects", []string{"projects"}},
		{"environments", "/v1/orgs/org_1/projects/prj_1/environments", []string{"environments"}},
		{"env servers", "/v1/orgs/org_1/environments/env_1/servers", []string{"serverIds"}},
		{"resources", "/v1/orgs/org_1/resources", []string{"resources"}},
		{"audit", "/v1/orgs/org_1/audit", []string{"entries"}},
		{"service tokens", "/v1/orgs/org_1/service-tokens", []string{"tokens"}},
		{"secrets", "/v1/orgs/org_1/projects/prj_1/secrets", []string{"secrets"}},
		{"backup targets", "/v1/orgs/org_1/backup-targets", []string{"targets"}},
		{"backup runs", "/v1/orgs/org_1/resources/res_1/backup-runs", []string{"runs"}},
		{"archived repo keys", "/v1/orgs/org_1/backup-repo-keys", []string{"keys"}},
		{"domains", "/v1/orgs/org_1/resources/res_1/domains", []string{"domains"}},
		{"deployments", "/v1/orgs/org_1/resources/res_1/deployments", []string{"deployments"}},
		{"rollback targets", "/v1/orgs/org_1/resources/res_1/rollback-targets", []string{"targets"}},
		{"buckets", "/v1/orgs/org_1/resources/res_1/buckets", []string{"buckets"}},
		{"alert channels", "/v1/orgs/org_1/alert-channels", []string{"channels"}},
		{"clusters", "/v1/orgs/org_1/clusters", []string{"clusters"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+testServiceToken)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200; body %s", tc.path, rec.Code, rec.Body.String())
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("unreadable JSON from %s: %v", tc.path, err)
			}
			for _, k := range tc.keys {
				got, ok := raw[k]
				if !ok {
					t.Fatalf("GET %s: response has no %q key; body %s", tc.path, k, rec.Body.String())
				}
				if string(got) != "[]" {
					t.Errorf("GET %s: %s = %s, want []: the dashboard maps and reads .length over "+
						"this key, so null is a thrown TypeError rather than an empty list",
						tc.path, k, got)
				}
			}
		})
	}
}

// The org-wide deploy feed is a struct rather than a map, so its two slices are
// nested one level deeper than every case above. cp-sync reads both of them
// (`cpDeploys.latest.map(...)`) on every dashboard render, which makes it the
// most expensive one to get wrong.
func TestOrgDeployFeedEncodesEmptyAsArrays(t *testing.T) {
	s := New(slog.Default(), fakePinger{}, nilListStore{&fakeStore{}}, nilLists{&fakeDomain{}}, Options{
		DevServiceToken: testServiceToken,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/orgs/org_1/deployments", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET deployments = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unreadable JSON: %v", err)
	}
	for _, k := range []string{"recent", "latest"} {
		if got := string(raw[k]); got != "[]" {
			t.Errorf("%s = %s, want []", k, got)
		}
	}
}

// A populated list must still arrive whole: a coercion that replaced every
// slice, rather than only the nil ones, would satisfy the tests above while
// emptying the dashboard.
func TestListEndpointsStillReturnTheirRows(t *testing.T) {
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken: testServiceToken,
		Clusters: &fakeClusters{list: []store.Cluster{
			{ID: "cls_1", Name: "production", Nodes: []store.ClusterNode{}},
		}},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/orgs/org_1/clusters", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var out struct {
		Clusters []store.Cluster `json:"clusters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unreadable JSON: %v", err)
	}
	if len(out.Clusters) != 1 || out.Clusters[0].ID != "cls_1" {
		t.Fatalf("clusters = %+v, want the one cluster the store returned", out.Clusters)
	}
}
