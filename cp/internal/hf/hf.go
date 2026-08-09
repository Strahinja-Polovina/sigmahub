// Package hf is the control plane's read-only window onto the Hugging Face Hub,
// and the one place in SigmaHub where a model's VRAM requirement is computed.
//
// Two jobs live in one package because they are the same question asked twice.
// The model picker in the New Resource wizard has to LIST models, and the deploy
// path has to know whether the chosen model FITS on the GPU the user picked;
// both answers come out of the same repository metadata. Derived in two places —
// Go for the gate, TypeScript for the badge — they would eventually disagree,
// and the number the user was shown would not be the number that refused their
// deploy. So the client hands back a fully sized ModelCard: the byte count AND
// the string that renders it are produced here and travel over the wire together
// (SIGMA-213, SIGMA-214). The dashboard compares numbers; it never re-derives
// them.
//
// The Hub is a third party on the far side of the internet sitting on the
// latency path of a search-as-you-type box. Everything below that looks
// defensive — the bounded cache, the caching of NEGATIVE answers, the byte cap
// on every response, the timeout that http.DefaultClient does not have — is
// there because a control plane that lets an upstream decide how much memory it
// allocates or how long it blocks is a control plane that goes down when that
// upstream has a bad day.
package hf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the public Hub. It is a field on Client rather than a
// constant reference so tests can point at an httptest server and an air-gapped
// install can point at a mirror.
const DefaultBaseURL = "https://huggingface.co"

// DefaultTTL is how long a Hub answer is reused. Model metadata changes on the
// scale of a release, and the cost of being five minutes stale is that a fresh
// upload takes five minutes to appear in the picker; the cost of a shorter TTL
// is paid on every keystroke of every user, forever.
const DefaultTTL = 5 * time.Minute

// maxCacheEntries bounds the cache. A search box that caches per keystroke
// produces one entry per prefix per user, so an unbounded map is a slow memory
// leak driven by strangers typing — the whole point of caching here is to spend
// a fixed amount of memory, not an unknown one.
const maxCacheEntries = 512

// maxResponseBytes caps what one Hub response may cost us. `full=true` search
// results carry the complete file listing of every repository, so the honest
// upper bound is generous — but it is an upper BOUND, because an unbounded
// io.ReadAll on a third party's response is how a control plane dies.
const maxResponseBytes = 8 << 20

const (
	// defaultSearchLimit is what an unspecified limit becomes: enough rows to
	// scroll, few enough to render instantly.
	defaultSearchLimit = 20
	// maxSearchLimit caps what a caller may ask for. The Hub will happily serve
	// a limit of 10000 full model records; nobody picks a model out of 10000
	// rows, and buffering them would be entirely our problem.
	maxSearchLimit = 50
)

// ErrNotFound is returned (wrapped, with the id and the fix) when the Hub has no
// such repository. The API layer tests for it with errors.Is so a typo in the
// picker answers 404 — a truthful "no such model" — instead of 502, which would
// read to the user as "SigmaHub is broken".
var ErrNotFound = errors.New("model not found on the Hugging Face Hub")

// defaultHTTPClient is deliberately NOT http.DefaultClient: that one has no
// timeout, so a Hub connection that opens and then stalls would pin the request
// goroutine (and the user's spinner) until the process restarts.
var defaultHTTPClient = &http.Client{Timeout: 15 * time.Second}

// ModelCard is one model as the dashboard sees it: what the Hub says about the
// repository, plus what this package decided about running it. Everything from
// Engine down is derived here, not stored anywhere and not recomputed anywhere
// else. The JSON tags are the wire contract with the dashboard — changing one is
// changing an API.
type ModelCard struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Gated       bool   `json:"gated"`
	Downloads   int    `json:"downloads"`
	Likes       int    `json:"likes"`
	PipelineTag string `json:"pipelineTag"`
	Library     string `json:"library"`
	// Engine is the runtime the control plane would render for this model. The
	// wizard used to ask; the metadata already knows (see EngineForModel).
	Engine string `json:"engine"`
	// Parameters is 0 and ParametersKnown false when nothing in the metadata or
	// the repository name reveals a size. That pair is load-bearing: it is what
	// switches the fit check OFF rather than guessing a number to gate on.
	Parameters      uint64  `json:"parameters"`
	ParametersKnown bool    `json:"parametersKnown"`
	Quantization    string  `json:"quantization"`
	BytesPerParam   float64 `json:"bytesPerParam"`
	// VRAMBytesRequired is what the fit check compares against the GPU's memory.
	// VRAMText is the same number rendered once, CP-side, so the sentence in the
	// picker and the sentence in the refusal cannot say different things.
	VRAMBytesRequired uint64 `json:"vramBytesRequired"`
	VRAMText          string `json:"vramText"`
	SizingBasis       string `json:"sizingBasis"`
}

