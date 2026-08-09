package container

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/build"
)

// apiVersion pins the Docker Engine API version. 1.43 ships with Docker 24.0+,
// which covers every supported host and CI runner; all fields used here (host
// config isolation knobs, labels, restart policy) predate it.
const apiVersion = "v1.43"

// DockerClient is a minimal Docker Engine API client speaking HTTP over the
// local unix socket. It intentionally uses only the standard library so the
// agent avoids the large Docker SDK dependency tree.
type DockerClient struct {
	http *http.Client
	host string // for a tcp DOCKER_HOST override; empty = unix socket
}

// NewDockerClient dials the Docker daemon. socket defaults to
// /var/run/docker.sock; a DOCKER_HOST of the form tcp://host:port is honoured
// for test rigs.
func NewDockerClient(socket, dockerHost string) *DockerClient {
	if strings.HasPrefix(dockerHost, "tcp://") {
		base := strings.TrimPrefix(dockerHost, "tcp://")
		return &DockerClient{
			http: &http.Client{Timeout: 0},
			host: "http://" + base,
		}
	}
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &DockerClient{http: &http.Client{Transport: tr}}
}

func (d *DockerClient) url(path string) string {
	if d.host != "" {
		return d.host + "/" + apiVersion + path
	}
	// The host part is ignored by the unix transport but required by net/http.
	return "http://docker/" + apiVersion + path
}

// apiError is a non-2xx Docker API response.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("docker api %d: %s", e.Status, e.Message) }

func isNotFound(err error) bool {
	var ae *apiError
	if ok := asAPIError(err, &ae); ok {
		return ae.Status == http.StatusNotFound
	}
	return false
}

func asAPIError(err error, target **apiError) bool {
	if ae, ok := err.(*apiError); ok {
		*target = ae
		return true
	}
	return false
}

// do issues a request, decoding a JSON body into out (may be nil). A non-2xx
// response is returned as *apiError carrying the daemon's message.
func (d *DockerClient) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.url(path), rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &apiError{Status: resp.StatusCode, Message: decodeDockerMessage(b)}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func decodeDockerMessage(b []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		return m.Message
	}
	return strings.TrimSpace(string(b))
}

