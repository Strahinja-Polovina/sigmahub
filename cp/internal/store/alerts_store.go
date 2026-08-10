package store

// P2-6 alerting: per-org notification channels + event→channel rules + the
// delivery outbox. Producers (heartbeat sweep, deploy/backup/cert state
// changes) enqueue outbox rows INSIDE the transaction that records the state
// change, so an alert can never be lost between commit and notify; the
// dispatcher loop (cp/internal/alerts) drains the outbox with retry/backoff,
// so sending can never block or fail the originating operation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Alert event vocabulary. Rules reference these; producers emit them.
const (
	AlertServerUnreachable = "server_unreachable"
	AlertServerRecovered   = "server_recovered"
	AlertDeployFailed      = "deploy_failed"
	AlertBackupFailed      = "backup_failed"
	AlertVerifyFailed      = "verify_failed"
	AlertCertFailed        = "cert_failed"
	AlertCertExpiring      = "cert_expiring"
	AlertPaymentFailed     = "payment_failed"
	// AlertDecommissionTimedOut fires when the sweeper gives up waiting for a
	// decommissioning agent's ack and removes the server anyway (SIGMA-233).
	//
	// It is the only ending of a disconnect that leaves the machine untouched
	// without anybody choosing that: on the agent's ack the host tore itself
	// down, and a force disconnect is an operator pressing a button with the
	// cleanup script in front of them. On the timeout the record goes away and
	// sigmad, its unit, its tunnel and every container are still on a box that
	// nothing in the product describes any more — so this event is the one
	// notice the operator gets that they now own a host by hand.
	AlertDecommissionTimedOut = "decommission_timed_out"
	// AlertDSDApplyFailed fires when a server exhausts its DSD apply re-drive
	// budget (SIGMA-247) — the CP has re-issued the same failing document
	// maxDSDRedrive times and is about to go quiet about it.
	//
	// It is the only notice that desired state and actual state have diverged
	// permanently. The host keeps heartbeating, so its status stays 'running'
	// and every other signal the product has says the machine is fine; what is
	// actually true is that some op — a firewall ruleset, a container, a proxy
	// config — has not applied for minutes and will not be retried again until
	// somebody changes the configuration. Without this event that divergence is
	// invisible: the re-drive cap is correct, but a cap nobody is told about is
	// indistinguishable from silence.
	AlertDSDApplyFailed = "dsd_apply_failed"
)

// AlertEvents is the full vocabulary, in display order.
var AlertEvents = []string{
	AlertServerUnreachable, AlertServerRecovered, AlertDecommissionTimedOut,
	AlertDSDApplyFailed,
	AlertDeployFailed, AlertBackupFailed, AlertVerifyFailed, AlertCertFailed,
	AlertCertExpiring, AlertPaymentFailed,
}

func validAlertEvent(e string) bool {
	for _, v := range AlertEvents {
		if v == e {
			return true
		}
	}
	return false
}

// alertChannelKinds gates channel creation; each kind's sender lives in
// cp/internal/alerts.
var alertChannelKinds = map[string]bool{"email": true, "slack": true, "telegram": true, "webhook": true}

// alertChannelAAD binds the channel secret ciphertext to its row (mirrors
// secretAAD/targetAAD): a moved ciphertext fails to open.
func alertChannelAAD(orgID, channelID string) []byte { return []byte(orgID + "|alch|" + channelID) }

// slackWebhookPrefix is the only host Slack ever issues an incoming webhook on.
// A slack channel's secret IS its destination URL, so this prefix is the whole
// validation: anything else is not a Slack channel, whatever it resolves to.
const slackWebhookPrefix = "https://hooks.slack.com/"