// Client reads the Hub through a bounded, TTL'd cache. It carries a mutex, so it
// is used as a pointer and shared: one Client per process is the point — a
// per-request Client would have an empty cache every time.
type Client struct {
	// HTTP is the transport; nil uses a shared client that HAS a timeout.
	HTTP *http.Client
	// BaseURL overrides the public Hub; empty means DefaultBaseURL.
	BaseURL string
	// Token is a Hub read token. Empty is a supported configuration — public
	// models resolve without it — but gated repositories (Llama & co) do not.
	Token string
	// TTL overrides DefaultTTL.
	TTL time.Duration
	// Now is injectable so cache expiry can be TESTED rather than slept through.
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
	// order is the eviction queue: insertion order of the keys currently held,
	// oldest first.
	order []string
}

type cacheEntry struct {
	cards []ModelCard
	// err is non-nil only for a cached ErrNotFound. Transport failures are
	// never cached — a five-minute network blip must not become a five-minute
	// outage of the picker.
	err     error
	expires time.Time
}

// TokenConfigured reports whether the control plane has Hub credentials. The
// picker publishes this so it can explain a gated model with the actual fix
// ("an operator must set CP_HUGGING_FACE_TOKEN" vs "accept the licence on the
// model page with the account that issued the token") instead of a bare failure.
func (c *Client) TokenConfigured() bool { return strings.TrimSpace(c.Token) != "" }

// Search lists models matching a free-text query, already sized. An empty query
// is legal and returns the Hub's own ordering, which is what fills the picker
// before the user has typed anything.
//
// The returned slice is never nil: an empty result must serialise as [] so the
// dashboard renders "no matches", not a null it has to defend against.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]ModelCard, error) {
	query = strings.TrimSpace(query)
	limit = clampLimit(limit)
	key := "search:" + strconv.Itoa(limit) + ":" + query

	if entry, ok := c.cached(key); ok {
		return entry.cards, entry.err
	}

	q := url.Values{}
	q.Set("search", query)
	q.Set("limit", strconv.Itoa(limit))
	// full=true is what carries safetensors.parameters, and safetensors is the
	// only EXACT parameter count the Hub gives us. Without it every card in the
	// list would be sized off its name.
	q.Set("full", "true")

	body, status, err := c.do(ctx, "/api/models?"+q.Encode(), "model search")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.statusError("the model search", status)
	}
	var raw []apiModel
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("hugging face model search returned something that is not a model list: %w", err)
	}
	cards := make([]ModelCard, 0, len(raw))
	for _, m := range raw {
		card, ok := cardFromAPI(m)
		if !ok {
			// A record with no id cannot be selected, deployed or shown; it is
			// not an error, it is one row worth dropping.
			continue
		}
		cards = append(cards, card)
	}
	c.remember(key, cards, nil)
	return cards, nil
}

