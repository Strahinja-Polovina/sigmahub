package store

// The org's container image registry.
//
// A build produces an image on ONE machine. That is enough while the same
// machine runs it, and it stops being enough the moment the two are different
// hosts — a dedicated build server, or any cluster workload, where the
// scheduler decides which node runs the pod. Those images have to travel, and
// the only thing every host can pull from is a registry.
//
// It is the customer's registry, not ours: their images, their quota, their
// retention. We store the coordinates and a KMS-wrapped credential, hand the
// credential to an agent only over the authenticated channel, and never put it
// in a Desired-State Document.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ImageRegistry is an org's registry configuration. The password is never a
// field here — it is fetched separately, by an agent, and audited.
type ImageRegistry struct {
	Host      string `json:"host"`
	Namespace string `json:"namespace"`
	Username  string `json:"username"`
	// HasPassword reports whether a credential is stored, so the dashboard can
	// say "configured" without ever reading the secret.
	HasPassword bool      `json:"hasPassword"`
	CreatedBy   string    `json:"createdBy"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Repository is the prefix every image tag gets: host, plus the namespace when
// there is one. Empty when no registry is configured, which is the signal to
// keep using local tags.
func (r ImageRegistry) Repository() string {
	host := strings.Trim(strings.TrimSpace(r.Host), "/")
	if host == "" {
		return ""
	}
	ns := strings.Trim(strings.TrimSpace(r.Namespace), "/")
	if ns == "" {
		return host
	}
	return host + "/" + ns
}

// registryPurpose namespaces the registry-password wrapping key per org, so a
// key recovered for one org cannot unwrap another's.
func registryPurpose(orgID string) string { return "registry:" + orgID }

// registryHostPattern rejects anything that is not a registry host. A tag is
// built by string concatenation and then handed to a container runtime, so a
// host carrying a slash, a space or a scheme would silently produce a different
// repository than the one the user configured.
func validRegistryHost(host string) bool {
	if host == "" || len(host) > 255 {
		return false
	}
	if strings.ContainsAny(host, "/ \t\n@\\") {
		return false
	}
	// A scheme is the most common paste-in mistake and changes the meaning of
	// the tag entirely; reject it with a specific message rather than building
	// "https:/reg.example.com/app:sha".
	return !strings.Contains(host, "://")
}

// validRegistryNamespace allows a path under the host (`acme`, `acme/team`).
func validRegistryNamespace(ns string) bool {
	if ns == "" {
		return true
	}
	if len(ns) > 255 || strings.ContainsAny(ns, " \t\n@\\") || strings.Contains(ns, "//") {
		return false
	}
	return !strings.HasPrefix(ns, "/") && !strings.HasSuffix(ns, "/")
}

// SetImageRegistryInput configures (or reconfigures) the org's registry.
type SetImageRegistryInput struct {
	Host      string
	Namespace string
	Username  string
	// Password is wrapped before it is stored. Empty on an update KEEPS the
	// stored credential, so editing the namespace does not silently clear the
	// password and break every push an hour later.
	Password string
}

// SetImageRegistry stores the org's registry coordinates and credential.
func (s *Store) SetImageRegistry(ctx context.Context, orgID string, in SetImageRegistryInput, actor string) (ImageRegistry, error) {
	host := strings.TrimSpace(in.Host)
	ns := strings.Trim(strings.TrimSpace(in.Namespace), "/")
	if !validRegistryHost(host) {
		return ImageRegistry{}, ErrInvalid{Msg: "host must be a registry hostname such as ghcr.io — no scheme, no path"}
	}
	if !validRegistryNamespace(ns) {
		return ImageRegistry{}, ErrInvalid{Msg: "namespace must be a repository path such as your-org"}
	}

	var wrapped []byte
	if in.Password != "" {
		w, err := s.custody.Wrap(ctx, registryPurpose(orgID), []byte(in.Password))
		if err != nil {
			return ImageRegistry{}, fmt.Errorf("wrap registry password: %w", err)
		}
		wrapped = w
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ImageRegistry{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var reg ImageRegistry
	var stored []byte
	// An empty password on an update keeps whatever is stored — COALESCE on the
	// EXCLUDED value, not on the column, so a first insert with no password is
	// still allowed (an anonymous/pull-only registry).
	err = tx.QueryRow(ctx, `
		INSERT INTO org_registries (org_id, host, namespace, username, password_wrapped, created_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (org_id) DO UPDATE SET
			host = EXCLUDED.host,
			namespace = EXCLUDED.namespace,
			username = EXCLUDED.username,
			password_wrapped = COALESCE(EXCLUDED.password_wrapped, org_registries.password_wrapped),
			updated_at = now()
		RETURNING host, namespace, username, password_wrapped, created_by, updated_at`,
		orgID, host, ns, strings.TrimSpace(in.Username), wrapped, actor).
		Scan(&reg.Host, &reg.Namespace, &reg.Username, &stored, &reg.CreatedBy, &reg.UpdatedAt)
	if err != nil {
		return ImageRegistry{}, err
	}
	reg.HasPassword = len(stored) > 0
	if err := auditTx(ctx, tx, orgID, actor, "Image registry configured", reg.Repository()); err != nil {
		return ImageRegistry{}, err
	}
	return reg, tx.Commit(ctx)
}

// GetImageRegistry returns the org's registry, or ok=false when none is set.
// Never returns the password.
func (s *Store) GetImageRegistry(ctx context.Context, orgID string) (ImageRegistry, bool, error) {
	var reg ImageRegistry
	var wrapped []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT host, namespace, username, password_wrapped, created_by, updated_at
		  FROM org_registries WHERE org_id = $1`, orgID).
		Scan(&reg.Host, &reg.Namespace, &reg.Username, &wrapped, &reg.CreatedBy, &reg.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ImageRegistry{}, false, nil
	}
	if err != nil {
		return ImageRegistry{}, false, err
	}
	reg.HasPassword = len(wrapped) > 0
	return reg, true, nil
}

