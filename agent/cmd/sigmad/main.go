// sigmad is the SigmaHub host agent: outbound-only, registers once with a
// bootstrap token, then heartbeats the control plane.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/backup"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/build"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/client"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/container"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/facts"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/host"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/mesh"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/metrics"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/state"
)

// version is stamped at release time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sigmad"
	}
	return filepath.Join(home, ".sigmad")
}

func run() error {
	var (
		endpoint   = flag.String("endpoint", envOr("SIGMAD_ENDPOINT", "http://localhost:8080"), "control plane base URL")
		bootstrap  = flag.String("bootstrap-token", os.Getenv("SIGMAD_BOOTSTRAP_TOKEN"), "one-time bootstrap token (first run only)")
		dataDir    = flag.String("data-dir", envOr("SIGMAD_DATA_DIR", defaultDataDir()), "directory for persisted identity")
		interval   = flag.Duration("interval", 30*time.Second, "heartbeat interval")
		name       = flag.String("name", "", "server display name (defaults to hostname)")
		wgUp       = flag.Bool("wg-up", false, "apply the rendered WireGuard config via wg-quick (Linux only; default renders config only)")
		dockerSock = flag.String("docker-socket", envOr("SIGMAD_DOCKER_SOCKET", "/var/run/docker.sock"), "Docker Engine unix socket")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := client.New(*endpoint)

	// Mesh identity exists before registration so the pubkey rides along on
	// the very first request. The private key never leaves the data dir.
	meshPriv, meshPub, err := mesh.LoadOrCreateKey(*dataDir)
	if err != nil {
		return err
	}

	st, ok, err := state.Load(*dataDir)
	if err != nil {
		return err
	}
	if !ok {
		if *bootstrap == "" {
			return errors.New("no persisted identity and no --bootstrap-token given")
		}
		f, _ := json.Marshal(facts.Collect())
		hostname := *name
		if hostname == "" {
			hostname, _ = os.Hostname()
		}
		res, err := c.Register(ctx, client.RegisterRequest{
			BootstrapToken: *bootstrap,
			Name:           hostname,
			AgentVersion:   version,
			Facts:          f,
			Pubkey:         meshPub,
		})
		if err != nil {
			return err
		}
		st = state.State{ServerID: res.ServerID, AgentToken: res.AgentToken, DSDPublicKey: res.DSDPublicKey}
		if err := state.Save(*dataDir, st); err != nil {
			return err
		}
		log.Info("registered", "server_id", st.ServerID, "data_dir", *dataDir)
	} else {
		log.Info("resuming identity", "server_id", st.ServerID)
	}

	// DSD apply subsystem: open the crash-safe journal and register the op
	// handlers. P1-2 ships only the stub "resource.sync"; P1-3 registers the
	// container ops behind this same registry (the no-shell enforcement point).
	journal, err := apply.Open(*dataDir)
	if err != nil {
		return err
	}
	defer journal.Close()
	registry := apply.NewRegistry()
	// resource.sync stays as a no-op for resource kinds not yet containerised
	// (databases etc. land in P1-10); "app" resources render into the container
	// ops registered by the Docker driver below.
	registry.Register("resource.sync", func(context.Context, dsd.Op) error { return nil })

	// Container runtime (P1-3): the Docker driver registers the typed container
	// op kinds and owns actual state. Opening the client is lazy (no connection
	// until a call), so a host without Docker still runs — its container ops
	// simply fail and are reported as such. The desired-state store lets the
	// reconcile loop repair drift with no control-plane round-trip.
	cstore, err := container.OpenStore(*dataDir)
	if err != nil {
		return err
	}
	defer cstore.Close()
	docker := container.NewDockerClient(*dockerSock, os.Getenv("DOCKER_HOST"))
	// The driver resolves a resource's secrets from the CP at container-create
	// over the authenticated agent channel.
	secretFetcher := func(ctx context.Context, resourceID string) ([]container.Secret, error) {
		res, err := c.FetchSecrets(ctx, st.AgentToken, resourceID)
		if err != nil {
			return nil, err
		}
		out := make([]container.Secret, 0, len(res.Secrets))
		for _, s := range res.Secrets {
			out = append(out, container.Secret{Name: s.Name, Value: s.Value, EnvVar: s.EnvVar})
		}
		return out, nil
	}
	driver := container.NewDriver(docker, cstore, log, secretFetcher)
	driver.Register(registry)
	// Git deploy build path (P1-9): git.clone + image.build behind the same
	// registry (the no-shell enforcement point). The clone credential is fetched
	// into memory per deployment (never persisted); build/orchestration logs stream
	// to the CP for the deploy view. workRoot holds per-resource build contexts.
	buildRoot := filepath.Join(*dataDir, "build")
	builder := build.NewBuilder(
		docker,
		func(ctx context.Context, credentialRef string) (string, error) {
			// credentialRef is the deployment id; the CP scopes the credential to
			// this server. A public repo carries no ref and never reaches here.
			res, err := c.FetchCloneCredential(ctx, st.AgentToken, credentialRef)
			if err != nil {
				return "", err
			}
			return res.Token, nil
		},
		buildRoot,
		log,
		func(ctx context.Context, deploymentID, stream, line string) {
			if deploymentID == "" {
				return
			}
			if err := c.PostBuildLog(ctx, st.AgentToken, deploymentID, stream, []string{line}); err != nil {
				log.Warn("build log post failed", "err", err)
			}
		},
	)
	builder.Register(registry)
	// Backup ops (P1-11): engine-native dump → restic (client-side encrypted),
	// restic check + GFS retention, restore-verify into a scratch container, and
	// the fire-drill restore. The repo key + S3 credentials are fetched per run
	// over the authenticated channel (audited CP-side) and live only in the
	// restic child process env.
	backupRunner := backup.NewRunner(
		docker,
		func(ctx context.Context, runID string) (backup.Credential, error) {
			res, err := c.FetchBackupCredential(ctx, st.AgentToken, runID)
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.Status == 404 {
				return backup.Credential{}, backup.ErrRunSettled
			}
			if err != nil {
				return backup.Credential{}, err
			}
			return backup.Credential{
				Repository: res.Repository,
				RepoKey:    res.RepoKey,
				AccessKey:  res.AccessKey,
				SecretKey:  res.SecretKey,
				Region:     res.Region,
			}, nil
		},
		func(ctx context.Context, runID string, ok bool, snapshotID, dumpSha, detail string) {
			if err := c.PostBackupResult(ctx, st.AgentToken, runID, ok, snapshotID, dumpSha, detail); err != nil {
				log.Warn("backup result post failed", "err", err, "run", runID)
			}
		},
		filepath.Join(*dataDir, "backup"),
		log,
	)
	backupRunner.Register(registry)
	// Host-hardening ops (P1-5): nftables / sshd / CIS. These mutate the host
	// itself and require root; the handlers fail cleanly (reported as a failed op)
	// when sigmad is not root. Registered behind the same apply registry, so an
	// unregistered host op kind is still rejected.
	hostDriver := host.NewDriver()
	hostDriver.Register(registry)
	if avail, ver := container.Probe(ctx, docker); avail {
		log.Info("docker runtime available", "version", ver)
	} else {
		log.Warn("docker runtime unavailable; container ops will fail until a daemon is reachable", "socket", *dockerSock)
	}
	// Actual-state reconcile: converge managed containers every 30s (well under
	// the 60s drift-repair SLO); survives control-plane outages.
	go driver.RunReconcile(ctx, 30*time.Second)

	// The DSD loop runs alongside the heartbeat loop, outbound-only. After each
	// applied DSD it garbage-collects containers the document no longer describes.
	go runDSDLoop(ctx, log, c, st, journal, registry, driver)

	// Discover the reachable WireGuard endpoint once at startup (STUN-style
	// probe); the CP serves it to peers so tunnels form across NAT. Re-probed
	// periodically below to track a changing public IP.
	meshEndpoint, err := mesh.DiscoverEndpoint(ctx)
	if err != nil {
		log.Warn("mesh: could not discover reachable endpoint; peers may be unable to dial this host", "err", err)
	} else {
		log.Info("mesh: discovered endpoint", "endpoint", meshEndpoint)
	}

	// Heartbeat loop: fixed interval with jitter; exponential backoff on
	// transient failures; permanent (4xx) failures mean the credential is
	// gone — exit so the operator re-bootstraps.
	backoff := *interval
	hb := 0
	// Hardening posture is a daily drift re-check, not a per-heartbeat probe:
	// compute it at startup and roughly every 6h, and report the cached value on
	// every heartbeat so the dashboard's score stays fresh without hammering the
	// host with lsblk/sshd/sysctl/nft calls each tick.
	var posture host.Posture
	postureEvery := int((6 * time.Hour) / *interval)
	if postureEvery < 1 {
		postureEvery = 1
	}
	// Mesh application state, refreshed by syncMesh after each heartbeat and
	// reported on the next one to drive the CP's mesh-gated Ready state.
	var meshApplied bool
	var meshPeers int
	for {
		hb++
		// Re-probe the endpoint roughly every 10 minutes to track IP changes.
		if hb%20 == 0 {
			if ep, err := mesh.DiscoverEndpoint(ctx); err == nil && ep != "" {
				meshEndpoint = ep
			}
		}
		if hb == 1 || hb%postureEvery == 0 {
			posture = hostDriver.Posture(ctx)
		}
		hostFacts := facts.Collect()
		if avail, ver := container.Probe(ctx, docker); avail {
			hostFacts.DockerAvailable = true
			hostFacts.DockerVersion = ver
		}
		f, _ := json.Marshal(hostFacts)
		sample := metrics.Collect(ctx)
		err := c.Heartbeat(ctx, st.AgentToken, client.HeartbeatRequest{
			AgentVersion: version,
			Facts:        f,
			Pubkey:       meshPub,
			Endpoint:     meshEndpoint,
			Metrics: &client.MetricSample{
				CPUPct:  sample.CPUPct,
				MemPct:  sample.MemPct,
				DiskPct: sample.DiskPct,
				Load1:   sample.Load1,
			},
			Hardening: &client.HardeningReport{
				Score:         posture.Score,
				DiskEncrypted: posture.DiskEncrypted,
				SSHLocked:     posture.SSHLocked,
			},
			MeshApplied:   meshApplied,
			MeshPeerCount: meshPeers,
		})
		var apiErr *client.APIError
		switch {
		case err == nil:
			backoff = *interval
			log.Info("heartbeat ok", "server_id", st.ServerID)
			meshApplied, meshPeers = syncMesh(ctx, log, c, st.AgentToken, *dataDir, meshPriv, *wgUp)
			// Report Traefik-issued certificate state (P1-8). A no-op on non-proxy
			// servers (no ACME store); never fails the loop.
			reportCertStatus(ctx, log, c, st.AgentToken, driver)
		case errors.As(err, &apiErr) && apiErr.Permanent():
			return err
		case ctx.Err() != nil:
			log.Info("shutting down")
			return nil
		default:
			if backoff < 5*time.Minute {
				backoff *= 2
			}
			log.Warn("heartbeat failed; backing off", "err", err, "retry_in", backoff)
		}

		jitter := time.Duration(rand.Int64N(int64(*interval / 10)))
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return nil
		case <-time.After(backoff + jitter):
		}
	}
}