// Resolve returns one model by repository id.
//
// A gated model resolved WITHOUT a token still comes back as a ModelCard with
// Gated set, built from what the id alone reveals. That is deliberate and it is
// the opposite of what the status code suggests: the Hub answers 401/403, but
// answering the dashboard with an error would make the model VANISH from a
// picker that had just listed it (search results carry gated repositories and
// their flag). The user's problem is a missing token or an unaccepted licence,
// and they can only be told that if the model is still on screen to be told
// about.
func (c *Client) Resolve(ctx context.Context, repoID string) (ModelCard, error) {
	id := strings.Trim(strings.TrimSpace(repoID), "/")
	if !validRepoID(id) {
		return ModelCard{}, fmt.Errorf("%w: %q is not a repository id — use owner/name exactly as it appears in the model's URL, e.g. meta-llama/Llama-3.1-8B-Instruct", ErrNotFound, repoID)
	}
	key := "resolve:" + id
	if entry, ok := c.cached(key); ok {
		if entry.err != nil {
			return ModelCard{}, entry.err
		}
		if len(entry.cards) == 1 {
			return entry.cards[0], nil
		}
	}

	segments := strings.Split(id, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	body, status, err := c.do(ctx, "/api/models/"+strings.Join(segments, "/"), "model "+id)
	if err != nil {
		return ModelCard{}, err
	}

	switch status {
	case http.StatusOK:
		var m apiModel
		if err := json.Unmarshal(body, &m); err != nil {
			return ModelCard{}, fmt.Errorf("hugging face returned something that is not a model for %s: %w", id, err)
		}
		if m.ID == "" && m.ModelID == "" {
			// Echo the id we asked for rather than shipping a card with no id:
			// the dashboard keys deploys off it.
			m.ID = id
		}
		card, _ := cardFromAPI(m)
		c.remember(key, []ModelCard{card}, nil)
		return card, nil

	case http.StatusNotFound:
		// Cached like any other answer. Without this, a repository id typed into
		// a search-as-you-type box costs the Hub one request per KEYSTROKE, and
		// every one of them is a request we already know the answer to.
		notFound := fmt.Errorf("%w: %s — check the repository id, or set CP_HUGGING_FACE_TOKEN if the repository is private", ErrNotFound, id)
		c.remember(key, nil, notFound)
		return ModelCard{}, notFound

	case http.StatusUnauthorized, http.StatusForbidden:
		card := gatedCard(id)
		c.remember(key, []ModelCard{card}, nil)
		return card, nil

	default:
		return ModelCard{}, c.statusError("model "+id, status)
	}
}

// gatedCard is everything that can be honestly said about a repository the Hub
// will not describe to us: it exists, it is gated, and its NAME still sizes it
// (see ParseParameterCount). Sizing basis "name" is exactly the signal the
// dashboard needs to soften the fit check for it.
func gatedCard(id string) ModelCard {
	card := ModelCard{ID: id, Name: displayName(id), Gated: true}
	applySizing(&card, nil, "", 0)
	return card
}

// apiModel is the subset of the Hub's model record this package understands.
type apiModel struct {
	ID      string `json:"id"`
	ModelID string `json:"modelId"`
	// Gated arrives as `false` OR as the string "auto"/"manual" — the same key
	// with two JSON types, which is why it is not a bool here. Decoding it into
	// a bool errors on the string form and decoding into a string errors on
	// `false`; either mistake silently drops the one flag that tells the user
	// why the download will fail.
	Gated       json.RawMessage `json:"gated"`
	Likes       int             `json:"likes"`
	Downloads   int             `json:"downloads"`
	PipelineTag string          `json:"pipeline_tag"`
	LibraryName string          `json:"library_name"`
	Tags        []string        `json:"tags"`
	Safetensors struct {
		// Parameters is element counts per dtype, e.g. {"BF16": 8030261248}.
		Parameters map[string]uint64 `json:"parameters"`
		Total      uint64            `json:"total"`
	} `json:"safetensors"`
}

func cardFromAPI(m apiModel) (ModelCard, bool) {
	id := m.ID
	if id == "" {
		id = m.ModelID
	}
	if id == "" {
		return ModelCard{}, false
	}
	card := ModelCard{
		ID:          id,
		Name:        displayName(id),
		Gated:       gatedFlag(m.Gated),
		Downloads:   m.Downloads,
		Likes:       m.Likes,
		PipelineTag: m.PipelineTag,
		Library:     m.LibraryName,
	}
	applySizing(&card, m.Tags, dominantDType(m.Safetensors.Parameters), m.Safetensors.Total)
	return card, true
}

func gatedFlag(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "false" && s != "null"
}

// displayName turns a repository id into something readable. The Hub has no
// display-name field at all — the id IS the name everywhere on huggingface.co —
// so "meta-llama/Llama-3.1-8B-Instruct" becomes "Llama 3.1 8B Instruct" here
// rather than each surface inventing its own prettifier.
func displayName(repoID string) string {
	name := repoID
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.NewReplacer("-", " ", "_", " ").Replace(name)
	return strings.Join(strings.Fields(name), " ")
}

// validRepoID accepts what the Hub itself accepts: `name` or `owner/name` over a
// conservative character set. It is also the guard that keeps a crafted id from
// walking out of the /api/models/ prefix — ".." and control characters never
// reach the URL.
func validRepoID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	segments := strings.Split(id, "/")
	if len(segments) > 2 {
		return false
	}
	for _, s := range segments {
		if s == "" || s == "." || s == ".." {
			return false
		}
		for i := 0; i < len(s); i++ {
			ch := s[i]
			switch {
			case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
			case ch == '-', ch == '_', ch == '.':
			default:
				return false
			}
		}
	}
	return true
}

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultSearchLimit
	case limit > maxSearchLimit:
		return maxSearchLimit
	default:
		return limit
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultTTL
}