// Ping reports whether the daemon is reachable and its version string.
func (d *DockerClient) Ping(ctx context.Context) (string, error) {
	var v struct {
		Version    string `json:"Version"`
		APIVersion string `json:"ApiVersion"`
	}
	if err := d.do(ctx, http.MethodGet, "/version", nil, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// --- Networks ---

type networkInspect struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

func (d *DockerClient) NetworkExists(ctx context.Context, name string) (bool, error) {
	err := d.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, &networkInspect{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

func (d *DockerClient) NetworkCreate(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{
		"Name":           name,
		"Driver":         "bridge",
		"CheckDuplicate": true,
		"Labels":         labels,
	}
	err := d.do(ctx, http.MethodPost, "/networks/create", body, nil)
	// A concurrent ensure may have created it; treat 409 as success.
	var ae *apiError
	if asAPIError(err, &ae) && ae.Status == http.StatusConflict {
		return nil
	}
	return err
}

// ManagedNetworks lists the names of sigmahub-managed bridge networks (those the
// agent created for projects). Used to attach the Traefik proxy so it can reach
// every app it fronts.
func (d *DockerClient) ManagedNetworks(ctx context.Context) ([]string, error) {
	q := url.Values{}
	q.Set("filters", `{"label":["`+LabelManaged+`=true"]}`)
	var nets []networkInspect
	if err := d.do(ctx, http.MethodGet, "/networks?"+q.Encode(), nil, &nets); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.Name)
	}
	return out, nil
}

// NetworkRemove deletes a network. A network that is already gone is success
// (idempotent) — the decommission teardown may run twice if the op is
// re-delivered after a crash.
func (d *DockerClient) NetworkRemove(ctx context.Context, name string) error {
	err := d.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(name), nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// ManagedVolumes lists the named volumes carrying the managed label. The
// /volumes response nests them under "Volumes" (unlike /networks), and a daemon
// with none returns null there — hence the explicit nil-safe walk.
func (d *DockerClient) ManagedVolumes(ctx context.Context) ([]string, error) {
	q := url.Values{}
	q.Set("filters", `{"label":["`+LabelManaged+`=true"]}`)
	var res struct {
		Volumes []struct {
			Name string `json:"Name"`
		} `json:"Volumes"`
	}
	if err := d.do(ctx, http.MethodGet, "/volumes?"+q.Encode(), nil, &res); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Volumes))
	for _, v := range res.Volumes {
		out = append(out, v.Name)
	}
	return out, nil
}

// NetworkConnect attaches a container to a network. Connecting an
// already-connected container is treated as success (idempotent).
func (d *DockerClient) NetworkConnect(ctx context.Context, network, container string) error {
	body := map[string]any{"Container": container}
	err := d.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", body, nil)
	var ae *apiError
	if asAPIError(err, &ae) && (ae.Status == http.StatusForbidden || ae.Status == http.StatusConflict) {
		return nil // already connected
	}
	return err
}

// VolumeMountpoint returns a managed volume's host filesystem path (the
// _data dir), so the agent (running as root) can read files Traefik persists
// there — namely the ACME store, to report certificate status.
func (d *DockerClient) VolumeMountpoint(ctx context.Context, name string) (string, error) {
	var v struct {
		Mountpoint string `json:"Mountpoint"`
	}
	if err := d.do(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, &v); err != nil {
		return "", err
	}
	return v.Mountpoint, nil
}

// --- Volumes ---

func (d *DockerClient) VolumeExists(ctx context.Context, name string) (bool, error) {
	_, exists, err := d.VolumeInspect(ctx, name)
	return exists, err
}

// VolumeInspect returns a volume's labels and whether it exists. Used to gate
// removal to sigmahub-managed volumes only.
func (d *DockerClient) VolumeInspect(ctx context.Context, name string) (map[string]string, bool, error) {
	var v struct {
		Labels map[string]string `json:"Labels"`
	}
	err := d.do(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, &v)
	if err == nil {
		return v.Labels, true, nil
	}
	if isNotFound(err) {
		return nil, false, nil
	}
	return nil, false, err
}

func (d *DockerClient) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{"Name": name, "Driver": "local", "Labels": labels}
	return d.do(ctx, http.MethodPost, "/volumes/create", body, nil)
}

func (d *DockerClient) VolumeRemove(ctx context.Context, name string, force bool) error {
	path := "/volumes/" + url.PathEscape(name)
	if force {
		path += "?force=true"
	}
	err := d.do(ctx, http.MethodDelete, path, nil, nil)
	if isNotFound(err) {
		return nil // already gone: idempotent
	}
	return err
}

// --- Images ---

// ImagePull pulls an image reference. It drains the progress stream and fails
// if the stream reports an error. Public images need no auth.
func (d *DockerClient) ImagePull(ctx context.Context, image string) error {
	fromImage, tag := splitImageRef(image)
	q := url.Values{}
	q.Set("fromImage", fromImage)
	if tag != "" {
		q.Set("tag", tag)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url("/images/create?"+q.Encode()), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Registry-Auth", "")
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &apiError{Status: resp.StatusCode, Message: decodeDockerMessage(b)}
	}
	// The stream is newline-delimited JSON; a message carrying "error" means the
	// pull failed even though the HTTP status was 200.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("image pull: %s", msg.Error)
		}
	}
}

// ImageExists reports whether an image reference is present locally.
func (d *DockerClient) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := d.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, &struct{}{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// ImageDigest returns an image's content digest (RepoDigests[0] when present,
// else the image Id), used to pin a deploy to an immutable image.
func (d *DockerClient) ImageDigest(ctx context.Context, ref string) (string, error) {
	var out struct {
		ID          string   `json:"Id"`
		RepoDigests []string `json:"RepoDigests"`
	}
	if err := d.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, &out); err != nil {
		return "", err
	}
	if len(out.RepoDigests) > 0 {
		return out.RepoDigests[0], nil
	}
	return out.ID, nil
}

// ImageSummary is the subset of GET /images/json each entry carries that image
// retention needs.
type ImageSummary struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Created  int64    `json:"Created"`
}