// runDSDLoop long-polls the control plane for the server's Desired-State
// Document, verifies it against the pinned CP key, applies its ops in
// dependency order (resuming from the journal), and reports status. It never
// applies a DSD whose version is <= the last applied one (replay/downgrade
// rejection) and never applies an unsigned/tampered/wrongly-keyed DSD.
func runDSDLoop(ctx context.Context, log *slog.Logger, c *client.Client, st state.State, journal *apply.Journal, registry *apply.Registry, driver *container.Driver) {
	if st.DSDPublicKey == "" {
		log.Warn("no pinned DSD key; DSD sync disabled (re-bootstrap to enrol)")
		return
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		after, err := journal.LastAppliedVersion()
		if err != nil {
			log.Error("dsd: read journal", "err", err)
			return
		}
		signed, got, err := c.GetDSD(ctx, st.AgentToken, after)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.Permanent() {
				log.Error("dsd: permanent error; stopping loop", "err", err)
				return
			}
			log.Warn("dsd: fetch failed; backing off", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		if !got {
			continue // 204: poll again
		}

		// Trust anchor: reject anything not signed by the pinned CP key.
		if err := dsd.Verify(st.DSDPublicKey, signed); err != nil {
			log.Error("dsd: rejected", "reason", err, "version", signed.Document.Version)
			continue
		}
		// Identity binding: a validly-signed DSD is scoped to exactly one
		// server. Delivery is already token-scoped, so this only bites on a CP
		// misroute/bug, but applying another server's ops here would be
		// catastrophic — reject anything not addressed to us.
		if signed.Document.ServerID != st.ServerID {
			log.Error("dsd: rejected foreign server", "doc_server", signed.Document.ServerID, "self", st.ServerID)
			continue
		}
		// Replay/downgrade rejection: never apply an older-or-equal version.
		if signed.Document.Version <= after {
			log.Warn("dsd: rejected stale version", "got", signed.Document.Version, "last_applied", after)
			continue
		}

		results, err := registry.Apply(ctx, log, journal, signed.Document)
		if err != nil {
			log.Error("dsd: apply", "err", err, "version", signed.Document.Version)
			continue
		}
		log.Info("dsd applied", "version", signed.Document.Version, "ops", len(results))
		// Converge actual state to the document: remove managed containers this
		// DSD no longer describes (e.g. a deleted resource).
		if driver != nil {
			driver.GC(ctx, signed.Document)
		}
		if err := c.PostDSDStatus(ctx, st.AgentToken, signed.Document.Version, apply.StatusPayload(results)); err != nil {
			log.Warn("dsd: status report failed", "err", err)
		}
	}
}

// reportCertStatus reads Traefik's ACME store and reports issued certificates to
// the control plane (P1-8). Best-effort: a non-proxy server (no ACME store) or a
// proxy with nothing issued yet reports nothing; errors never fail the loop.
func reportCertStatus(ctx context.Context, log *slog.Logger, c *client.Client, agentToken string, driver *container.Driver) {
	if driver == nil {
		return
	}
	reports, err := driver.TraefikCertStatus(ctx)
	if err != nil {
		log.Warn("cert status: read failed", "err", err)
		return
	}
	if len(reports) == 0 {
		return
	}
	if err := c.PostDomainStatus(ctx, agentToken, reports); err != nil {
		log.Warn("cert status: report failed", "err", err)
	}
}

// syncMesh refreshes the WireGuard peer config after a successful heartbeat.
// Mesh trouble never fails the heartbeat loop — the coordination plane is
// best-effort in v0.
// syncMesh renders + writes the WireGuard peer config and reports whether the
// mesh config is applied and how many peers it covers, so the next heartbeat can
// drive the CP's mesh-gated Ready state. "applied" here means the peer config for
// the current peer set is rendered/written (the CP is outbound-only and never on
// the WG data plane, so it cannot observe a real handshake in v1).
func syncMesh(ctx context.Context, log *slog.Logger, c *client.Client, agentToken, dataDir, meshPriv string, wgUp bool) (applied bool, peerCount int) {
	res, err := c.MeshPeers(ctx, agentToken)
	if err != nil {
		log.Warn("mesh: peer fetch failed", "err", err)
		return false, 0
	}
	if res.Self.MeshIP == nil || *res.Self.MeshIP == "" {
		log.Warn("mesh: no mesh IP allocated yet")
		return false, 0
	}
	path, changed, err := mesh.WriteConfig(dataDir, mesh.RenderConfig(meshPriv, *res.Self.MeshIP, res.Peers))
	if err != nil {
		log.Warn("mesh: config write failed", "err", err)
		return false, len(res.Peers)
	}
	if changed {
		log.Info("mesh: peer config updated", "config", path, "peers", len(res.Peers), "mesh_ip", *res.Self.MeshIP)
		if wgUp {
			mesh.Apply(ctx, log, path)
		}
	}
	return true, len(res.Peers)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
