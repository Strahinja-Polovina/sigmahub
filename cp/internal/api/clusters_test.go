package api

// The shape of a cluster on the wire.
//
// ListClusters goes out of its way to set `c.Nodes = []ClusterNode{}` before
// appending, because the dashboard reads `cluster.nodes.length` and a nil Go
// slice marshals to `null` rather than `[]`. CreateCluster built its Cluster
// literal without touching Nodes, so the one endpoint that is guaranteed to
// return a cluster with no nodes yet was the one endpoint that returned
// `"nodes": null` — while the TypeScript type declares nodes as an array and
// the compiler agrees.
//
// That is a latent trap rather than a live crash: the create dialog currently
// discards the returned cluster. The moment any caller uses it the way the
// panel uses listed clusters, it throws `Cannot read properties of null` right
// after a cluster was successfully created. Asserted on the RAW bytes for the
// reason compose_test.go gives — `null` and `[]` both decode into a nil Go
// slice, so a decoded assertion would pass against the exact payload that
// crashes the page (SIGMA-336).

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// fakeClusters answers like the store does: CreateCluster returns the row it
// just wrote, which by construction has no nodes attached to the value yet.
type fakeClusters struct {
	list []store.Cluster
}

func (f *fakeClusters) CreateCluster(_ context.Context, orgID string, in store.CreateClusterInput, actor string) (store.Cluster, error) {
	return store.Cluster{
		ID: "cls_1", OrgID: orgID, EnvironmentID: in.EnvironmentID,
		Name: in.Name, Status: "provisioning", CreatedBy: actor,
	}, nil
}

func (f *fakeClusters) ListClusters(context.Context, string, string) ([]store.Cluster, error) {
	if f.list == nil {
		return []store.Cluster{}, nil
	}
	return f.list, nil
}

func (f *fakeClusters) AddClusterNode(context.Context, string, string, string, string) error {
	return nil
}

func (f *fakeClusters) RemoveClusterNode(context.Context, string, string, string, string) error {
	return nil
}

func (f *fakeClusters) DeleteCluster(context.Context, string, string, string) ([]string, error) {
	return nil, nil
}

func (f *fakeClusters) ReportClusterNode(context.Context, string, store.ClusterNodeReport) (string, string, error) {
	return "", "", nil
}

func clusterServer(t *testing.T) *Server {
	t.Helper()
	return New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken: testServiceToken,
		Clusters:        &fakeClusters{},
	})
}

func TestCreateClusterAnswersAnEmptyNodeListRatherThanNull(t *testing.T) {
	s := clusterServer(t)

	body := `{"environmentId":"env_1","name":"production","controlPlaneId":"srv_1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/orgs/org_1/clusters", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST clusters = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unreadable JSON: %v", err)
	}
	if got := string(raw["nodes"]); got != "[]" {
		t.Errorf(`nodes = %s, want []: CpCluster.nodes is typed as an array and the clusters `+
			`panel reads .length and .flatMap on it, so null is a thrown TypeError the moment `+
			`a caller uses the created cluster instead of discarding it`, got)
	}
}

func TestListClustersKeepsAnsweringWithItsNodes(t *testing.T) {
	// The empty-list guard must not be reachable when there ARE nodes — a "fix"
	// that flattened every answer to [] would satisfy the test above.
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken: testServiceToken,
		Clusters: &fakeClusters{list: []store.Cluster{{
			ID: "cls_1", Name: "production",
			Nodes: []store.ClusterNode{{ServerID: "srv_1", Role: store.NodeRoleControlPlane}},
		}}},
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
	if len(out.Clusters) != 1 || len(out.Clusters[0].Nodes) != 1 ||
		out.Clusters[0].Nodes[0].ServerID != "srv_1" {
		t.Fatalf("clusters = %+v, want the one cluster with its control-plane node", out.Clusters)
	}
}
