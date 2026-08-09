package api

// The onboarding installer, served by the control plane.
//
// The connect-server wizard renders one line an operator pastes into a root
// shell, and until SIGMA-217 every URL in it pointed at github.com: install.sh
// came from a release asset, and install.sh then fetched five more (the sigmad
// archive, checksums.txt, its .sig and .pem, and sigmad.service) from the same
// place. All six are unauthenticated curls, so on a PRIVATE release repository
// all six 404 and not one server can be onboarded. The only workaround the
// operator had was to make the repository public, which is not a fix.
//
// So the control plane serves them instead, authenticating to GitHub with a
// credential that stays on this side (CP_RELEASE_TOKEN). The host then talks to
// exactly one machine — the control plane it must already be able to reach —
// and no credential appears in a command anyone pastes into a terminal.
//
// # This does not weaken the trust model, and that is the first question to ask
//
// install.sh verifies checksums.txt with cosign against the release workflow's
// keyless OIDC identity, and verifies the archive and the systemd unit against
// that checksums.txt, before it executes anything. Authenticity comes from that
// signature, never from who served the bytes. A signed artifact handed over by a
// proxy is still the signed artifact; a control plane that tampered with one
// would fail verification on the host exactly as a tampered github.com would.
// What the proxy changes is REACHABILITY, which was the whole defect.
//
// # These two routes are unauthenticated, and that is not an oversight
//
// A host being onboarded holds no credential yet — that is what onboarding is
// for. The bootstrap token in the rendered command belongs to the AGENT and is
// spent registering it; making a download depend on it would mean the token has
// to survive a curl, and it would put a one-time secret in the query string of
// six requests. So the routes are open, and every bound below exists because
// they are.
//
// # The security surface
//
// This is an unauthenticated endpoint holding a credential to a private
// repository. The version and the asset name arrive from the caller, so the
// naive shape of this handler — clean the path, join it onto the release URL,
// stream whatever comes back — is a directory traversal into a private
// repository with a token attached, which is a strictly worse product than the
// public repository the operator was forced into. Hence: the asset name is an
// ALLOWLIST rather than a sanitised string (allowedAsset), the version must
// match the release-tag shape (releaseTagPattern), and neither is ever
// concatenated into a URL before both have passed.
//
// # Rate limiting: deliberately not here
//
// These routes are not rate limited in this process, and the reasoning is worth
// having written down because it is a decision, not an omission.
//
// Nothing here touches the database, the secret store or the KMS: a request
// costs one or two outbound HTTP calls and a streamed copy, both bounded below
// (MaxBytes, and the client timeout). The exposure that remains is egress and
// this control plane's GitHub budget — with a token, 5000 API requests an hour
// shared by the whole process.
//
// A limiter here would have to refuse requests to protect that budget, and the
// request it refuses is a server being onboarded: a 503 in the middle of a fleet
// rollout is the exact failure this change exists to remove, traded for a burst
// that costs bandwidth. The control plane already sits behind a reverse proxy or
// CDN in every deployment that has a PublicURL, and that edge can rate limit
// /install.sh and /dl/ by source address without knowing anything about
// releases. That is where the limit belongs; an operator who wants one should
// put it there, at roughly ten requests a minute per address (one onboarding is
// six).
//
// # Caching: deliberately not here either
//
// A release tag is immutable, so caching (repo, tag, asset) looks free. It is
// not. A correct cache needs a size-accounted, evictable store — the assets are
// tens of megabytes, so an unbounded map is the memory-exhaustion primitive
// MaxBytes exists to prevent — plus a decision about writing a private
// repository's contents to this host's disk, which nothing in the control plane
// does today and which has no retention story. Against that: the fetch happens
// once per host onboarded, not once per request per user. A wrong cache would
// not be a security bug (the host still cosign-verifies) but it would be a
// silent one, and a stale asset is much harder to debug than a re-fetch.
//
// So: no cache, and no Cache-Control header inviting an intermediary to build
// one for us — these bytes may come out of a repository the operator chose to
// keep private, and `public, max-age=...` would put them in every proxy between
// here and the host. If the egress ever matters, the answer is a CDN in front of
// the control plane keyed on the immutable URL, not a bespoke cache in here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