// ImageList returns images whose repo tag matches the reference filter (e.g.
// "sigmahub/res_x:*"), newest first — the input to keep-last-N retention.
func (d *DockerClient) ImageList(ctx context.Context, reference string) ([]ImageSummary, error) {
	filters := `{"reference":["` + reference + `"]}`
	var out []ImageSummary
	if err := d.do(ctx, http.MethodGet, "/images/json?filters="+url.QueryEscape(filters), nil, &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out, nil
}

// ImageRemove deletes an image by reference (a tag or id). A not-found removal is
// treated as success (already gone), so retention is idempotent.
func (d *DockerClient) ImageRemove(ctx context.Context, ref string, force bool) error {
	path := "/images/" + url.PathEscape(ref)
	if force {
		path += "?force=true"
	}
	err := d.do(ctx, http.MethodDelete, path, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// ImageBuild builds an image from a local context directory via the daemon's
// /build endpoint (BuildKit-backed when the daemon defaults to it). It tars the
// context, streams the build output to logs line-by-line, and fails if the build
// stream reports an error.
func (d *DockerClient) ImageBuild(ctx context.Context, contextDir, dockerfile, tag string, logs io.Writer) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarDir(contextDir, pw)) }()

	q := url.Values{}
	q.Set("t", tag)
	q.Set("dockerfile", dockerfile)
	q.Set("rm", "1")
	q.Set("forcerm", "1")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url("/build?"+q.Encode()), pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	// Ask the daemon to use BuildKit for the build.
	req.Header.Set("X-Registry-Config", "")
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &apiError{Status: resp.StatusCode, Message: decodeDockerMessage(b)}
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("image build: %s", msg.Error)
		}
		if msg.Stream != "" && logs != nil {
			_, _ = io.WriteString(logs, msg.Stream)
		}
	}
}

// tarDir writes a tar of a directory tree (regular files + dirs) to w. It is the
// Docker build context; symlinks and special files are skipped.
func tarDir(root string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil // skip symlinks/devices
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// splitImageRef separates a reference into (fromImage, tag) for /images/create.
// A digest reference stays whole (fromImage carries the @sha256:… and tag is
// empty); a tagged reference is split on the tag colon (guarding against a
// registry host:port colon).
func splitImageRef(image string) (string, string) {
	if strings.Contains(image, "@") {
		return image, ""
	}
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i+1:], "/") {
		return image[:i], image[i+1:]
	}
	return image, "latest"
}

// --- Containers ---

// ContainerState is the subset of a container inspect the driver needs.
type ContainerState struct {
	ID      string
	Name    string
	Image   string
	Running bool
	// Pid is the container's main-process host PID (0 when not running). Used to
	// seed secret files through the container's live mount namespace via
	// /proc/<pid>/root, which lands in the tmpfs and never on the host disk layer.
	Pid int
	// IP is the container's address on its (first) bridge network, so the agent
	// can health-probe a new container directly during a zero-downtime rollout.
	IP     string
	Labels map[string]string
}

type containerInspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Running bool `json:"Running"`
		Pid     int  `json:"Pid"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// firstIP returns a deterministic non-empty container IP (networks sorted by
// name) so the health probe targets a stable address.
func (ci containerInspect) firstIP() string {
	names := make([]string, 0, len(ci.NetworkSettings.Networks))
	for n := range ci.NetworkSettings.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if ip := ci.NetworkSettings.Networks[n].IPAddress; ip != "" {
			return ip
		}
	}
	return ""
}

// ContainerInspect returns (state, true, nil) when the named container exists,
// (_, false, nil) when it does not, or an error.
func (d *DockerClient) ContainerInspect(ctx context.Context, name string) (ContainerState, bool, error) {
	var ci containerInspect
	err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &ci)
	if isNotFound(err) {
		return ContainerState{}, false, nil
	}
	if err != nil {
		return ContainerState{}, false, err
	}
	return ContainerState{
		ID:      ci.ID,
		Name:    strings.TrimPrefix(ci.Name, "/"),
		Image:   ci.Config.Image,
		Running: ci.State.Running,
		Pid:     ci.State.Pid,
		IP:      ci.firstIP(),
		Labels:  ci.Config.Labels,
	}, true, nil
}

// ContainerList returns managed containers (those carrying LabelManaged=true),
// including stopped ones.
func (d *DockerClient) ContainerList(ctx context.Context) ([]ContainerState, error) {
	filters := fmt.Sprintf(`{"label":["%s=true"]}`, LabelManaged)
	q := url.Values{}
	q.Set("all", "true")
	q.Set("filters", filters)
	var raw []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Image  string            `json:"Image"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/json?"+q.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]ContainerState, 0, len(raw))
	for _, c := range raw {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, ContainerState{
			ID:      c.ID,
			Name:    name,
			Image:   c.Image,
			Running: c.State == "running",
			Labels:  c.Labels,
		})
	}
	return out, nil
}