// validateChannelConfig rejects a channel that could never deliver, so
// misconfiguration surfaces at create time instead of as a delivery failure —
// and, for the two kinds whose destination the tenant chooses, rejects one that
// could deliver somewhere it must not (SIGMA-259).
//
// secret is passed because for a slack channel the secret IS the destination:
// there was no `slack` case here at all, so a slack channel's URL reached the
// HTTP client with zero validation. The webhook case checked only an http(s)
// prefix, which admits 127.0.0.1, 169.254.169.254 and every RFC1918 address.
// Alert channels are the CP's only tenant-controlled egress destination and the
// CP sits inside the operator's private network next to Postgres,
// VictoriaMetrics and Loki, so that was a request-forgery primitive handed to
// every self-service signup.
func validateChannelConfig(kind string, cfg json.RawMessage, secret string) error {
	switch kind {
	case "slack":
		// Not a general destination check: Slack has exactly one webhook host,
		// and pinning it is both stricter and stable (no DNS at create time).
		if !strings.HasPrefix(secret, slackWebhookPrefix) {
			return ErrInvalid{Msg: "slack channels require an incoming-webhook URL starting with " + slackWebhookPrefix}
		}
	case "email":
		var c struct {
			Host string   `json:"host"`
			From string   `json:"from"`
			To   []string `json:"to"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil {
			return ErrInvalid{Msg: "config must be a JSON object"}
		}
		if c.Host == "" || c.From == "" || len(c.To) == 0 {
			return ErrInvalid{Msg: "email channels require config.host, config.from and config.to"}
		}
	case "telegram":
		var c struct {
			ChatID string `json:"chatId"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil {
			return ErrInvalid{Msg: "config must be a JSON object"}
		}
		if c.ChatID == "" {
			return ErrInvalid{Msg: "telegram channels require config.chatId"}
		}
	case "webhook":
		var c struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil {
			return ErrInvalid{Msg: "config must be a JSON object"}
		}
		if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
			return ErrInvalid{Msg: "webhook channels require an http(s) config.url"}
		}
		if err := ValidateAlertDestination(c.URL); err != nil {
			return err
		}
	}
	return nil
}

// destinationLookupTimeout bounds the DNS resolution the destination check
// does. It runs on an API request (create channel) and on the dispatcher's send
// path, and neither may hang on a resolver.
const destinationLookupTimeout = 5 * time.Second

// ValidateAlertDestination refuses an alert destination the control plane must
// not be made to POST to (SIGMA-259).
//
// It is exported because the same rule has to hold in two places, and checking
// it in only one is the same as not checking it: at CREATE time, so the operator
// is told immediately; and at SEND time in cp/internal/alerts, because rows
// created before this existed are still in the table, DNS answers change between
// the two moments, and a redirect can point anywhere.
//
// What is refused, and why each one matters on a hosted control plane whose
// process sits inside the operator's private network:
//
//   - loopback — the CP's own admin surfaces and anything else on its host;
//   - link-local (169.254.0.0/16, fe80::/10) — the cloud metadata service, i.e.
//     169.254.169.254/latest/meta-data/iam/security-credentials/;
//   - private (RFC1918, fc00::/7) and CGNAT — Postgres, VictoriaMetrics, Loki
//     and every other internal service the CP can reach;
//   - unspecified, multicast, and anything not global unicast;
//   - ports other than 80 and 443 — an alert channel delivers over the web, and
//     every other port on a reachable host is somebody's service (vminsert on
//     8480, Elasticsearch on 9200).
//
// A hostname is RESOLVED and every address it answers with must pass, so
// `internal.example.com` pointing at 10.0.0.5 is refused as surely as the
// literal is. A name that does not resolve is refused too: we cannot show that
// it is safe, and a destination that cannot be resolved cannot deliver either.
//
// This is not proof against DNS rebinding — the address can change between this
// check and the dial. Closing that needs a dial-time hook on the transport;
// what this does close is the whole self-service class of the attack, where the
// tenant simply types the address they want the CP to fetch.
func ValidateAlertDestination(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ErrInvalid{Msg: "destination must be a valid http(s) URL"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalid{Msg: "destination must be an http(s) URL"}
	}
	host := u.Hostname()
	if host == "" {
		return ErrInvalid{Msg: "destination must name a host"}
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return ErrInvalid{Msg: "destination port must be 80 or 443"}
	}
	if ip := net.ParseIP(host); ip != nil {
		return checkPublicIP(host, ip)
	}
	// Names that never mean anything but "this machine" / "this network",
	// checked before the resolver so a search-domain trick cannot dodge them.
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return ErrInvalid{Msg: "destination " + host + " is an internal address; alert channels must point at a public endpoint"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), destinationLookupTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return ErrInvalid{Msg: "destination host " + host + " does not resolve"}
	}
	for _, ip := range ips {
		if err := checkPublicIP(host, ip); err != nil {
			return err
		}
	}
	return nil
}