const (
	// defaultDownloadBase and defaultAPIBase are github.com and its REST API.
	// They are fields on ReleaseSource rather than constants referenced
	// directly so tests can point at an httptest server and a self-hoster can
	// point at GitHub Enterprise.
	defaultDownloadBase = "https://github.com"
	defaultAPIBase      = "https://api.github.com"

	// defaultMaxAssetBytes caps one proxied asset. The largest thing on the
	// list is the sigmad archive — a static Go binary, tens of megabytes
	// compressed — so 64 MiB is generous for what the release actually
	// publishes while still being a BOUND, which is the point: an
	// unauthenticated endpoint that streams whatever an upstream sends is a
	// memory- and bandwidth-exhaustion primitive wearing a download's clothes.
	defaultMaxAssetBytes = 64 << 20

	// maxReleaseJSONBytes caps the release metadata read on the authenticated
	// path. A release listing is a few kilobytes of JSON per asset; 4 MiB is
	// four hundred assets' worth and still a bound.
	maxReleaseJSONBytes = 4 << 20

	// releaseMetadataTimeout bounds the release lookup specifically. It is a
	// small JSON GET on the critical path of an operator's curl, so it does not
	// get the whole-fetch budget the asset body does.
	releaseMetadataTimeout = 15 * time.Second

	// releaseFetchTimeout bounds one whole proxied fetch, body included.
	//
	// It is generous on purpose, and the reason is that this handler STREAMS:
	// it copies the upstream body straight to the operator's curl rather than
	// buffering it, so the operator's own link speed is on this clock too.
	// Buffering would take it off, and buffering tens of megabytes per
	// concurrent request on an unauthenticated route is precisely what MaxBytes
	// exists to prevent — so the clock stays generous instead. Five minutes is
	// a 68 KB/s floor for a 20 MB archive; below that the install was not going
	// to finish anyway, and the timeout releases the goroutine rather than
	// pinning it on a stalled connection forever.
	releaseFetchTimeout = 5 * time.Minute
)

// releaseTagPattern is the shape a version segment must have before it is
// allowed anywhere near an upstream URL: a goreleaser tag, optionally with a
// prerelease suffix (the release workflow sets `prerelease: auto`).
//
// This is a gate, not a tidy-up. `..`, `%2e%2e`, a slash, a query string and an
// absolute URL all fail it, because every one of them requires a character this
// pattern does not admit — which is why the handler validates and refuses
// rather than cleaning and continuing. RE2 matches in linear time, so an
// attacker-supplied segment cannot make the check itself expensive.
var releaseTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// fixedAssets is the version-independent half of the allowlist, mapped to the
// content type the control plane serves each as. Being a map from the exact
// name is what makes it an allowlist: there is no path to construct, no prefix
// to match and no directory to escape.
//
// sigmad.service is on this list even though the reported bug named only five
// URLs. install.sh fetches it too, and falls back to an EMBEDDED unit when the
// fetch fails — so leaving it off would not 404 anything an operator could see;
// it would silently downgrade every proxied install to the unsigned fallback
// unit and quietly undo SIGMA-155, which exists so a root ExecStart is checked
// against the cosign-signed checksums.txt. installer_test.go reads install.sh
// off disk and fails if this list stops covering what the script fetches.
var fixedAssets = map[string]string{
	"install.sh":        "text/x-shellscript; charset=utf-8",
	"checksums.txt":     "text/plain; charset=utf-8",
	"checksums.txt.sig": "text/plain; charset=utf-8",
	"checksums.txt.pem": "application/x-pem-file",
	"sigmad.service":    "text/plain; charset=utf-8",
}

