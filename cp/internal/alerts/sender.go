// Package alerts delivers P2-6 notifications: a Sender that speaks each
// channel kind's transport, and a dispatcher loop that drains the store's
// alert outbox with retry/backoff. Channel secrets arrive already unwrapped
// (store.AlertChannelForSend) and live only in memory for the send.
package alerts

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
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TelegramAPIBase is overridable in tests.
const TelegramAPIBase = "https://api.telegram.org"

// Sender delivers one alert on one channel. Safe for concurrent use.
type Sender struct {
	HTTP *http.Client
	// telegramBase overrides the Telegram API root in tests.
	telegramBase string
	// sendMail is swapped in tests; production uses smtp.SendMail.
	sendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

// NewSender returns a Sender with bounded transports.
func NewSender() *Sender {
	return &Sender{
		HTTP:         &http.Client{Timeout: 10 * time.Second},
		telegramBase: TelegramAPIBase,
		sendMail:     smtp.SendMail,
	}
}

// Send delivers title/body on the channel. The error is what lands in the
// outbox row's last_error — keep it actionable, never include the secret.
func (s *Sender) Send(ctx context.Context, ch store.AlertChannelSend, event, title, body string) error {
	switch ch.Kind {
	case "slack":
		return s.sendSlack(ctx, ch, title, body)
	case "telegram":
		return s.sendTelegram(ctx, ch, title, body)
	case "webhook":
		return s.sendWebhook(ctx, ch, event, title, body)
	case "email":
		return s.sendEmail(ch, title, body)
	default:
		return fmt.Errorf("unknown channel kind %q", ch.Kind)
	}
}

// post sends JSON and demands a 2xx, returning a truncated body on failure.
func (s *Sender) post(ctx context.Context, url string, payload any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// sendSlack posts to an incoming-webhook URL (the channel secret).
func (s *Sender) sendSlack(ctx context.Context, ch store.AlertChannelSend, title, body string) error {
	if ch.Secret == "" {
		return fmt.Errorf("slack channel has no webhook URL configured")
	}
	if err := s.post(ctx, ch.Secret, map[string]string{
		"text": "*" + title + "*\n" + body,
	}); err != nil {
		// The webhook URL is the secret — make sure it can't ride an error.
		return fmt.Errorf("slack webhook: %s", scrub(err.Error(), ch.Secret))
	}
	return nil
}

type telegramConfig struct {
	ChatID string `json:"chatId"`
}

// sendTelegram calls the Bot API's sendMessage (bot token = channel secret).
func (s *Sender) sendTelegram(ctx context.Context, ch store.AlertChannelSend, title, body string) error {
	var cfg telegramConfig
	_ = json.Unmarshal(ch.Config, &cfg)
	if ch.Secret == "" || cfg.ChatID == "" {
		return fmt.Errorf("telegram channel needs a bot token (secret) and a chatId")
	}
	base := s.telegramBase
	if base == "" {
		base = TelegramAPIBase
	}
	u := base + "/bot" + url.PathEscape(ch.Secret) + "/sendMessage"
	if err := s.post(ctx, u, map[string]string{
		"chat_id": cfg.ChatID,
		"text":    title + "\n" + body,
	}); err != nil {
		return fmt.Errorf("telegram: %s", scrub(err.Error(), ch.Secret))
	}
	return nil
}

type webhookConfig struct {
	URL string `json:"url"`
}

// WebhookPayload is the JSON a generic webhook receives. When the channel has
// a signing key, X-Sigmahub-Signature-256 carries "sha256=<hex hmac>" over
// the exact request body — the same scheme SigmaHub verifies on inbound
// GitHub webhooks, so receivers can reuse standard verification code.
type WebhookPayload struct {
	Event string    `json:"event"`
	Title string    `json:"title"`
	Body  string    `json:"body"`
	At    time.Time `json:"at"`
}

func (s *Sender) sendWebhook(ctx context.Context, ch store.AlertChannelSend, event, title, body string) error {
	var cfg webhookConfig
	_ = json.Unmarshal(ch.Config, &cfg)
	if cfg.URL == "" {
		return fmt.Errorf("webhook channel has no url configured")
	}
	buf, err := json.Marshal(WebhookPayload{Event: event, Title: title, Body: body, At: time.Now().UTC()})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if ch.Secret != "" {
		mac := hmac.New(sha256.New, []byte(ch.Secret))
		mac.Write(buf)
		req.Header.Set("X-Sigmahub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook: status %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

type emailConfig struct {
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	Username string   `json:"username"`
}

// sendEmail delivers over SMTP (STARTTLS when the server offers it; use port
// 587 — implicit-TLS 465 is not supported). Password = channel secret.
func (s *Sender) sendEmail(ch store.AlertChannelSend, title, body string) error {
	var cfg emailConfig
	_ = json.Unmarshal(ch.Config, &cfg)
	if cfg.Host == "" || cfg.From == "" || len(cfg.To) == 0 {
		return fmt.Errorf("email channel needs host, from and at least one recipient")
	}
	port := cfg.Port
	if port == 0 {
		port = 587
	}
	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, ch.Secret, cfg.Host)
	}
	msg := buildEmailMessage(cfg.From, cfg.To, title, body)
	if err := s.sendMail(fmt.Sprintf("%s:%d", cfg.Host, port), auth, cfg.From, cfg.To, msg); err != nil {
		return fmt.Errorf("smtp: %s", scrub(err.Error(), ch.Secret))
	}
	return nil
}

// buildEmailMessage renders a minimal RFC 5322 plain-text message.
func buildEmailMessage(from string, to []string, title, body string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: [SigmaHub] " + sanitizeHeader(title) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(title + "\n\n" + body + "\n")
	return []byte(b.String())
}

// sanitizeHeader strips CR/LF so a crafted alert title can't inject headers.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
}

// scrub removes a secret from an error string before it is persisted.
func scrub(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}