// ContainerCreate creates a container from a fully-resolved create body and
// returns its id.
func (d *DockerClient) ContainerCreate(ctx context.Context, name string, body any) (string, error) {
	var res struct {
		ID string `json:"Id"`
	}
	err := d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body, &res)
	return res.ID, err
}

func (d *DockerClient) ContainerStart(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil)
}

// FSChange is one entry of a container's filesystem diff (docker diff).
type FSChange struct {
	Path string `json:"Path"`
	Kind int    `json:"Kind"` // 0=modified, 1=added, 2=deleted
}

// ContainerChanges returns the container's filesystem changes relative to its
// image (the docker-diff endpoint). Crucially, files written into a tmpfs mount
// do NOT appear here — the diff reports only the on-disk graphdriver (rw) layer.
// That makes it a portable disk-scan: a secret path showing up here would mean
// the value leaked onto host disk instead of staying in the in-memory tmpfs.
func (d *DockerClient) ContainerChanges(ctx context.Context, id string) ([]FSChange, error) {
	var out []FSChange
	if err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/changes", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ContainerStop stops a container with a grace period (seconds). A missing or
// already-stopped container is not an error.
func (d *DockerClient) ContainerStop(ctx context.Context, id string, grace time.Duration) error {
	secs := int(grace.Seconds())
	err := d.do(ctx, http.MethodPost, fmt.Sprintf("/containers/%s/stop?t=%d", url.PathEscape(id), secs), nil, nil)
	if isNotFound(err) {
		return nil
	}
	var ae *apiError
	if asAPIError(err, &ae) && ae.Status == http.StatusNotModified {
		return nil // already stopped
	}
	return err
}

// ContainerRemove force-removes a container. A missing container is not an error.
func (d *DockerClient) ContainerRemove(ctx context.Context, id string, force bool) error {
	path := "/containers/" + url.PathEscape(id)
	if force {
		path += "?force=true"
	}
	err := d.do(ctx, http.MethodDelete, path, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// ContainerExec runs cmd inside a running container and streams its stdout to
// out (stderr is captured into the returned tail for diagnostics). Returns the
// command's exit code. Used by the P1-11 backup ops to run engine-native dump
// and load tools inside the database's own container — the command comes from
// the agent's engine catalog, never from the DSD, preserving the
// no-generic-run-shell invariant.
func (d *DockerClient) ContainerExec(ctx context.Context, containerID string, cmd []string, out io.Writer) (exitCode int, stderrTail string, err error) {
	return d.containerExec(ctx, containerID, cmd, nil, out)
}

// ContainerExecEnv is ContainerExec with extra process environment for the exec
// ("KEY=value" entries). It lets a caller hand a secret to the command via the
// environment instead of argv, so the value never lands in the container's
// process cmdline (ps / /proc/*/cmdline) — only /proc/<pid>/environ, which ps
// does not surface (SIGMA-79).
func (d *DockerClient) ContainerExecEnv(ctx context.Context, containerID string, cmd, env []string, out io.Writer) (exitCode int, stderrTail string, err error) {
	return d.containerExec(ctx, containerID, cmd, env, out)
}

func (d *DockerClient) containerExec(ctx context.Context, containerID string, cmd, env []string, out io.Writer) (exitCode int, stderrTail string, err error) {
	var created struct {
		ID string `json:"Id"`
	}
	execBody := map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          false,
		"Cmd":          cmd,
	}
	if len(env) > 0 {
		execBody["Env"] = env
	}
	if err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(containerID)+"/exec", execBody, &created); err != nil {
		return -1, "", err
	}
	body, _ := json.Marshal(map[string]any{"Detach": false, "Tty": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url("/exec/"+created.ID+"/start"), bytes.NewReader(body))
	if err != nil {
		return -1, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return -1, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return -1, "", &apiError{Status: resp.StatusCode, Message: decodeDockerMessage(b)}
	}
	var stderr bytes.Buffer
	if err := demuxDockerStream(resp.Body, out, capWriter(&stderr, 4096)); err != nil {
		return -1, stderr.String(), fmt.Errorf("exec stream: %w", err)
	}
	var st struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	if err := d.do(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, &st); err != nil {
		return -1, stderr.String(), err
	}
	return st.ExitCode, stderr.String(), nil
}

// demuxDockerStream splits Docker's non-TTY attach framing (8-byte header:
// stream type, 3 padding bytes, 4-byte big-endian frame length) into stdout
// and stderr writers.
func demuxDockerStream(r io.Reader, stdout, stderr io.Writer) error {
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		length := int(hdr[4])<<24 | int(hdr[5])<<16 | int(hdr[6])<<8 | int(hdr[7])
		dst := stdout
		if hdr[0] == 2 {
			dst = stderr
		}
		if _, err := io.CopyN(dst, r, int64(length)); err != nil {
			return err
		}
	}
}

// capWriter bounds a diagnostics buffer so a chatty stderr can't balloon memory.
func capWriter(buf *bytes.Buffer, max int) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		if buf.Len() < max {
			room := max - buf.Len()
			if len(p) > room {
				buf.Write(p[:room])
			} else {
				buf.Write(p)
			}
		}
		return len(p), nil
	})
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// ContainerStatsOnce reads one non-streaming stats sample and returns the
// container's CPU utilisation (percent of one core, like `docker stats`) and
// memory usage in bytes. Used by the P1-13 telemetry collector.
func (d *DockerClient) ContainerStatsOnce(ctx context.Context, id string) (cpuPct float64, memBytes int64, err error) {
	var st struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs  int    `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage uint64 `json:"usage"`
			Stats struct {
				InactiveFile uint64 `json:"inactive_file"`
			} `json:"stats"`
		} `json:"memory_stats"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats?stream=false", nil, &st); err != nil {
		return 0, 0, err
	}
	cpuDelta := float64(st.CPUStats.CPUUsage.TotalUsage) - float64(st.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(st.CPUStats.SystemUsage) - float64(st.PreCPUStats.SystemUsage)
	cpus := st.CPUStats.OnlineCPUs
	if cpus <= 0 {
		cpus = 1
	}
	if cpuDelta > 0 && sysDelta > 0 {
		cpuPct = cpuDelta / sysDelta * float64(cpus) * 100.0
	}
	// Match docker stats: usage minus the reclaimable page cache.
	mem := st.MemoryStats.Usage
	if st.MemoryStats.Stats.InactiveFile < mem {
		mem -= st.MemoryStats.Stats.InactiveFile
	}
	return cpuPct, int64(mem), nil
}