// installScriptAsset is the release asset GET /install.sh serves.
const installScriptAsset = "install.sh"

// allowedAsset reports whether asset may be proxied out of release `version`,
// and the content type to serve it as.
//
// The archive name is checked against the name the release ACTUALLY publishes
// for this version — sigmad_<version-without-v>_linux_<arch>.tar.gz, for the
// architectures a host can enroll on at all — rather than against a pattern with
// a free version field. Matching loosely there would put an
// attacker-chosen-but-plausible segment back into the upstream URL for no
// benefit, since a v0.3.0 release has never held a v9.9.9 archive.
//
// The architectures come from the server catalog rather than being retyped
// here. That list is already pinned to agent/packaging/install.sh (and through
// it to selfupdate.SupportedArches and the goreleaser build matrix) by
// store/installer_vocabulary_test.go, so an architecture added to the release
// becomes proxyable without anyone remembering this file, and one removed stops
// being proxyable the same way.
func allowedAsset(version, asset string) (contentType string, ok bool) {
	if ct, found := fixedAssets[asset]; found {
		return ct, true
	}
	for _, arch := range store.SupportedArches() {
		if asset == archiveName(version, arch) {
			return "application/gzip", true
		}
	}
	return "", false
}

func archiveName(version, arch string) string {
	return "sigmad_" + strings.TrimPrefix(version, "v") + "_linux_" + arch + ".tar.gz"
}

// allowedAssetNames renders the allowlist for a version, so the refusal message
// lists what this route will serve instead of only what it will not. Every name
// on it is public information (it is the release's own asset list), so naming
// them costs nothing and saves an operator a support round trip.
func allowedAssetNames(version string) []string {
	names := make([]string, 0, len(fixedAssets)+2)
	for name := range fixedAssets {
		names = append(names, name)
	}
	for _, arch := range store.SupportedArches() {
		names = append(names, archiveName(version, arch))
	}
	// Sorted because the map range above is not: two operators comparing the
	// same refusal should be reading the same sentence.
	slices.Sort(names)
	return names
}

// ReleaseSource is where the installer routes fetch from, and the only place in
// the control plane that holds the release credential.
//
// Repo empty means the routes are not configured and answer 503 — there is no
// fallback repository spelled in this package, because a second spelling of the
// slug is a second thing to keep in step with config.CP_RELEASE_REPO.
type ReleaseSource struct {
	// Repo is the "owner/name" of the repository whose releases hold the
	// installer and the sigmad assets.
	Repo string
	// Version is the release tag GET /install.sh serves. Empty or unreleased
	// (a `dev` build stamp) makes that route answer 503 naming the setting;
	// /dl/{version}/{asset} carries its own version and is unaffected.
	Version string
	// Token is the server-side GitHub credential. Empty is a SUPPORTED
	// configuration, not a degraded one: a public release repository — the
	// upstream one, or a self-hoster's own public fork — is proxied
	// unauthenticated and needs no configuration at all.
	//
	// It is never written to a response, an error body or a log line. See
	// upstreamError, which names the SETTING and never its value.
	Token string
	// DownloadBase and APIBase are github.com and its REST API; overridable for
	// tests and for GitHub Enterprise.
	DownloadBase string
	APIBase      string
	// MaxBytes caps one proxied asset (defaultMaxAssetBytes when zero).
	MaxBytes int64
	// HTTP is the outbound client; the default carries releaseFetchTimeout,
	// which http.DefaultClient does not have.
	HTTP *http.Client
}