// cached returns a live entry, with the cards copied so a caller that sorts or
// truncates the slice it was handed cannot rewrite everyone else's cache hit.
//
// An expired entry is reported as a miss and LEFT IN PLACE rather than deleted:
// the fetch that follows overwrites it under the same key, which keeps the
// eviction queue free of duplicate keys. Deleting here would let the same key be
// queued twice and evict its own live entry early.
func (c *Client) cached(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[key]
	if !ok || !c.now().Before(entry.expires) {
		return cacheEntry{}, false
	}
	cards := make([]ModelCard, len(entry.cards))
	copy(cards, entry.cards)
	entry.cards = cards
	return entry, true
}

func (c *Client) remember(key string, cards []ModelCard, err error) {
	stored := make([]ModelCard, len(cards))
	copy(stored, cards)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = make(map[string]cacheEntry, maxCacheEntries)
	}
	if _, exists := c.cache[key]; !exists {
		if len(c.order) >= maxCacheEntries {
			delete(c.cache, c.order[0])
			c.order = c.order[1:]
		}
		c.order = append(c.order, key)
	}
	c.cache[key] = cacheEntry{cards: stored, err: err, expires: c.now().Add(c.ttl())}
}

// do performs one Hub request and returns the (capped) body and status. The
// mutex is NOT held across this call — a slow Hub must not stop every other
// request in the process from reading the cache.
func (c *Client) do(ctx context.Context, path, what string) ([]byte, int, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	// The token is what makes gated repositories resolve at all: without it the
	// Hub answers 401/403 for Llama & co, and the picker can only show the name
	// it already had.
	if c.TokenConfigured() {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.Token))
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = defaultHTTPClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot reach the Hugging Face Hub for %s — check the control plane's outbound network access: %w", what, err)
	}
	defer resp.Body.Close()

	// Read one byte past the cap so an oversize body is DETECTED rather than
	// silently truncated into a JSON parse error that names the wrong cause.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading the Hugging Face response for %s: %w", what, err)
	}
	if len(body) > maxResponseBytes {
		return nil, resp.StatusCode, fmt.Errorf("the Hugging Face response for %s is larger than the %d byte cap the control plane will buffer — narrow the search, or resolve the model by id", what, maxResponseBytes)
	}
	return body, resp.StatusCode, nil
}

// statusError turns a Hub status into a sentence that names the fix. Which fix
// depends on whether we have a token at all, and the operator cannot tell those
// two situations apart from a bare 403.
func (c *Client) statusError(what string, status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		if c.TokenConfigured() {
			return fmt.Errorf("hugging face refused %s (%d) — the configured CP_HUGGING_FACE_TOKEN is not valid for it; issue a fresh read token at huggingface.co/settings/tokens and accept the model's licence with that account", what, status)
		}
		return fmt.Errorf("hugging face refused %s (%d) — set CP_HUGGING_FACE_TOKEN to a Hub read token so gated and private models can be read", what, status)
	case http.StatusTooManyRequests:
		return fmt.Errorf("hugging face rate-limited %s — set CP_HUGGING_FACE_TOKEN to raise the limit, or retry in a minute", what)
	default:
		return fmt.Errorf("hugging face returned status %d for %s", status, what)
	}
}
