# Billing (P2-4, Paddle)

SigmaHub's price model is one flat unit (€5) per **connected server** per
month, with the first 3 connected servers free. Billing runs through
[Paddle](https://www.paddle.com/) as the merchant of record, so VAT/sales tax
is handled Paddle-side.

## Honest-off

Billing is optional. With no Paddle credentials configured the whole surface
degrades to an honest **usage preview** — the Billing page shows real metered
usage and what it *would* cost, and says plainly that no charges are made.
Nothing is ever silently billed or paused.

- `CP_PADDLE_API_KEY` empty → no checkout / no subscription sync (outbound off).
- `CP_PADDLE_WEBHOOK_SECRET` empty → the webhook receiver returns 503 (mirrors
  the GitHub receiver) rather than accept unverifiable deliveries.

## The meter

`server_hours` is an idempotent hourly sweep (mirrors `usage_hours`) that
records one row per **connected** (`status='running'`) server per hour — the
time-integrated meter, so a month's usage reflects actual connected time, not
a point-in-time snapshot. The Billing summary reports the current connected
count, the billable count (`max(0, connected - 3)`), the monthly charge, and
the accrued server-hours this month for reconciliation.

## Subscription lifecycle

Subscription state (Paddle customer/subscription ids + status) lives CP-side
in `org_billing`, keyed by the org string (the CP has no orgs table — mirrors
`org_tenants`).

Statuses: `none`, `active`, `past_due`, `paused`, `canceled`. `paused` is
deliberately distinct from `canceled` — a pause is reversible, so the dashboard
offers **Resume** (customer portal) rather than a Subscribe button, and checkout
returns 409 while any non-canceled subscription exists. Without that an org
could end up with two Paddle subscriptions and a double charge (SIGMA-294).

1. **Checkout** — `POST /v1/orgs/{orgId}/billing/checkout` (Org Admin) creates
   a Paddle hosted-checkout transaction for the org's current billable
   quantity and returns its URL. The org id rides `custom_data` for webhook
   correlation. Returns 409 if the org already has a live subscription and 422
   if it has no billable units yet.
2. **Webhook** — `POST /v1/webhooks/paddle` (public, Paddle-Signature verified,
   idempotent via `webhook_deliveries`) applies `subscription.*` and
   `transaction.payment_failed` events to `org_billing`. Correlation is
   `custom_data.orgId` first and then, for the many events that carry no
   `custom_data` (renewals, portal cancellations, support edits), the stored
   `paddle_subscription_id` / `paddle_customer_id`. Anything still
   uncorrelated is acked and logged at WARN — never dropped silently.
3. **Portal** — `POST /v1/orgs/{orgId}/billing/portal` (Org Admin) returns a
   Paddle customer-portal URL to manage the payment method / subscription.

## Dunning policy

What happens when an org stops paying is a decision, not an accident. The
constants live in `store/billing.go` (`BillingGracePeriod`,
`BillingDunningInterval`) and the behaviour is:

| When | What happens |
| --- | --- |
| Entering `past_due` or `canceled` | A `payment_failed` alert is enqueued to the org's channels, saying what changes and when. |
| Every 72 h while delinquent | `SweepBillingDunning` repeats that alert. It runs inside the 10-minute usage sweep and logs at WARN, which is the operator's notice — alert channels are per-org. |
| After 14 days delinquent | The org is **capped**: `POST` of a new server or resource returns **402 Payment Required**. |
| Always | Everything already running keeps running: deploys, certificates, backups, restore-verifies, telemetry, the dashboard. We do not hold a customer's data or uptime hostage over an expired card. |
| On recovery | A move back to `active` resets `status_since`, clears `dunning_last_at`, and lifts the cap on the next request. |

The grace clock is `org_billing.status_since`, which moves only on a real status
change — re-applying the same status (Paddle re-sends `subscription.updated` for
quantity edits) must not hand a delinquent org another two weeks.

### Operator query: who is not paying

`store.DelinquentOrgs` is the programmatic view; the equivalent by hand:

```sql
SELECT org_id,
       status,
       status_since,
       status_since + interval '14 days'          AS grace_expires_at,
       now() >= status_since + interval '14 days' AS capped,
       paddle_subscription_id,
       (SELECT count(*) FROM servers sv
         WHERE sv.org_id = org_billing.org_id
           AND sv.deleted_at IS NULL AND sv.status = 'running') AS running_servers
  FROM org_billing
 WHERE status IN ('past_due', 'canceled')
 ORDER BY status_since;
```

## Configuration

```
CP_PADDLE_API_KEY=pdl_...              # empty = billing off
CP_PADDLE_WEBHOOK_SECRET=pdl_ntfset_...# empty = webhook receiver 503
CP_PADDLE_ENV=sandbox                  # sandbox | production
CP_PADDLE_PRICE_ID=pri_...             # the connected-server price
```

Point the Paddle webhook at `https://<cp-host>/v1/webhooks/paddle` and set the
notification-setting secret as `CP_PADDLE_WEBHOOK_SECRET`. Use `sandbox` with
Paddle sandbox keys to verify checkout + webhooks end-to-end before flipping
`CP_PADDLE_ENV=production`.