func (rs ReleaseSource) normalized() ReleaseSource {
	if rs.DownloadBase == "" {
		rs.DownloadBase = defaultDownloadBase
	}
	if rs.APIBase == "" {
		rs.APIBase = defaultAPIBase
	}
	if rs.MaxBytes <= 0 {
		rs.MaxBytes = defaultMaxAssetBytes
	}
	if rs.HTTP == nil {
		rs.HTTP = &http.Client{Timeout: releaseFetchTimeout}
	}
	rs.DownloadBase = strings.TrimRight(rs.DownloadBase, "/")
	rs.APIBase = strings.TrimRight(rs.APIBase, "/")
	rs.Repo = strings.Trim(strings.TrimSpace(rs.Repo), "/")
	rs.Token = strings.TrimSpace(rs.Token)
	return rs
}

// handleInstallScript serves the installer for the version this control plane is
// pinned to. This is the URL the wizard renders, and the shape install.sh's own
// header documents: `curl -fsSL https://<host>/install.sh`.
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	if s.release.Repo == "" {
		s.writeInstallerError(w, http.StatusServiceUnavailable,
			"this control plane is not configured to serve the agent installer. Set CP_RELEASE_REPO to the owner/name of the repository whose releases publish sigmad.")
		return
	}
	if !releaseTagPattern.MatchString(s.release.Version) {
		// A source build stamps main.version as "dev", so this is what a
		// self-hoster who has not pinned an agent version sees. Name both ways
		// out: the setting, and the URL that carries its own version.
		s.writeInstallerError(w, http.StatusServiceUnavailable, fmt.Sprintf(
			"this control plane is not pinned to a released agent version (it has %q). Set CP_AGENT_VERSION to a released tag such as v0.3.0, or fetch the version-explicit URL /dl/<version>/install.sh.",
			s.release.Version))
		return
	}
	s.proxyReleaseAsset(w, r, s.release.Version, installScriptAsset)
}

// handleReleaseAsset serves one pinned release asset. install.sh points its
// SIGMAHUB_DOWNLOAD_BASE at /dl/{version}, so this is where the four downloads
// it makes after cosign is installed — and the systemd unit — come from.
func (s *Server) handleReleaseAsset(w http.ResponseWriter, r *http.Request) {
	if s.release.Repo == "" {
		s.writeInstallerError(w, http.StatusServiceUnavailable,
			"this control plane is not configured to serve release assets. Set CP_RELEASE_REPO to the owner/name of the repository whose releases publish sigmad.")
		return
	}
	version := r.PathValue("version")
	asset := r.PathValue("asset")
	// Both refusals happen BEFORE any outbound request exists, so a probe
	// cannot use this route to make the control plane spend its GitHub budget,
	// and cannot use timing to learn what a private repository holds.
	if !releaseTagPattern.MatchString(version) {
		s.log.Warn("release proxy refused a version", "version", version, "asset", asset)
		s.writeInstallerError(w, http.StatusNotFound, fmt.Sprintf(
			"%q is not a release tag. The version segment must be a published tag such as v0.3.0.", version))
		return
	}
	if _, ok := allowedAsset(version, asset); !ok {
		s.log.Warn("release proxy refused an asset", "version", version, "asset", asset)
		s.writeInstallerError(w, http.StatusNotFound, fmt.Sprintf(
			"%q is not an asset this control plane serves. Release %s publishes: %s.",
			asset, version, strings.Join(allowedAssetNames(version), ", ")))
		return
	}
	s.proxyReleaseAsset(w, r, version, asset)
}

