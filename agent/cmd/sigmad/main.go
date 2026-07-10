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

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/client"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/facts"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/mesh"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/metrics"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/state"
)

const version = "0.1.0"

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
		endpoint  = flag.String("endpoint", envOr("SIGMAD_ENDPOINT", "http://localhost:8080"), "control plane base URL")
		bootstrap = flag.String("bootstrap-token", os.Getenv("SIGMAD_BOOTSTRAP_TOKEN"), "one-time bootstrap token (first run only)")
		dataDir   = flag.String("data-dir", envOr("SIGMAD_DATA_DIR", defaultDataDir()), "directory for persisted identity")
		interval  = flag.Duration("interval", 30*time.Second, "heartbeat interval")
		name      = flag.String("name", "", "server display name (defaults to hostname)")
		wgUp      = flag.Bool("wg-up", false, "apply the rendered WireGuard config via wg-quick (Linux only; default renders config only)")
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
		st = state.State{ServerID: res.ServerID, AgentToken: res.AgentToken}
		if err := state.Save(*dataDir, st); err != nil {
			return err
		}
		log.Info("registered", "server_id", st.ServerID, "data_dir", *dataDir)
	} else {
		log.Info("resuming identity", "server_id", st.ServerID)
	}

	// Heartbeat loop: fixed interval with jitter; exponential backoff on
	// transient failures; permanent (4xx) failures mean the credential is
	// gone — exit so the operator re-bootstraps.
	backoff := *interval
	for {
		f, _ := json.Marshal(facts.Collect())
		sample := metrics.Collect(ctx)
		err := c.Heartbeat(ctx, st.AgentToken, client.HeartbeatRequest{
			AgentVersion: version,
			Facts:        f,
			Pubkey:       meshPub,
			Metrics: &client.MetricSample{
				CPUPct:  sample.CPUPct,
				MemPct:  sample.MemPct,
				DiskPct: sample.DiskPct,
				Load1:   sample.Load1,
			},
		})
		var apiErr *client.APIError
		switch {
		case err == nil:
			backoff = *interval
			log.Info("heartbeat ok", "server_id", st.ServerID)
			syncMesh(ctx, log, c, st.AgentToken, *dataDir, meshPriv, *wgUp)
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

// syncMesh refreshes the WireGuard peer config after a successful heartbeat.
// Mesh trouble never fails the heartbeat loop — the coordination plane is
// best-effort in v0.
func syncMesh(ctx context.Context, log *slog.Logger, c *client.Client, agentToken, dataDir, meshPriv string, wgUp bool) {
	res, err := c.MeshPeers(ctx, agentToken)
	if err != nil {
		log.Warn("mesh: peer fetch failed", "err", err)
		return
	}
	if res.Self.MeshIP == nil || *res.Self.MeshIP == "" {
		log.Warn("mesh: no mesh IP allocated yet")
		return
	}
	path, changed, err := mesh.WriteConfig(dataDir, mesh.RenderConfig(meshPriv, *res.Self.MeshIP, res.Peers))
	if err != nil {
		log.Warn("mesh: config write failed", "err", err)
		return
	}
	if changed {
		log.Info("mesh: peer config updated", "config", path, "peers", len(res.Peers), "mesh_ip", *res.Self.MeshIP)
		if wgUp {
			mesh.Apply(ctx, log, path)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
