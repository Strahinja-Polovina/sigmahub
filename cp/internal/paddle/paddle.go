// Package paddle is the outbound Paddle Billing client and inbound webhook
// signature verification (P2-4). Bounded HTTP client, sandbox/production base
// URL, Bearer API key — mirrors the githubapp AppAuth pattern. A nil *Client
// means billing is not configured; callers degrade to an honest not-configured
// state rather than pretending to charge.
package paddle

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	ProductionAPIBase = "https://api.paddle.com"
	SandboxAPIBase    = "https://sandbox-api.paddle.com"
)

// Client calls the Paddle Billing API.
type Client struct {
	apiKey  string
	APIBase string
	HTTP    *http.Client
}

// NewClient returns a client for the given environment ("sandbox" or
// "production") and API key. Returns nil when the key is empty (billing off).
func NewClient(env, apiKey string) *Client {
	if apiKey == "" {
		return nil
	}
	base := ProductionAPIBase
	if env == "sandbox" {
		base = SandboxAPIBase
	}
	return &Client{apiKey: apiKey, APIBase: base, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.APIBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("paddle request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("paddle %s %s: status %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("paddle decode: %w", err)
		}
	}
	return nil
}

// CreateTransactionInput starts a hosted checkout for a quantity of the
// connected-server price.
type CreateTransactionInput struct {
	PriceID    string
	Quantity   int
	CustomerID string // optional; links to an existing Paddle customer
	OrgID      string // round-tripped in custom_data for webhook correlation
}

// Transaction is the created checkout's identifiers + hosted URL.
type Transaction struct {
	ID          string
	CustomerID  string
	CheckoutURL string
}

// CreateCheckout creates a transaction and returns its hosted-checkout URL.
func (c *Client) CreateCheckout(ctx context.Context, in CreateTransactionInput) (Transaction, error) {
	body := map[string]any{
		"items": []map[string]any{
			{"price_id": in.PriceID, "quantity": in.Quantity},
		},
		"custom_data": map[string]string{"orgId": in.OrgID},
	}
	if in.CustomerID != "" {
		body["customer_id"] = in.CustomerID
	}
	var out struct {
		Data struct {
			ID         string `json:"id"`
			CustomerID string `json:"customer_id"`
			Checkout   struct {
				URL string `json:"url"`
			} `json:"checkout"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/transactions", body, &out); err != nil {
		return Transaction{}, err
	}
	return Transaction{ID: out.Data.ID, CustomerID: out.Data.CustomerID, CheckoutURL: out.Data.Checkout.URL}, nil
}

// UpdateSubscriptionQuantity syncs a subscription's billed quantity to the
// current billable-server count (prorated by Paddle).
func (c *Client) UpdateSubscriptionQuantity(ctx context.Context, subscriptionID, priceID string, quantity int) error {
	body := map[string]any{
		"items": []map[string]any{
			{"price_id": priceID, "quantity": quantity},
		},
		"proration_billing_mode": "prorated_immediately",
	}
	return c.do(ctx, http.MethodPatch, "/subscriptions/"+subscriptionID, body, nil)
}

// SetSubscriptionOrg stamps the org id into the SUBSCRIPTION's own custom_data.
//
// CreateCheckout puts orgId on the checkout TRANSACTION, and that is the only
// place it lived: a renewal transaction, a portal cancellation and a support
// edit in the Paddle dashboard all reach the webhook receiver without it
// (SIGMA-293). The receiver now falls back to the stored subscription/customer
// ids, but the fallback is a repair — this makes the primary correlation path
// work for every later event on the subscription, so an org whose org_billing
// row is somehow missing an id is still identifiable.
func (c *Client) SetSubscriptionOrg(ctx context.Context, subscriptionID, orgID string) error {
	return c.do(ctx, http.MethodPatch, "/subscriptions/"+subscriptionID,
		map[string]any{"custom_data": map[string]string{"orgId": orgID}}, nil)
}

// CustomerPortalURL returns a management-portal session URL for a customer.
func (c *Client) CustomerPortalURL(ctx context.Context, customerID string) (string, error) {
	var out struct {
		Data struct {
			URLs struct {
				General struct {
					Overview string `json:"overview"`
				} `json:"general"`
			} `json:"urls"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/customers/"+customerID+"/portal-sessions", map[string]any{}, &out); err != nil {
		return "", err
	}
	return out.Data.URLs.General.Overview, nil
}

// Sign builds a Paddle-Signature header value for the given body and unix
// timestamp — the inverse of VerifySignature (used by senders and tests).
func Sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + ":"))
	mac.Write(body)
	return fmt.Sprintf("ts=%d;h1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// SignForTest is an alias kept for handler tests in other packages.
func SignForTest(secret string, ts int64, body []byte) string { return Sign(secret, ts, body) }

// VerifySignature checks a Paddle-Signature header ("ts=<unix>;h1=<hex>"): the
// HMAC-SHA256 keyed by the webhook secret over "<ts>:<rawBody>", constant-time
// compared to h1, with the timestamp inside maxSkew of now to bound replay.
func VerifySignature(secret string, body []byte, header string, now time.Time, maxSkew time.Duration) bool {
	if secret == "" {
		return false
	}
	var ts, h1 string
	for _, part := range strings.Split(header, ";") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "ts":
			ts = kv[1]
		case "h1":
			h1 = kv[1]
		}
	}
	if ts == "" || h1 == "" {
		return false
	}
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	delta := now.Unix() - tsInt
	if delta < 0 {
		delta = -delta
	}
	if time.Duration(delta)*time.Second > maxSkew {
		return false
	}
	want, err := hex.DecodeString(h1)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + ":"))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}