// proxyReleaseAsset streams one allowlisted asset to the caller. Both callers
// have already validated version and asset; this function assumes that and does
// not re-derive it, so there is exactly one place where the allowlist is
// enforced and it is upstream of the URL being built.
func (s *Server) proxyReleaseAsset(w http.ResponseWriter, r *http.Request, version, asset string) {
	contentType, ok := allowedAsset(version, asset)
	if !ok {
		// Unreachable via either route; kept so a future caller cannot skip the
		// allowlist by calling this directly.
		s.writeInstallerError(w, http.StatusNotFound, fmt.Sprintf("%q is not an asset this control plane serves.", asset))
		return
	}
	rs := s.release
	body, size, err := rs.open(r.Context(), version, asset)
	if err != nil {
		var rerr *releaseError
		if errors.As(err, &rerr) {
			// The repository slug goes to the LOG, not the body: it is not a
			// credential, but an anonymous caller has no business learning the
			// name of a private repository, and the operator debugging this can
			// read the control plane's logs.
			s.log.Warn("release proxy failed",
				"repo", rs.Repo, "version", version, "asset", asset,
				"status", rerr.status, "upstream_status", rerr.upstream)
			s.writeInstallerError(w, rerr.status, rerr.msg)
			return
		}
		s.log.Error("release proxy could not reach github",
			"repo", rs.Repo, "version", version, "asset", asset, "err", err)
		s.writeInstallerError(w, http.StatusBadGateway,
			"the control plane could not reach GitHub to fetch this release asset. Check the control plane's outbound network access and retry.")
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", contentType)
	// The content type is ours, from the allowlist, not the upstream's guess —
	// so tell the browser not to second-guess it either.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)

	// The cap is enforced a second time here even though open() already checked
	// the declared size, because the declaration is the upstream's and the
	// stream is the thing that costs memory. Truncating produces a short body
	// against a sent Content-Length, which fails the operator's curl AND the
	// host's checksum verification — the two outcomes that are safe. There is
	// no honest status code left to send at this point, so the log line is how
	// an operator finds out.
	n, err := io.Copy(w, io.LimitReader(body, rs.MaxBytes))
	if n == rs.MaxBytes {
		// Reaching the cap is not the same as exceeding it — an asset of
		// exactly MaxBytes was served whole. Ask the upstream for one more byte
		// before crying truncation, so the log line stays worth believing.
		var probe [1]byte
		if more, _ := body.Read(probe[:]); more > 0 {
			s.log.Error("release asset exceeded the proxy cap and was truncated; the host will refuse it at checksum verification",
				"repo", rs.Repo, "version", version, "asset", asset, "cap_bytes", rs.MaxBytes)
			return
		}
	}
	if err != nil {
		s.log.Warn("release asset stream ended early",
			"repo", rs.Repo, "version", version, "asset", asset, "bytes", n, "err", err)
	}
}

// writeInstallerError answers these two routes' failures as plain text whose
// every line is a shell comment, followed by `exit 1`.
//
// That looks like decoration and is not. The wizard renders
// `curl -fsSL <host>/install.sh | sudo bash`, where -f makes curl discard a
// 4xx/5xx body — but the moment anyone drops -f (a hand-typed retry, a command
// copied out of a screenshot, a shell that mangled the flags) the body of this
// response IS the script root executes. A JSON error object piped into bash is
// a pile of syntax errors run as root; the same sentence commented out is a
// no-op that exits non-zero, which is what -f was there to produce anyway.
func (s *Server) writeInstallerError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	for _, line := range strings.Split(msg, "\n") {
		_, _ = fmt.Fprintf(w, "# sigmahub: %s\n", line)
	}
	_, _ = fmt.Fprintln(w, "exit 1")
}

// releaseError is an upstream failure already translated into what the operator
// should see: the status this control plane will answer with, and a sentence
// naming the fix. upstream is carried separately for the log, so the two never
// have to be reconstructed from each other.
type releaseError struct {
	status   int
	upstream int
	msg      string
}

func (e *releaseError) Error() string { return e.msg }

// open fetches one asset, returning its body and declared size.
//
// There are two fetch paths because GitHub offers exactly one correct answer
// for each case and they are different endpoints:
//
//   - With no token, the browser download URL. It is unauthenticated and
//     unmetered, which is what a public release wants. Sending it through the
//     REST API instead would put every install under the anonymous API's 60
//     requests an hour — one onboarding is six requests, so a public repository
//     would start failing at ten hosts an hour for no reason at all.
//   - With a token, the REST API. The browser download URL does NOT accept
//     `Authorization` for a private repository (it answers 404 as if the
//     release did not exist), so the API's release-by-tag lookup followed by
//     the asset-by-id download is the only shape that works — and it is the
//     entire reason this file exists.
func (rs ReleaseSource) open(ctx context.Context, version, asset string) (io.ReadCloser, int64, error) {
	if rs.Token == "" {
		return rs.openPublic(ctx, version, asset)
	}
	return rs.openAuthenticated(ctx, version, asset)
}

