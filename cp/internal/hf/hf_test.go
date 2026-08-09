package hf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Raw Hub records, kept as JSON text rather than built from maps, because the
// two shapes this package has to survive are shapes only the real API produces:
// `gated` carrying a STRING where a bool is implied, and safetensors listing
// index tensors beside the weights.
const (
	llama8B = `{
		"id": "meta-llama/Llama-3.1-8B-Instruct",
		"modelId": "meta-llama/Llama-3.1-8B-Instruct",
		"likes": 4321,
		"downloads": 1234567,
		"gated": "manual",
		"pipeline_tag": "text-generation",
		"library_name": "transformers",
		"tags": ["transformers","safetensors","llama","text-generation","conversational"],
		"safetensors": {"parameters": {"BF16": 8030261248, "I64": 8192}, "total": 8030269440}
	}`

	llama7BGGUF = `{
		"id": "TheBloke/Llama-2-7B-Chat-GGUF",
		"modelId": "TheBloke/Llama-2-7B-Chat-GGUF",
		"likes": 900,
		"downloads": 50000,
		"gated": false,
		"pipeline_tag": "text-generation",
		"library_name": "gguf",
		"tags": ["gguf","llama","text-generation"]
	}`
)

// stubHub serves the two Hub endpoints this package calls, counts requests (so a
// cache hit is provable rather than assumed) and records what it was asked with.
type stubHub struct {
	models map[string]string
	// gated ids answer 401 to an unauthenticated request, which is what the Hub
	// does for a repository whose licence has not been accepted.
	gated map[string]bool
	// wantToken, when set, makes every unauthenticated request 401.
	wantToken string

	mu    sync.Mutex
	calls int
	auths []string
}

func (h *stubHub) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		h.mu.Lock()
		h.calls++
		h.auths = append(h.auths, auth)
		h.mu.Unlock()

		if h.wantToken != "" && auth != "Bearer "+h.wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/models")
		w.Header().Set("Content-Type", "application/json")

		if rest == "" || rest == "/" {
			query := strings.ToLower(r.URL.Query().Get("search"))
			var matched []string
			for id, raw := range h.models {
				if query == "" || strings.Contains(strings.ToLower(id), query) {
					matched = append(matched, raw)
				}
			}
			// Deterministic order: the client must not depend on it, but a test
			// that indexes the result must.
			sortRawByID(matched)
			_, _ = fmt.Fprint(w, "["+strings.Join(matched, ",")+"]")
			return
		}

		id := strings.TrimPrefix(rest, "/")
		if h.gated[id] && auth == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw, ok := h.models[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":"Repository not found"}`)
			return
		}
		_, _ = fmt.Fprint(w, raw)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (h *stubHub) requests() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (h *stubHub) authHeaders() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.auths...)
}

func sortRawByID(raw []string) {
	for i := 1; i < len(raw); i++ {
		for j := i; j > 0 && raw[j] < raw[j-1]; j-- {
			raw[j], raw[j-1] = raw[j-1], raw[j]
		}
	}
}

func newClient(t *testing.T, hub *stubHub, token string) *Client {
	t.Helper()
	srv := hub.start(t)
	return &Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: token}
}

// A search result is not a Hub record passed through: it is a decision about how
// to RUN the model. Each field below is one the wizard no longer has to ask for.
func TestSearchTurnsHubRecordsIntoRunnableModelCards(t *testing.T) {
	hub := &stubHub{models: map[string]string{
		"meta-llama/Llama-3.1-8B-Instruct": llama8B,
		"TheBloke/Llama-2-7B-Chat-GGUF":    llama7BGGUF,
	}}
	c := newClient(t, hub, "")

	cards, err := c.Search(context.Background(), "llama", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("got %d cards, want 2: %+v", len(cards), cards)
	}
	byID := map[string]ModelCard{}
	for _, card := range cards {
		byID[card.ID] = card
	}

	safetensors := byID["meta-llama/Llama-3.1-8B-Instruct"]
	if safetensors.Name != "Llama 3.1 8B Instruct" {
		t.Errorf("name = %q, want the readable form of the repo id", safetensors.Name)
	}
	if !safetensors.Gated {
		t.Error(`gated = false, but the Hub reported gated:"manual" — the picker cannot explain a licence it does not know about`)
	}
	if safetensors.Downloads != 1234567 || safetensors.Likes != 4321 {
		t.Errorf("downloads/likes = %d/%d, want 1234567/4321", safetensors.Downloads, safetensors.Likes)
	}
	if safetensors.Engine != "vllm" {
		t.Errorf("engine = %q, want vllm for a safetensors repo", safetensors.Engine)
	}
	if safetensors.SizingBasis != "safetensors" || safetensors.Parameters != 8030269440 {
		t.Errorf("sizing = %s/%d, want the exact safetensors total", safetensors.SizingBasis, safetensors.Parameters)
	}
	if safetensors.BytesPerParam != 2 {
		t.Errorf("bytesPerParam = %v, want 2 for BF16", safetensors.BytesPerParam)
	}
	if safetensors.VRAMText != "~21 GB" {
		t.Errorf("vramText = %q, want ~21 GB", safetensors.VRAMText)
	}

	// The GGUF repository is the decision-removal in one row: no safetensors, so
	// it is sized off its name, and its format — not a question to the user —
	// selects the runtime.
	gguf := byID["TheBloke/Llama-2-7B-Chat-GGUF"]
	if gguf.Engine != "ollama" {
		t.Errorf("engine = %q, want ollama for GGUF weights", gguf.Engine)
	}
	if gguf.Quantization != "gguf" || gguf.BytesPerParam != 0.6 {
		t.Errorf("quantization/bytesPerParam = %s/%v, want gguf/0.6", gguf.Quantization, gguf.BytesPerParam)
	}
	if gguf.SizingBasis != "name" || gguf.Parameters != 7_000_000_000 {
		t.Errorf("sizing = %s/%d, want name/7e9", gguf.SizingBasis, gguf.Parameters)
	}
	if gguf.Gated {
		t.Error("gated = true, but the Hub reported gated:false")
	}
}

