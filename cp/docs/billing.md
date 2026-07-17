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

1. **Checkout** — `POST /v1/orgs/{orgId}/billing/checkout` (Org Admin) creates
   a Paddle hosted-checkout transaction for the org's current billable
   quantity and returns its URL. The org id rides `custom_data` for webhook
   correlation.
2. **Webhook** — `POST /v1/webhooks/paddle` (public, Paddle-Signature verified,
   idempotent via `webhook_deliveries`) applies `subscription.*` and
   `transaction.payment_failed` events to `org_billing`.
3. **Portal** — `POST /v1/orgs/{orgId}/billing/portal` (Org Admin) returns a
   Paddle customer-portal URL to manage the payment method / subscription.

A `past_due` transition enqueues a `payment_failed` **alert** (P2-6) so the org
is notified through its channels. Grace-period behaviour is honest: servers
keep running; nothing is paused automatically.

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