func (rs ReleaseSource) openPublic(ctx context.Context, version, asset string) (io.ReadCloser, int64, error) {
	u := fmt.Sprintf("%s/%s/releases/download/%s/%s",
		rs.DownloadBase, rs.Repo, url.PathEscape(version), url.PathEscape(asset))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := rs.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		drain(resp)
		return nil, 0, upstreamError(resp.StatusCode, version, asset)
	}
	if resp.ContentLength > rs.MaxBytes {
		drain(resp)
		return nil, 0, &releaseError{status: http.StatusBadGateway, upstream: resp.StatusCode, msg: oversizedMessage(asset, rs.MaxBytes)}
	}
	return resp.Body, resp.ContentLength, nil
}

// releaseAsset is the slice of GitHub's release JSON this file reads. The
// numeric id is what the download URL is built FROM; the `url` GitHub returns
// alongside it is deliberately ignored, because following a URL an upstream
// chose — with this control plane's credential attached — is a server-side
// request forgery waiting for a compromised or misconfigured upstream. Building
// it from the id keeps the host fixed at APIBase.
type releaseAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func (rs ReleaseSource) openAuthenticated(ctx context.Context, version, asset string) (io.ReadCloser, int64, error) {
	meta, err := rs.releaseAssets(ctx, version, asset)
	if err != nil {
		return nil, 0, err
	}
	var want *releaseAsset
	for i := range meta {
		if meta[i].Name == asset {
			want = &meta[i]
			break
		}
	}
	if want == nil {
		// The release exists and does not publish this asset. That is the same
		// answer as "no such release" to the caller, and the same fixes apply.
		return nil, 0, upstreamError(http.StatusNotFound, version, asset)
	}
	// Refuse on the DECLARED size, before a byte is transferred. This is the
	// cheap half of the cap; the stream is capped again in proxyReleaseAsset
	// because a declaration is not a guarantee.
	if want.Size > rs.MaxBytes {
		return nil, 0, &releaseError{status: http.StatusBadGateway, upstream: http.StatusOK, msg: oversizedMessage(asset, rs.MaxBytes)}
	}

	u := fmt.Sprintf("%s/repos/%s/releases/assets/%d", rs.APIBase, rs.Repo, want.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	// application/octet-stream is what turns this endpoint from "describe the
	// asset" into "download it": GitHub answers with a redirect to a
	// short-lived signed URL on its object host. net/http follows it and drops
	// the Authorization header on the cross-domain hop, which is both what the
	// signed URL requires (it rejects a request carrying two auth mechanisms)
	// and what we would want anyway — the credential is for api.github.com and
	// nowhere else.
	req.Header.Set("Accept", "application/octet-stream")
	rs.authenticate(req)
	resp, err := rs.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		drain(resp)
		return nil, 0, upstreamError(resp.StatusCode, version, asset)
	}
	size := resp.ContentLength
	if size <= 0 {
		// The release metadata already told us, and it is the more reliable of
		// the two: the signed-URL hop can answer without a Content-Length.
		size = want.Size
	}
	if size > rs.MaxBytes {
		drain(resp)
		return nil, 0, &releaseError{status: http.StatusBadGateway, upstream: resp.StatusCode, msg: oversizedMessage(asset, rs.MaxBytes)}
	}
	return resp.Body, size, nil
}

