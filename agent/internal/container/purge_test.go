package container

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDaemon serves just enough of the Docker Engine API for the uninstall
// teardown, and records what was asked of it. A real daemon is not available on
// every host `go test` runs on, and the property under test is which calls the
// teardown makes and in what state it leaves the agent — not Docker's own
// behaviour, which docker_e2e_test.go covers when a daemon is present.
type fakeDaemon struct {
	containers []map[string]any
	networks   []map[string]any
	volumes    []string
	removed    []string
	// failList makes /containers/json fail, standing in for a daemon that is
	// down — the case where the teardown cannot do its first job.
	failList bool
}

func (f *fakeDaemon) server(t *testing.T) *DockerClient {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/containers/json"):
			if f.failList {
				http.Error(w, `{"message":"daemon not reachable"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(f.containers)
		case strings.HasSuffix(path, "/networks"):
			_ = json.NewEncoder(w).Encode(f.networks)
		case strings.HasSuffix(path, "/volumes"):
			vols := make([]map[string]any, 0, len(f.volumes))
			for _, v := range f.volumes {
				vols = append(vols, map[string]any{"Name": v})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": vols})
		case r.Method == http.MethodDelete:
			// /containers/<id>, /networks/<name>, /volumes/<name>
			parts := strings.Split(strings.Trim(path, "/"), "/")
			f.removed = append(f.removed, parts[len(parts)-2]+":"+parts[len(parts)-1])
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(ts.Close)
	// The client only honours a tcp:// DOCKER_HOST override; anything else
	// dials the unix socket this host does not have.
	return NewDockerClient("", "tcp://"+strings.TrimPrefix(ts.URL, "http://"))
}

func testDriver(t *testing.T, f *fakeDaemon) (*Driver, *Store) {
	t.Helper()
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewDriver(f.server(t), st, slog.New(slog.NewTextHandler(io.Discard, nil)), nil), st
}

// The teardown must leave nothing for the reconcile loop to restore.
//
// The driver re-creates any container still recorded as desired, every 30
// seconds, from local state and with no control-plane round-trip. A teardown
// that removed the containers without emptying that store would have the agent
// reaping a workload and then dutifully bringing it back — right up until it
// deletes its own data directory, at which point whatever it restored last is
// what the operator finds still running on the machine they were told is clean.
func TestRemoveManagedContainersEmptiesTheDesiredStore(t *testing.T) {
	f := &fakeDaemon{containers: []map[string]any{
		{"Id": "c1", "Names": []string{"/sigmahub-res_a"}, "State": "running",
			"Labels": map[string]string{LabelManaged: "true"}},
		{"Id": "c2", "Names": []string{"/sigmahub-res_b"}, "State": "exited",
			"Labels": map[string]string{LabelManaged: "true"}},
	}}
	d, st := testDriver(t, f)
	for _, name := range []string{"sigmahub-res_a", "sigmahub-res_b"} {
		if err := st.PutDesired(name, ContainerSpec{Name: name, Image: "nginx:1@sha256:abc"}); err != nil {
			t.Fatal(err)
		}
	}

	if err := d.RemoveManagedContainers(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	desired, err := st.AllDesired()
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 0 {
		t.Fatalf("desired store still holds %v — the reconcile loop will recreate them", desired)
	}
	if len(f.removed) != 2 {
		t.Fatalf("removed %v, want both containers", f.removed)
	}
}

// And it empties the store even when Docker cannot be reached. The agent is
// going away either way; leaving the desired set populated would mean a
// restart-before-the-binary-goes resurrects workloads on a host the control
// plane has already been told about.
func TestRemoveManagedContainersForgetsDesiredEvenWhenDockerFails(t *testing.T) {
	f := &fakeDaemon{failList: true}
	d, st := testDriver(t, f)
	if err := st.PutDesired("sigmahub-res_a", ContainerSpec{Name: "sigmahub-res_a"}); err != nil {
		t.Fatal(err)
	}

	err := d.RemoveManagedContainers(context.Background())
	if err == nil {
		t.Fatal("an unreachable daemon reported a clean teardown")
	}
	desired, derr := st.AllDesired()
	if derr != nil {
		t.Fatal(derr)
	}
	if len(desired) != 0 {
		t.Fatalf("desired store still holds %v after a failed teardown", desired)
	}
}

// A peer agent's objects are never swept. On a real host there is one agent, but
// the fleet e2e runs several against ONE daemon, and an uninstall that matched
// on the managed label alone would take a peer's workloads down with it.
func TestRemoveManagedContainersLeavesAPeersObjectsAlone(t *testing.T) {
	f := &fakeDaemon{containers: []map[string]any{
		{"Id": "mine", "Names": []string{"/sigmahub-res_a"}, "State": "running",
			"Labels": map[string]string{LabelManaged: "true", LabelServerID: "srv_me"}},
		{"Id": "theirs", "Names": []string{"/sigmahub-res_b"}, "State": "running",
			"Labels": map[string]string{LabelManaged: "true", LabelServerID: "srv_peer"}},
	}}
	d, _ := testDriver(t, f)
	d.SetServerID("srv_me")

	if err := d.RemoveManagedContainers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != 1 || !strings.HasSuffix(f.removed[0], "mine") {
		t.Fatalf("removed %v, want only this agent's container", f.removed)
	}
}

func TestRemoveManagedNetworksAndVolumes(t *testing.T) {
	f := &fakeDaemon{
		networks: []map[string]any{{"Name": "sigmahub-prj_1"}, {"Name": "sigmahub-app-res_a"}},
		volumes:  []string{"sigmahub-res_a-data"},
	}
	d, _ := testDriver(t, f)

	if err := d.RemoveManagedNetworks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != 2 {
		t.Fatalf("networks removed = %v, want both", f.removed)
	}
	f.removed = nil
	if err := d.RemoveManagedVolumes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != 1 || !strings.Contains(f.removed[0], "sigmahub-res_a-data") {
		t.Fatalf("volumes removed = %v", f.removed)
	}
}