// ContainerLogTail returns the last n lines a container wrote, without
// following. Used when a rollout's health gate fails: the container is about to
// be removed, and its output is the only evidence of WHY it never became
// healthy. Without this the deploy reports "health gate timed out" and the
// stack trace that explains it is destroyed along with the container.
//
// Errors are the caller's to ignore — a container that produced nothing, or one
// already gone, must not turn a health-gate failure into a different failure.
func (d *DockerClient) ContainerLogTail(ctx context.Context, id string, n int) ([]string, error) {
	if n <= 0 {
		n = 100
	}
	q := url.Values{}
	q.Set("stdout", "true")
	q.Set("stderr", "true")
	q.Set("tail", fmt.Sprintf("%d", n))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		d.url("/containers/"+url.PathEscape(id)+"/logs?"+q.Encode()), nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &apiError{Status: resp.StatusCode, Message: decodeDockerMessage(b)}
	}

	var lines []string
	collect := func(_ string, _ time.Time, line string) {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	stdout := &logLineSplitter{stream: "stdout", fn: collect}
	stderr := &logLineSplitter{stream: "stderr", fn: collect}
	// Bound the read: a container that logged megabytes before dying must not
	// be able to stall the rollout's failure path.
	if err := demuxDockerStream(io.LimitReader(resp.Body, 1<<20), stdout, stderr); err != nil {
		// Partial output is still worth reporting — return what was decoded.
		stdout.flush()
		stderr.flush()
		return lines, nil
	}
	stdout.flush()
	stderr.flush()
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// FollowContainerLogs tails a container's stdout/stderr from `since`, invoking
// fn per line with the Docker-stamped timestamp. Blocks until ctx is done, the
// container stops, or the stream errors. Used by the P1-13 log shipper.
func (d *DockerClient) FollowContainerLogs(ctx context.Context, id string, since time.Time, fn func(stream string, ts time.Time, line string)) error {
	q := url.Values{}
	q.Set("follow", "true")
	q.Set("stdout", "true")
	q.Set("stderr", "true")
	q.Set("timestamps", "true")
	q.Set("since", fmt.Sprintf("%d", since.Unix()))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url("/containers/"+url.PathEscape(id)+"/logs?"+q.Encode()), nil)
	if err != nil {
		return err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &apiError{Status: resp.StatusCode, Message: decodeDockerMessage(b)}
	}
	stdout := &logLineSplitter{stream: "stdout", fn: fn}
	stderr := &logLineSplitter{stream: "stderr", fn: fn}
	err = demuxDockerStream(resp.Body, stdout, stderr)
	stdout.flush()
	stderr.flush()
	return err
}