// releaseAssets reads the asset listing of one release by tag.
func (rs ReleaseSource) releaseAssets(ctx context.Context, version, asset string) ([]releaseAsset, error) {
	ctx, cancel := context.WithTimeout(ctx, releaseMetadataTimeout)
	defer cancel()

	u := fmt.Sprintf("%s/repos/%s/releases/tags/%s", rs.APIBase, rs.Repo, url.PathEscape(version))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	rs.authenticate(req)
	resp, err := rs.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, upstreamError(resp.StatusCode, version, asset)
	}
	var payload struct {
		Assets []releaseAsset `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseJSONBytes)).Decode(&payload); err != nil {
		return nil, &releaseError{status: http.StatusBadGateway, upstream: resp.StatusCode,
			msg: "GitHub returned a release listing this control plane could not read. Retry; if it persists, check that CP_RELEASE_REPO names a GitHub repository and CP_RELEASE_TOKEN is a GitHub token."}
	}
	return payload.Assets, nil
}

// authenticate attaches the release credential. It is the only place the token
// is read, and it is written to a request header and nowhere else — never to a
// URL (where it would land in every proxy log between here and GitHub), never
// to a response, never to a log line.
func (rs ReleaseSource) authenticate(req *http.Request) {
	if rs.Token != "" {
		req.Header.Set("Authorization", "Bearer "+rs.Token)
	}
}

// upstreamError translates GitHub's answer into the one this control plane
// gives, with a sentence naming the likely cause. Every message names a SETTING
// and never a value: the token must not appear in a body, an error or a log,
// and the repository slug is withheld from anonymous callers on purpose (see
// proxyReleaseAsset).
func upstreamError(status int, version, asset string) *releaseError {
	switch status {
	case http.StatusNotFound:
		// Answered honestly as a 404: GitHub has no such asset, and neither do
		// we. The three causes below are, in order, what this actually turns
		// out to be in practice.
		return &releaseError{status: http.StatusNotFound, upstream: status, msg: fmt.Sprintf(
			"%s is not published in release %s. Likely causes: this control plane is pinned to a version that was never released (CP_AGENT_VERSION), the release exists but is still a draft, or CP_RELEASE_TOKEN does not have contents:read on the release repository — a private repository answers 404 rather than 403 when the credential cannot see it.",
			asset, version)}
	case http.StatusUnauthorized, http.StatusForbidden:
		// NOT passed through. A 403 here would tell the operator that THEY are
		// forbidden, and they are not — the control plane is. 502 is the honest
		// status for "my upstream refused me", and the sentence says whose
		// credential is at fault.
		return &releaseError{status: http.StatusBadGateway, upstream: status, msg: fmt.Sprintf(
			"GitHub refused this control plane's release credential while fetching %s. Set CP_RELEASE_TOKEN to a token with read access to the release repository's contents (a fine-grained token needs the Contents permission; a classic token needs the repo scope).",
			asset)}
	case http.StatusTooManyRequests:
		return &releaseError{status: http.StatusServiceUnavailable, upstream: status, msg: fmt.Sprintf(
			"GitHub rate-limited this control plane while fetching %s. Retry in a few minutes; setting CP_RELEASE_TOKEN raises the limit from 60 requests an hour to 5000.",
			asset)}
	default:
		return &releaseError{status: http.StatusBadGateway, upstream: status, msg: fmt.Sprintf(
			"GitHub answered %d while fetching %s. This is an upstream failure, not a configuration error; retry, and check https://www.githubstatus.com if it persists.",
			status, asset)}
	}
}

func oversizedMessage(asset string, maxBytes int64) string {
	return fmt.Sprintf(
		"%s is larger than the %d MiB this control plane will proxy, so it was refused rather than streamed. If the release genuinely publishes an asset this large, raise the cap in the control plane rather than removing it.",
		asset, maxBytes>>20)
}

// drain closes an upstream response, reading a bounded amount first so the
// connection can be reused. The body is NEVER forwarded: GitHub's error bodies
// are JSON that would be piped into a root shell by an install command that
// lost its -f, and they can echo request details back at us.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}