// checkPublicIP is the address half of ValidateAlertDestination. `shown` is what
// the operator typed, so the message names their input rather than an address
// they never saw.
func checkPublicIP(shown string, ip net.IP) error {
	internal := ip.IsLoopback() || ip.IsUnspecified() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		!ip.IsGlobalUnicast()
	if v4 := ip.To4(); v4 != nil && !internal {
		switch {
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10 CGNAT
			internal = true
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24 IETF protocol assignments
			internal = true
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15 benchmarking
			internal = true
		}
	}
	if internal {
		return ErrInvalid{Msg: "destination " + shown + " is an internal address; alert channels must point at a public endpoint"}
	}
	return nil
}

// AlertChannel is channel METADATA — the secret never rides on this type.
type AlertChannel struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	Enabled   bool            `json:"enabled"`
	Events    []string        `json:"events"`
	LastOKAt  *time.Time      `json:"lastOkAt,omitempty"`
	LastError string          `json:"lastError,omitempty"`
	CreatedBy string          `json:"createdBy"`
	CreatedAt time.Time       `json:"createdAt"`
}

// CreateAlertChannelInput describes a new channel. Secret is the channel
// credential (Slack webhook URL, Telegram bot token, SMTP password, webhook
// HMAC key) and is DEK-envelope-encrypted at rest.
type CreateAlertChannelInput struct {
	Kind   string
	Name   string
	Config json.RawMessage
	Secret string
}