// An empty result must serialise as [] and not null: the picker renders "no
// matches" from a slice, and a null is a crash waiting for a rare query.
func TestSearchWithNoMatchesReturnsAnEmptyListNotNil(t *testing.T) {
	hub := &stubHub{models: map[string]string{"meta-llama/Llama-3.1-8B-Instruct": llama8B}}
	c := newClient(t, hub, "")

	cards, err := c.Search(context.Background(), "definitely-not-a-model", 10)
	if err != nil {
		t.Fatal(err)
	}
	if cards == nil {
		t.Fatal("a search with no matches returned nil, which serialises as null")
	}
	if len(cards) != 0 {
		t.Fatalf("got %d cards, want none", len(cards))
	}
}

func TestResolveReportsAnUnknownRepositoryAsNotFound(t *testing.T) {
	hub := &stubHub{models: map[string]string{"meta-llama/Llama-3.1-8B-Instruct": llama8B}}
	c := newClient(t, hub, "")

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"a typo the Hub 404s", "meta-llama/Llama-3.1-8B-Instruc"},
		{"not a repository id at all", "not a repo id"},
		{"a path that would escape the models prefix", "../../datasets/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Resolve(context.Background(), tc.id)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound so the API answers 404 rather than 502", err)
			}
		})
	}

	if _, err := c.Resolve(context.Background(), "meta-llama/Llama-3.1-8B-Instruct"); err != nil {
		t.Fatalf("the model that does exist must still resolve: %v", err)
	}
}

// The token is the difference between a picker that can read Llama and one that
// cannot, and an absent token must not send an empty Authorization header — the
// Hub treats that as a malformed credential rather than as anonymous.
func TestTheTokenReachesTheAuthorizationHeaderAndItsAbsenceSendsNoHeader(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		want  string
	}{
		{"configured", "hf_secret", "Bearer hf_secret"},
		{"padded value is trimmed", "  hf_secret  ", "Bearer hf_secret"},
		{"absent", "", ""},
		{"blank is not a token", "   ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := &stubHub{models: map[string]string{"meta-llama/Llama-3.1-8B-Instruct": llama8B}}
			c := newClient(t, hub, tc.token)

			if _, err := c.Search(context.Background(), "llama", 5); err != nil {
				t.Fatal(err)
			}
			auths := hub.authHeaders()
			if len(auths) != 1 || auths[0] != tc.want {
				t.Fatalf("Authorization = %q, want %q", auths, tc.want)
			}
			if got := c.TokenConfigured(); got != (tc.want != "") {
				t.Errorf("TokenConfigured() = %v, want %v", got, tc.want != "")
			}
		})
	}
}

