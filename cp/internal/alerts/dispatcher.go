package alerts

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// deliverySendTimeout caps a single alert delivery so one slow channel can't
// stall the whole batch (SIGMA-119). Above the per-transport ceilings (10s) with
// headroom for the SMTP conversation.
const deliverySendTimeout = 20 * time.Second

// defaultConcurrency is how many deliveries a single drain sends in parallel.
//
// The drain used to be strictly serial, which made worst-case throughput
// BatchSize deliveries per BatchSize*deliverySendTimeout seconds — 50 per ~17
// minutes (SIGMA-255). That is fine for the steady-state trickle of alerts but
// catastrophic for the case alerts exist for: a correlated incident. The
// heartbeat sweeper enqueues one row per newly-unreachable server per pass, so
// a network partition across a 300-server fleet produces hundreds of rows at
// once — and the endpoint they must reach is often degraded by the same event,
// so every send burns the full deliverySendTimeout. Serially the operator
// learned their fleet was down in a trickle over several hours; with a pool the
// same backlog clears in minutes.
//
// The bound matters as much as the parallelism: these are outbound calls to
// third-party endpoints (Slack, Telegram, arbitrary webhooks, SMTP) made on
// behalf of every org at once, so an unbounded fan-out would turn one CP
// replica into a burst source and would let a single huge backlog pin an
// unbounded number of goroutines and sockets. Eight is small enough to stay
// polite to any one endpoint and large enough that a batch of 50 slow sends
// clears in a couple of minutes rather than seventeen.
const defaultConcurrency = 8

// Store is the outbox slice the dispatcher needs; *store.Store satisfies it.
type Store interface {
	DueAlertDeliveries(ctx context.Context, limit int) ([]store.AlertDelivery, error)
	AlertChannelForSend(ctx context.Context, orgID, channelID string) (store.AlertChannelSend, error)
	RenewAlertDeliveryLease(ctx context.Context, deliveryID int64) (bool, error)
	SetAlertDeliveryResult(ctx context.Context, deliveryID int64, ok bool, errText string, maxAttempts int) error
	EnqueueCertExpiringAlerts(ctx context.Context, within time.Duration) error
}

// Config bounds the dispatch loop.
type Config struct {
	// Interval between outbox drains.
	Interval time.Duration
	// BatchSize caps deliveries per drain.
	BatchSize int
	// Heartbeat, when set, is called once per drain with that drain's outcome
	// (SIGMA-248). A per-channel transport failure is NOT a dispatcher failure —
	// that is the delivery row's business, recorded on the row and retried — so
	// only the dispatcher's own inability to read the outbox or record a result
	// counts here. Otherwise one unreachable customer webhook would permanently
	// stale the loop's last-success and page the operator about a healthy
	// dispatcher.
	Heartbeat func(error)

	// Concurrency caps how many deliveries of one batch are in flight at once.
	Concurrency int
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
	if c.Concurrency <= 0 {
		c.Concurrency = defaultConcurrency
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
			passErr := drain(ctx, log, st, snd, cfg)
			if cfg.Heartbeat != nil {
				cfg.Heartbeat(passErr)
			}
		}
	}
}

func drain(ctx context.Context, log *slog.Logger, st Store, snd *Sender, cfg Config) error {
	due, err := st.DueAlertDeliveries(ctx, cfg.BatchSize)
	if err != nil {
		log.Error("alerts: list due deliveries", "err", err)
		return err
	}
	// Send the batch through a bounded worker pool rather than one row at a
	// time (SIGMA-255). Concurrent claiming is already safe: DueAlertDeliveries
	// claimed every row in this batch in a single atomic UPDATE, and the
	// per-row lease renewal below is what actually guards against a sibling
	// replica re-sending a row — neither depends on the sends being ordered.
	// Rows within a batch are independent (different orgs, channels and events),
	// so nothing here needs the sequencing the old loop provided.
	workers := cfg.Concurrency
	if workers > len(due) {
		workers = len(due)
	}
	if workers <= 0 {
		return nil
	}
	queue := make(chan store.AlertDelivery)
	// The drain's own failure, reported to the heartbeat (SIGMA-248). Written
	// from every worker, so it needs the mutex the serial version did not.
	var mu sync.Mutex
	var drainErr error
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for d := range queue {
				if err := deliver(ctx, log, st, snd, cfg, d); err != nil {
					mu.Lock()
					if drainErr == nil {
						drainErr = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, d := range due {
		if ctx.Err() != nil {
			break
		}
		queue <- d
	}
	close(queue)
	wg.Wait()
	return drainErr
}

// deliver sends one claimed outbox row and records its result. It is called
// from several goroutines at once; *Sender is documented safe for concurrent
// use and every Store call it makes is a self-contained statement.
// deliver returns an error only when the dispatcher itself could not record
// the outcome; a failed SEND is recorded on the row and is not reported here.
func deliver(ctx context.Context, log *slog.Logger, st Store, snd *Sender, cfg Config, d store.AlertDelivery) error {
	if ctx.Err() != nil {
		return nil
	}
	var sendErr error
	ch, err := st.AlertChannelForSend(ctx, d.OrgID, d.ChannelID)
	if err != nil {
		sendErr = err
	} else {
		// Refresh the claim lease right before sending. A slow batch (many
		// channels each timing out near deliverySendTimeout) can outlast the
		// once-per-batch lease, letting a sibling replica reclaim and re-send
		// the still-'sending' tail (SIGMA-130). If the row is no longer ours —
		// a sibling already finalized it — skip to avoid a duplicate delivery.
		held, rerr := st.RenewAlertDeliveryLease(ctx, d.ID)
		if rerr != nil {
			log.Warn("alerts: renew delivery lease", "delivery", d.ID, "err", rerr)
		} else if !held {
			return nil
		}
		// Bound each delivery so one slow/unreachable channel can't stall the
		// cross-org drain (SIGMA-119). The transports honour ctx; the SMTP path
		// additionally dials with its own deadline.
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
		return err
	}
	return nil
}