// logLineSplitter splits a demuxed log stream into timestamped lines. Docker
// prefixes each line with an RFC3339Nano timestamp when timestamps=true.
type logLineSplitter struct {
	stream string
	fn     func(stream string, ts time.Time, line string)
	buf    []byte
}

func (l *logLineSplitter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.emit(string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
	// Backstop: an absurdly long line without a newline is emitted in chunks
	// rather than growing the buffer without bound.
	if len(l.buf) > 64*1024 {
		l.emit(string(l.buf))
		l.buf = nil
	}
	return len(p), nil
}

func (l *logLineSplitter) flush() {
	if len(l.buf) > 0 {
		l.emit(string(l.buf))
		l.buf = nil
	}
}

func (l *logLineSplitter) emit(raw string) {
	ts := time.Now()
	line := raw
	if sp := strings.IndexByte(raw, ' '); sp > 0 {
		if parsed, err := time.Parse(time.RFC3339Nano, raw[:sp]); err == nil {
			ts = parsed
			line = raw[sp+1:]
		}
	}
	l.fn(l.stream, ts, line)
}

// PutArchive extracts a tar stream into a container at path. Content lands on
// the container's writable layer — callers must only ship non-secret payloads
// (P1-11 uses it to place dumps into throwaway verify/restore containers that
// are removed right after).
func (d *DockerClient) PutArchive(ctx context.Context, containerID, path string, tarStream io.Reader) error {
	u := d.url("/containers/" + url.PathEscape(containerID) + "/archive?path=" + url.QueryEscape(path))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, tarStream)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &apiError{Status: resp.StatusCode, Message: decodeDockerMessage(b)}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ImagePush publishes a locally built image to its registry, streaming the
// progress into logs. Used by a dedicated build server: the deploy target
// cannot read this host's Docker daemon, so the image has to travel through a
// registry both can see.
//
// Like ImagePull, the HTTP status alone does not mean success — the response is
// a newline-delimited JSON stream and a message carrying "error" means the push
// failed under a 200.
func (d *DockerClient) ImagePush(ctx context.Context, ref string, auth build.RegistryAuth, logs io.Writer) error {
	name, tag := splitImageRef(ref)
	q := url.Values{}
	if tag != "" {
		q.Set("tag", tag)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		d.url("/images/"+url.PathEscape(name)+"/push?"+q.Encode()), nil)
	if err != nil {
		return err
	}
	// Docker requires the header to be present even for an anonymous push; an
	// empty value means "no credentials". A hosted registry answers that with a
	// 401, so a real credential goes here whenever we have one.
	encoded := "e30=" // base64("{}")
	if auth.Username != "" || auth.Password != "" {
		blob, merr := json.Marshal(auth)
		if merr != nil {
			return merr
		}
		encoded = base64.URLEncoding.EncodeToString(blob)
	}
	req.Header.Set("X-Registry-Auth", encoded)
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &apiError{Status: resp.StatusCode, Message: decodeDockerMessage(b)}
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("push %s: %s", ref, msg.Error)
		}
		if logs != nil && msg.Status != "" {
			_, _ = io.WriteString(logs, msg.Status+"\n")
		}
	}
}