// The Hub refuses to describe a gated repository to an anonymous caller. Turning
// that into an error would make a model the user had just seen in the list
// disappear when they clicked it; they would learn nothing about the licence
// they have to accept. So the card survives the 401 with everything its name
// still tells us.
func TestAGatedModelWithoutATokenStillProducesACard(t *testing.T) {
	hub := &stubHub{
		models: map[string]string{"meta-llama/Llama-3.1-8B-Instruct": llama8B},
		gated:  map[string]bool{"meta-llama/Llama-3.1-8B-Instruct": true},
	}
	c := newClient(t, hub, "")

	card, err := c.Resolve(context.Background(), "meta-llama/Llama-3.1-8B-Instruct")
	if err != nil {
		t.Fatalf("a gated model must not fail to resolve: %v", err)
	}
	if !card.Gated {
		t.Error("gated = false — the one thing the user needs told")
	}
	if card.ID != "meta-llama/Llama-3.1-8B-Instruct" || card.Name != "Llama 3.1 8B Instruct" {
		t.Errorf("id/name = %q/%q, want them recovered from the requested id", card.ID, card.Name)
	}
	if card.SizingBasis != "name" || card.Parameters != 8_000_000_000 {
		t.Errorf("sizing = %s/%d, want name/8e9 from the repo id", card.SizingBasis, card.Parameters)
	}
	if card.Engine != "vllm" || card.VRAMText == "" {
		t.Errorf("card = %+v, want a runnable, sized card", card)
	}

	// With a token the same repository resolves fully, and the exact
	// safetensors count replaces the name estimate.
	hub2 := &stubHub{
		models:    map[string]string{"meta-llama/Llama-3.1-8B-Instruct": llama8B},
		wantToken: "hf_secret",
	}
	c2 := newClient(t, hub2, "hf_secret")
	full, err := c2.Resolve(context.Background(), "meta-llama/Llama-3.1-8B-Instruct")
	if err != nil {
		t.Fatal(err)
	}
	if full.SizingBasis != "safetensors" {
		t.Errorf("sizing basis = %q, want safetensors once the token lets us read the index", full.SizingBasis)
	}
}

func TestASecondIdenticalLookupIsAnsweredFromTheCache(t *testing.T) {
	hub := &stubHub{models: map[string]string{"meta-llama/Llama-3.1-8B-Instruct": llama8B}}
	c := newClient(t, hub, "")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := c.Search(ctx, "llama", 10); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Resolve(ctx, "meta-llama/Llama-3.1-8B-Instruct"); err != nil {
			t.Fatal(err)
		}
	}
	if got := hub.requests(); got != 2 {
		t.Fatalf("%d requests reached the Hub, want 2 (one search, one resolve)", got)
	}

	// A different limit is a different answer and must not be served from the
	// first one's entry.
	if _, err := c.Search(ctx, "llama", 5); err != nil {
		t.Fatal(err)
	}
	if got := hub.requests(); got != 3 {
		t.Fatalf("%d requests, want 3 — a different limit is a different query", got)
	}
}

// Cached forever is a bug; cached until a clock says otherwise is the feature.
// The clock is injected so this test states the behaviour without sleeping.
func TestAnEntryIsFetchedAgainOnceItsTTLHasPassed(t *testing.T) {
	hub := &stubHub{models: map[string]string{"meta-llama/Llama-3.1-8B-Instruct": llama8B}}
	srv := hub.start(t)
	clock := time.Unix(1_700_000_000, 0)
	c := &Client{
		HTTP:    srv.Client(),
		BaseURL: srv.URL,
		TTL:     time.Minute,
		Now:     func() time.Time { return clock },
	}
	ctx := context.Background()

	if _, err := c.Search(ctx, "llama", 10); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(59 * time.Second)
	if _, err := c.Search(ctx, "llama", 10); err != nil {
		t.Fatal(err)
	}
	if got := hub.requests(); got != 1 {
		t.Fatalf("%d requests inside the TTL, want 1", got)
	}

	clock = clock.Add(2 * time.Second)
	if _, err := c.Search(ctx, "llama", 10); err != nil {
		t.Fatal(err)
	}
	if got := hub.requests(); got != 2 {
		t.Fatalf("%d requests after the TTL, want 2", got)
	}
}

// The expensive miss is the one that never becomes a hit. A repository id typed
// into a search-as-you-type box is wrong on every keystroke but the last, so a
// 404 that is not cached is a 404 asked once per character.
func TestARepeatedTypoIsNotAskedTwice(t *testing.T) {
	hub := &stubHub{models: map[string]string{"meta-llama/Llama-3.1-8B-Instruct": llama8B}}
	c := newClient(t, hub, "")
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := c.Resolve(ctx, "meta-llama/Llama-3.1-8B-Instruc"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound every time", err)
		}
	}
	if got := hub.requests(); got != 1 {
		t.Fatalf("%d requests for the same missing model, want 1", got)
	}
}