// DeleteImageRegistry removes the configuration. Deploys that need a registry
// start failing with "no registry configured" rather than silently pushing to
// docker.io, which is the whole reason this exists.
func (s *Store) DeleteImageRegistry(ctx context.Context, orgID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `DELETE FROM org_registries WHERE org_id = $1`, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := auditTx(ctx, tx, orgID, actor, "Image registry removed", ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RegistryCredential is what an agent needs to authenticate a push or a pull.
type RegistryCredential struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// serverNeedsRegistry reports whether this server has a reason to hold the org's
// registry credential right now (SIGMA-258).
//
// There are exactly three of them, and they mirror what the reconciler renders:
//
//   - PUSH. The server is the build_server_id of a deployment that is still in
//     flight, so it is about to run `docker push` (renderDeployOps only
//     qualifies an image with the registry when the build and the run are on
//     different daemons, and ClusterBuildSpecsForServer renders cluster builds
//     for the same statuses).
//   - PULL, single host. The server is the deploy target of a deployment whose
//     build happened somewhere else, so its rollout carries an image.pull op
//     against the org's private registry (SIGMA-243). 'success' counts: the
//     rollout stays in that server's document after the deploy finishes and a
//     restart re-pulls the same image.
//   - PULL, cluster node. The server is a node of a cluster that carries
//     workloads; the k8s driver mints an imagePullSecret from this credential.
//
// Nothing else has a use for it, and the credential is in practice a registry
// PAT with push rights over every image the org publishes — so anything else is
// a 404. Before this, the query filtered on org_id alone and the server id only
// labelled the audit row, which made every enrolled host in the org — including
// a low-value staging box — a full supply-chain compromise of the whole tenant.
func (s *Store) serverNeedsRegistry(ctx context.Context, orgID, serverID string) (bool, error) {
	var need bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS (
		         SELECT 1 FROM deployments
		          WHERE org_id = $1 AND build_server_id = $2
		            AND status IN ('queued','building','deploying'))
		    OR EXISTS (
		         SELECT 1 FROM deployments
		          WHERE org_id = $1 AND server_id = $2
		            AND build_server_id IS NOT NULL AND build_server_id <> $2
		            AND status IN ('queued','building','deploying','success'))
		    OR EXISTS (
		         SELECT 1 FROM cluster_nodes n
		           JOIN clusters c ON c.id = n.cluster_id
		           JOIN resources r ON r.cluster_id = n.cluster_id
		          WHERE n.server_id = $2 AND c.org_id = $1)`,
		orgID, serverID).Scan(&need)
	if err != nil {
		return false, fmt.Errorf("check registry need: %w", err)
	}
	return need, nil
}

// RegistryCredentialForServer resolves the registry credential for one server,
// scoped by the agent token the caller already authenticated AND by whether that
// server has anything to push or pull (see serverNeedsRegistry). Audited either
// way: handing out a push credential is exactly the kind of access that has to
// leave a trail, and so is refusing one — a host asking for a credential it has
// no business holding is the first line of an incident timeline.
func (s *Store) RegistryCredentialForServer(ctx context.Context, orgID, serverID string) (RegistryCredential, error) {
	var cred RegistryCredential
	var wrapped []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT host, username, password_wrapped FROM org_registries WHERE org_id = $1`, orgID).
		Scan(&cred.Host, &cred.Username, &wrapped)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegistryCredential{}, ErrNotFound
	}
	if err != nil {
		return RegistryCredential{}, err
	}
	need, err := s.serverNeedsRegistry(ctx, orgID, serverID)
	if err != nil {
		return RegistryCredential{}, err
	}
	if !need {
		if _, aerr := s.Pool.Exec(ctx, `
			INSERT INTO cp_audit_log (org_id, actor, action, target)
			VALUES ($1, $2, 'Registry credential refused', $3)`,
			orgID, "agent:"+serverID, cred.Host); aerr != nil {
			return RegistryCredential{}, fmt.Errorf("audit: %w", aerr)
		}
		// The same answer an org with no registry gets: a server that is not part
		// of a build or a pull learns nothing about the registry from asking.
		return RegistryCredential{}, ErrNotFound
	}
	if len(wrapped) > 0 {
		plain, uerr := s.custody.Unwrap(ctx, registryPurpose(orgID), wrapped)
		if uerr != nil {
			return RegistryCredential{}, fmt.Errorf("unwrap registry password: %w", uerr)
		}
		cred.Password = string(plain)
	}
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO cp_audit_log (org_id, actor, action, target)
		VALUES ($1, $2, 'Registry credential released', $3)`,
		orgID, "agent:"+serverID, cred.Host); err != nil {
		return RegistryCredential{}, fmt.Errorf("audit: %w", err)
	}
	return cred, nil
}

// ImageRepositoryForOrg is the reconciler's read: the repository prefix to
// qualify cross-host image tags with, empty when no registry is configured.
func (s *Store) ImageRepositoryForOrg(ctx context.Context, orgID string) (string, error) {
	reg, ok, err := s.GetImageRegistry(ctx, orgID)
	if err != nil || !ok {
		return "", err
	}
	return reg.Repository(), nil
}