// CreateAlertChannel stores a channel with its secret enveloped and enables
// every event for it by default (rules are the off switch). Audited.
func (s *Store) CreateAlertChannel(ctx context.Context, orgID, actor string, in CreateAlertChannelInput) (AlertChannel, error) {
	if !alertChannelKinds[in.Kind] {
		return AlertChannel{}, ErrInvalid{Msg: "kind must be one of email, slack, telegram, webhook"}
	}
	if in.Name == "" {
		return AlertChannel{}, ErrInvalid{Msg: "name is required"}
	}
	// Slack and Telegram cannot deliver anything without their credential.
	if (in.Kind == "slack" || in.Kind == "telegram") && in.Secret == "" {
		return AlertChannel{}, ErrInvalid{Msg: in.Kind + " channels require a secret (webhook URL / bot token)"}
	}
	cfg := in.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	if err := validateChannelConfig(in.Kind, cfg, in.Secret); err != nil {
		return AlertChannel{}, err
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return AlertChannel{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ch := AlertChannel{
		ID: newID("alch"), Kind: in.Kind, Name: in.Name, Config: cfg,
		Enabled: true, Events: append([]string{}, AlertEvents...), CreatedBy: actor,
	}
	var nonce, ct []byte
	var dekID *string
	if in.Secret != "" {
		id, dek, err := s.activeDEKTx(ctx, tx, orgID)
		if err != nil {
			return AlertChannel{}, err
		}
		nonce, ct, err = gcmSeal(dek, alertChannelAAD(orgID, ch.ID), []byte(in.Secret))
		if err != nil {
			return AlertChannel{}, err
		}
		dekID = &id
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO alert_channels (id, org_id, kind, name, config, secret_ciphertext, secret_nonce, dek_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at`,
		ch.ID, orgID, ch.Kind, ch.Name, cfg, ct, nonce, dekID, actor).Scan(&ch.CreatedAt)
	if err != nil {
		return AlertChannel{}, fmt.Errorf("insert alert channel: %w", err)
	}
	for _, ev := range AlertEvents {
		if _, err := tx.Exec(ctx, `
			INSERT INTO alert_rules (org_id, event, channel_id) VALUES ($1, $2, $3)`,
			orgID, ev, ch.ID); err != nil {
			return AlertChannel{}, fmt.Errorf("insert alert rule: %w", err)
		}
	}
	if err := auditTx(ctx, tx, orgID, actor, "Alert channel created", ch.Name+" ("+ch.Kind+")"); err != nil {
		return AlertChannel{}, err
	}
	return ch, tx.Commit(ctx)
}

// ListAlertChannels returns channel metadata + enabled events. Never secrets.
func (s *Store) ListAlertChannels(ctx context.Context, orgID string) ([]AlertChannel, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, kind, name, config, enabled, last_ok_at, last_error, created_by, created_at
		  FROM alert_channels WHERE org_id = $1 ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlertChannel{}
	byID := map[string]int{}
	for rows.Next() {
		var ch AlertChannel
		if err := rows.Scan(&ch.ID, &ch.Kind, &ch.Name, &ch.Config, &ch.Enabled,
			&ch.LastOKAt, &ch.LastError, &ch.CreatedBy, &ch.CreatedAt); err != nil {
			return nil, err
		}
		ch.Events = []string{}
		byID[ch.ID] = len(out)
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rrows, err := s.Pool.Query(ctx, `
		SELECT channel_id, event FROM alert_rules WHERE org_id = $1`, orgID)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var chID, ev string
		if err := rrows.Scan(&chID, &ev); err != nil {
			return nil, err
		}
		if i, ok := byID[chID]; ok {
			out[i].Events = append(out[i].Events, ev)
		}
	}
	return out, rrows.Err()
}

// DeleteAlertChannel removes a channel (rules and queued deliveries cascade).
func (s *Store) DeleteAlertChannel(ctx context.Context, orgID, channelID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var name string
	err = tx.QueryRow(ctx,
		`DELETE FROM alert_channels WHERE org_id = $1 AND id = $2 RETURNING name`,
		orgID, channelID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Alert channel deleted", name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetAlertRules replaces the event set a channel receives. Audited.
func (s *Store) SetAlertRules(ctx context.Context, orgID, channelID string, events []string, actor string) error {
	for _, ev := range events {
		if !validAlertEvent(ev) {
			return ErrInvalid{Msg: fmt.Sprintf("unknown alert event %q", ev)}
		}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var name string
	err = tx.QueryRow(ctx,
		`SELECT name FROM alert_channels WHERE org_id = $1 AND id = $2`, orgID, channelID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM alert_rules WHERE org_id = $1 AND channel_id = $2`, orgID, channelID); err != nil {
		return err
	}
	for _, ev := range events {
		if _, err := tx.Exec(ctx, `
			INSERT INTO alert_rules (org_id, event, channel_id) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, orgID, ev, channelID); err != nil {
			return err
		}
	}
	if err := auditTx(ctx, tx, orgID, actor, "Alert rules updated", name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AlertChannelSend is what the dispatcher needs to deliver on a channel —
// including the unwrapped secret, released only through this audited-DEK path
// and held in memory for the send.
type AlertChannelSend struct {
	ID     string
	OrgID  string
	Kind   string
	Config json.RawMessage
	Secret string
}

// AlertChannelForSend resolves a channel's config + secret for delivery.
func (s *Store) AlertChannelForSend(ctx context.Context, orgID, channelID string) (AlertChannelSend, error) {
	var out AlertChannelSend
	var ct, nonce []byte
	var dekID *string
	err := s.Pool.QueryRow(ctx, `
		SELECT id, org_id, kind, config, secret_ciphertext, secret_nonce, dek_id
		  FROM alert_channels WHERE org_id = $1 AND id = $2`,
		orgID, channelID).Scan(&out.ID, &out.OrgID, &out.Kind, &out.Config, &ct, &nonce, &dekID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AlertChannelSend{}, ErrNotFound
	}
	if err != nil {
		return AlertChannelSend{}, err
	}
	if len(ct) > 0 && dekID != nil {
		dek, err := s.dekPlaintext(ctx, s.Pool, *dekID)
		if err != nil {
			return AlertChannelSend{}, err
		}
		plain, err := gcmOpen(dek, alertChannelAAD(orgID, channelID), nonce, ct)
		if err != nil {
			return AlertChannelSend{}, fmt.Errorf("open channel secret: %w", err)
		}
		out.Secret = string(plain)
	}
	return out, nil
}

// ── outbox ──────────────────────────────────────────────────────────────────

// enqueueAlertTx fans an event out to every enabled channel whose rules
// subscribe to it. window bounds re-alerting per (channel, dedupKey):
// window <= 0 means "at most once ever" (per-deployment / per-run alerts);
// a positive window is a flap cooldown.
func enqueueAlertTx(ctx context.Context, tx pgx.Tx, orgID, event, dedupKey string, window time.Duration, title, body string) error {
	// Serialize concurrent enqueues of the SAME logical alert (overlapping sweeps
	// or multiple replicas) so the NOT EXISTS check-then-insert below is atomic
	// and can't double-enqueue under READ COMMITTED — the alert_outbox dedup index
	// is non-unique, and windowed keys (cert-failed, server-recovered) legitimately
	// re-fire, so a plain UNIQUE index can't be used (SIGMA-95). Xact-scoped; the
	// lock releases on commit/rollback, and the window filter still governs
	// re-fires after the lock is released.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('alert:' || $1 || ':' || $2))`, orgID, dedupKey); err != nil {
		return fmt.Errorf("alert dedup lock: %w", err)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO alert_outbox (org_id, channel_id, event, dedup_key, title, body)
		SELECT r.org_id, r.channel_id, $2, $3, $4, $5
		  FROM alert_rules r
		  JOIN alert_channels c ON c.id = r.channel_id AND c.enabled
		 WHERE r.org_id = $1 AND r.event = $2
		   AND NOT EXISTS (
			SELECT 1 FROM alert_outbox o
			 WHERE o.channel_id = r.channel_id AND o.dedup_key = $3
			   AND ($6::float8 <= 0 OR o.created_at > now() - make_interval(secs => $6))
		   )`,
		orgID, event, dedupKey, title, left(body, 4000), window.Seconds())
	if err != nil {
		return fmt.Errorf("enqueue alert: %w", err)
	}
	return nil
}

func left(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// AlertDelivery is one due outbox row.
type AlertDelivery struct {
	ID        int64
	OrgID     string
	ChannelID string
	Event     string
	Title     string
	Body      string
	Attempts  int
}

// deliveryLease is how long a claimed ('sending') delivery is reserved before it
// becomes reclaimable. It bounds double-delivery to the case where a single send
// runs longer than the lease; a crashed dispatcher's rows are retried after it.
const deliveryLease = 5 * time.Minute

// DueAlertDeliveries atomically CLAIMS pending deliveries whose next attempt is
// due (oldest first) and returns them. Without the claim (SIGMA-106) the drain
// was a plain SELECT that left rows visibly 'pending' for the whole external
// send, so two CP replicas' dispatchers both selected the same row and both sent
// — a duplicate webhook/email/Slack/Telegram delivery. The single UPDATE ...
// FOR UPDATE SKIP LOCKED marks the rows 'sending' and leases them, so a sibling
// replica skips locked rows and never re-sends a claimed one. A 'sending' row
// whose lease has expired (the dispatcher crashed mid-send) is reclaimable.
func (s *Store) DueAlertDeliveries(ctx context.Context, limit int) ([]AlertDelivery, error) {
	rows, err := s.Pool.Query(ctx, `
		UPDATE alert_outbox
		   SET status = 'sending', next_attempt_at = now() + make_interval(secs => $2)
		 WHERE id IN (
		     SELECT id FROM alert_outbox
		      WHERE status IN ('pending', 'sending') AND next_attempt_at <= now()
		      ORDER BY next_attempt_at
		      LIMIT $1
		      FOR UPDATE SKIP LOCKED
		 )
		 RETURNING id, org_id, channel_id, event, title, body, attempts`,
		limit, deliveryLease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlertDelivery{}
	for rows.Next() {
		var d AlertDelivery
		if err := rows.Scan(&d.ID, &d.OrgID, &d.ChannelID, &d.Event, &d.Title, &d.Body, &d.Attempts); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RenewAlertDeliveryLease refreshes the claim lease on a row the dispatcher is
// about to send, and reports whether the row is still this dispatcher's to send
// (status still 'sending'). A slow drain (a batch of channels each timing out
// near deliverySendTimeout) can exceed the once-per-batch lease, after which a
// sibling replica reclaims the still-'sending' tail and re-sends it — the exact
// duplicate SIGMA-106 targeted. Renewing right before the send closes that
// window; a false result means a sibling already finalized the row (sent/failed)
// or backed it off, so the caller must skip the send (SIGMA-130).
func (s *Store) RenewAlertDeliveryLease(ctx context.Context, deliveryID int64) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE alert_outbox SET next_attempt_at = now() + make_interval(secs => $2)
		 WHERE id = $1 AND status = 'sending'`, deliveryID, deliveryLease.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetAlertDeliveryResult records a send attempt: success marks the row sent
// and the channel healthy; failure backs off exponentially (30s·2^attempts,
// capped at 1h) and gives up for good after maxAttempts, leaving the failure
// visible on the row AND the channel (honest delivery status in the UI).
func (s *Store) SetAlertDeliveryResult(ctx context.Context, deliveryID int64, ok bool, errText string, maxAttempts int) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Finalize the row this dispatcher CLAIMED (status='sending' after
	// DueAlertDeliveries). A failure re-queues it as 'pending' with backoff so it
	// is re-claimed on a later drain (SIGMA-106).
	var channelID string
	if ok {
		err = tx.QueryRow(ctx, `
			UPDATE alert_outbox SET status = 'sent', sent_at = now(), last_error = ''
			 WHERE id = $1 AND status = 'sending' RETURNING channel_id`, deliveryID).Scan(&channelID)
	} else {
		err = tx.QueryRow(ctx, `
			UPDATE alert_outbox
			   SET attempts = attempts + 1,
			       last_error = left($2, 2000),
			       status = CASE WHEN attempts + 1 >= $3 THEN 'failed' ELSE 'pending' END,
			       next_attempt_at = now() + make_interval(secs => LEAST(3600, 30 * POWER(2, attempts)))
			 WHERE id = $1 AND status = 'sending' RETURNING channel_id`,
			deliveryID, errText, maxAttempts).Scan(&channelID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if ok {
		_, err = tx.Exec(ctx,
			`UPDATE alert_channels SET last_ok_at = now(), last_error = '' WHERE id = $1`, channelID)
	} else {
		_, err = tx.Exec(ctx,
			`UPDATE alert_channels SET last_error = left($2, 2000) WHERE id = $1`, channelID, errText)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EnqueueCertExpiringAlerts flags issued certificates that expire within the
// window. The dedup key carries the expiry timestamp, so each renewal cycle
// alerts at most once — a renewed cert gets a new expiry and a fresh key.
func (s *Store) EnqueueCertExpiringAlerts(ctx context.Context, within time.Duration) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT org_id, domain, cert_expires_at
		  FROM domains
		 WHERE cert_status = 'issued'
		   AND cert_expires_at IS NOT NULL
		   AND cert_expires_at > now()
		   AND cert_expires_at < now() + make_interval(secs => $1)`, within.Seconds())
	if err != nil {
		return err
	}
	type row struct {
		orgID, domain string
		expires       time.Time
	}
	var due []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.orgID, &r.domain, &r.expires); err != nil {
			rows.Close()
			return err
		}
		due = append(due, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range due {
		days := int(time.Until(r.expires).Hours() / 24)
		if err := enqueueAlertTx(ctx, tx, r.orgID, AlertCertExpiring,
			fmt.Sprintf("cert:%s:%d", r.domain, r.expires.Unix()), 0,
			fmt.Sprintf("Certificate for %s expires in %d days", r.domain, days),
			fmt.Sprintf("The TLS certificate for %s expires at %s. Automatic renewal should replace it; if this alert repeats as the date approaches, renewal is failing — check the domain's cert status.",
				r.domain, r.expires.UTC().Format(time.RFC3339)),
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
