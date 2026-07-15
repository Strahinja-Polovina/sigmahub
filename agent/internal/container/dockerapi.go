package container

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
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

// --- Volumes ---

func (d *DockerClient) VolumeExists(ctx context.Context, name string) (bool, error) {
	err := d.do(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, &struct{}{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
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
	ID     string
	Name   string
	Image  string
	Running bool
	Labels map[string]string
}

type containerInspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
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
