package alerts

import (
	"context"
	"log/slog"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// deliverySendTimeout caps a single alert delivery so one slow channel can't
// stall the whole batch (SIGMA-119). Above the per-transport ceilings (10s) with
// headroom for the SMTP conversation.
const deliverySendTimeout = 20 * time.Second

// Store is the outbox slice the dispatcher needs; *store.Store satisfies it.
type Store interface {
	DueAlertDeliveries(ctx context.Context, limit int) ([]store.AlertDelivery, error)
	AlertChannelForSend(ctx context.Context, orgID, channelID string) (store.AlertChannelSend, error)
	SetAlertDeliveryResult(ctx context.Context, deliveryID int64, ok bool, errText string, maxAttempts int) error
	EnqueueCertExpiringAlerts(ctx context.Context, within time.Duration) error
}

// Config bounds the dispatch loop.
type Config struct {
	// Interval between outbox drains.
	Interval time.Duration
	// BatchSize caps deliveries per drain.
	BatchSize int
	// MaxAttempts before a delivery is marked failed for good.
	MaxAttempts int
	// CertScanEvery bounds how often the cert-expiry producer runs.
	CertScanEvery time.Duration
	// CertExpiryWindow is how far ahead expiring certs alert.
	CertExpiryWindow time.Duration
}

func (c *Config) defaults() {
	if c.Interval <= 0 {
		c.Interval = 15 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 50
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 8
	}
	if c.CertScanEvery <= 0 {
		c.CertScanEvery = time.Hour
	}
	if c.CertExpiryWindow <= 0 {
		c.CertExpiryWindow = 14 * 24 * time.Hour
	}
}

// Run drains the alert outbox until ctx ends. Every failure is recorded on
// the delivery row (retry with exponential backoff) and the channel (visible
// in the UI) — one dead channel never blocks the rest of the queue, and a
// dispatcher crash loses nothing because rows stay pending.
func Run(ctx context.Context, log *slog.Logger, st Store, snd *Sender, cfg Config) {
	cfg.defaults()
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	var lastCertScan time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Since(lastCertScan) >= cfg.CertScanEvery {
				lastCertScan = time.Now()
				if err := st.EnqueueCertExpiringAlerts(ctx, cfg.CertExpiryWindow); err != nil {
					log.Error("alerts: cert expiry scan", "err", err)
				}
			}
			drain(ctx, log, st, snd, cfg)
		}
	}
}

func drain(ctx context.Context, log *slog.Logger, st Store, snd *Sender, cfg Config) {
	due, err := st.DueAlertDeliveries(ctx, cfg.BatchSize)
	if err != nil {
		log.Error("alerts: list due deliveries", "err", err)
		return
	}
	for _, d := range due {
		if ctx.Err() != nil {
			return
		}
		var sendErr error
		ch, err := st.AlertChannelForSend(ctx, d.OrgID, d.ChannelID)
		if err != nil {
			sendErr = err
		} else {
			// Bound each delivery so one slow/unreachable channel can't stall the
			// serial, cross-org drain (SIGMA-119). The transports honour ctx; the
			// SMTP path additionally dials with its own deadline.
			sendCtx, cancel := context.WithTimeout(ctx, deliverySendTimeout)
			sendErr = snd.Send(sendCtx, ch, d.Event, d.Title, d.Body)
			cancel()
		}
		ok := sendErr == nil
		errText := ""
		if sendErr != nil {
			errText = sendErr.Error()
			log.Warn("alerts: delivery failed", "delivery", d.ID, "channel", d.ChannelID, "attempt", d.Attempts+1, "err", sendErr)
		}
		if err := st.SetAlertDeliveryResult(ctx, d.ID, ok, errText, cfg.MaxAttempts); err != nil {
			log.Error("alerts: record delivery result", "delivery", d.ID, "err", err)
		}
	}
}
