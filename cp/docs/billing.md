# Billing (P2-4, Paddle)

SigmaHub bills **units**, not servers. A connected server is worth a number of
units that tracks how expensive it is to manage — an ordinary server is one
unit, a Kubernetes node two, a GPU server four — and you pay a flat price per
unit per month above a free allowance. Billing runs through
[Paddle](https://www.paddle.com/) as the merchant of record, so VAT/sales tax is
handled Paddle-side.

> This section used to price a flat €5 per *server* with the first three
> servers free, which predated the unit weights and put a three-GPU fleet at €0
> instead of €45 (SIGMA-296). The tables below are read by
> `TestBillingDocMatchesPricingConstants` and compared
> to `store.BillingUnitPrice`, `store.BillingFreeTier` and the catalog's weight
> table, so the prose can no longer drift on its own. Edit the constants, not
> just this file.

## The price

| Setting | Value |
| --- | --- |
| Unit price | €5 per unit per month |
| Free tier | 3 units, always included |
| Currency | EUR |

### What a server is worth

| Server type | Units each |
| --- | --- |
| `general` | 1 |
| `vps` | 1 |
| `database` | 1 |
| `storage` | 1 |
| `gpu` | 4 |
| `k8s` | 2 |
| `build` | 1 |

A type this table does not name bills as one unit — never as zero, so a typo in
a type string cannot silently make a server free.

The billable quantity is `max(0, units − 3)`, where `units` is the **weighted**
total, not the server count. An all-general fleet therefore prices exactly as it
did before weights existed: "your first three servers are free" stays literally
true for ordinary servers, and only for those.

### Worked example: a mixed fleet

| What is connected | Units |
| --- | --- |
| 2 general servers | 2 × 1 = 2 |
| 1 database server | 1 × 1 = 1 |
| 3 Kubernetes nodes | 3 × 2 = 6 |
| 1 GPU server | 1 × 4 = 4 |
| **Total** | **13** |

13 units − 3 free = 10 billable × €5 = **€50/month** for 7 servers.

And the case the old doc got wrong: **three GPU servers** are 12 units, not
three free servers. 12 − 3 = 9 × €5 = **€45/month**.

### What the subscription is priced on

The quantity pushed to Paddle is the **high-water mark** of the weighted unit
total over the last 24 hours, minus the free tier. It is deliberately not the
bare point-in-time count: servers flip to `unreachable` on a missed heartbeat,
so a network blip across a fleet would otherwise scale the subscription down —
with an immediate proration credit — and back up minutes later. A scale-up takes
effect immediately; a scale-down takes effect a day later.

That same number is what the Billing page shows as "Total due" and what the
checkout transaction carries, computed by one function so the three cannot fork
(`store.BillableQuantity` / `BilledUnitsForOrg`, SIGMA-292). One exception is
worth knowing: an active subscription cannot bill zero units, because Paddle
rejects a zero-quantity item, so a fleet that shrinks into the free tier while
subscribed still bills a one-unit minimum. The Billing page shows that minimum
as its own line rather than claiming nothing is due; cancelling in the customer
portal is what stops it.

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
server count, the weighted unit total, the units it is billed on (the 24-hour
high-water mark), the billable quantity `max(0, units − 3)`, the monthly charge,
and the accrued server-hours this month for reconciliation.

`incompatible` and `decommissioning` hosts are not `running`, so neither is
metered: a host that cannot do the job it was enrolled for is not billed, and
neither is one the operator has told us to stop using.

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
CP_PADDLE_PRICE_ID=pri_...             # the per-UNIT price (quantity = units)
```

Point the Paddle webhook at `https://<cp-host>/v1/webhooks/paddle` and set the
notification-setting secret as `CP_PADDLE_WEBHOOK_SECRET`. Use `sandbox` with
Paddle sandbox keys to verify checkout + webhooks end-to-end before flipping
`CP_PADDLE_ENV=production`.