// A response big enough to hurt is refused whole rather than truncated into a
// confusing parse error — and the message says which limit was hit.
func TestAnOversizeResponseIsRefusedRatherThanBuffered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"a/b","tags":["`))
		chunk := strings.Repeat("x", 64<<10)
		for written := 0; written < maxResponseBytes+(1<<20); written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return // the client hung up at the cap, which is the point
			}
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	_, err := c.Search(context.Background(), "anything", 10)
	if err == nil {
		t.Fatal("an oversize Hub response must not be buffered")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want it to name the cap rather than a JSON parse failure", err)
	}
}

// A search box types one query per keystroke, from every user, forever. The
// cache has to have a ceiling, and the oldest entry is the one to lose.
func TestTheCacheDoesNotGrowWithoutBound(t *testing.T) {
	hub := &stubHub{models: map[string]string{"meta-llama/Llama-3.1-8B-Instruct": llama8B}}
	c := newClient(t, hub, "")
	ctx := context.Background()

	for i := 0; i < maxCacheEntries+64; i++ {
		if _, err := c.Search(ctx, fmt.Sprintf("query-%d", i), 10); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	entries, queued := len(c.cache), len(c.order)
	c.mu.Unlock()
	if entries > maxCacheEntries || queued > maxCacheEntries {
		t.Fatalf("cache holds %d entries / %d queued keys, cap is %d", entries, queued, maxCacheEntries)
	}

	// The most recent query is still there: eviction takes the oldest, not
	// whatever the user just typed.
	before := hub.requests()
	if _, err := c.Search(ctx, fmt.Sprintf("query-%d", maxCacheEntries+63), 10); err != nil {
		t.Fatal(err)
	}
	if hub.requests() != before {
		t.Error("the newest entry was evicted — eviction must drop the oldest")
	}
}

// A caller that sorts or truncates the slice it was handed must not be editing
// the copy every later caller gets.
func TestACallerCannotCorruptTheCachedResult(t *testing.T) {
	hub := &stubHub{models: map[string]string{
		"meta-llama/Llama-3.1-8B-Instruct": llama8B,
		"TheBloke/Llama-2-7B-Chat-GGUF":    llama7BGGUF,
	}}
	c := newClient(t, hub, "")
	ctx := context.Background()

	first, err := c.Search(ctx, "llama", 10)
	if err != nil {
		t.Fatal(err)
	}
	first[0] = ModelCard{ID: "vandalised"}

	second, err := c.Search(ctx, "llama", 10)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].ID == "vandalised" {
		t.Fatal("mutating a returned card rewrote the cache")
	}
}

// A Hub that is unhappy has to produce an error an operator can act on, and the
// action differs depending on whether a token is configured at all.
func TestAnUpstreamFailureNamesTheFix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		token     string
		wantPhras string
	}{
		{"refused with no token", http.StatusForbidden, "", "set CP_HUGGING_FACE_TOKEN"},
		{"refused with a bad token", http.StatusForbidden, "hf_stale", "not valid"},
		{"rate limited", http.StatusTooManyRequests, "", "rate-limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: tc.token}
			_, err := c.Search(context.Background(), "llama", 10)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantPhras) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantPhras)
			}
		})
	}
}

// A transport failure is not cached: a network blip that lasted a second must
// not keep the picker broken for the whole TTL.
func TestATransportFailureIsNotCached(t *testing.T) {
	hub := &stubHub{models: map[string]string{"meta-llama/Llama-3.1-8B-Instruct": llama8B}}
	srv := hub.start(t)
	c := &Client{HTTP: srv.Client(), BaseURL: "http://127.0.0.1:1"}
	ctx := context.Background()

	if _, err := c.Search(ctx, "llama", 10); err == nil {
		t.Fatal("expected the unreachable base URL to fail")
	}
	c.BaseURL = srv.URL
	if _, err := c.Search(ctx, "llama", 10); err != nil {
		t.Fatalf("the retry after a transport failure must reach the Hub: %v", err)
	}
	if got := hub.requests(); got != 1 {
		t.Fatalf("%d requests reached the Hub, want 1 — the failure must not have been remembered as an answer", got)
	}
}

// The limit is ours to decide: nobody picks a model out of ten thousand rows,
// and buffering them would be entirely the control plane's problem.
func TestTheSearchLimitIsClampedToSomethingAPickerCanRender(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int
		want string
	}{
		{"unspecified", 0, "20"},
		{"negative", -5, "20"},
		{"reasonable", 7, "7"},
		{"absurd", 10000, "50"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.URL.Query().Get("limit")
				_, _ = fmt.Fprint(w, "[]")
			}))
			defer srv.Close()

			c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
			if _, err := c.Search(context.Background(), "llama", tc.in); err != nil {
				t.Fatal(err)
			}
			if seen != tc.want {
				t.Fatalf("limit sent = %q, want %q", seen, tc.want)
			}
		})
	}
}
