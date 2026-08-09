package hf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		"config": {
			"architectures": ["LlamaForCausalLM"],
			"model_type": "llama",
			"max_position_embeddings": 131072
		},
		"safetensors": {"parameters": {"BF16": 8030261248, "I64": 8192}, "total": 8030269440}
	}`

	// The catalogue's "fits anything" model — 2.9 GB against a 40 GiB card — and
	// the one a pinned 8192-token window killed at startup. Its context ceiling
	// is 2048, and vLLM raises rather than truncates when it is handed more.
	tinyLlama = `{
		"id": "TinyLlama/TinyLlama-1.1B-Chat-v1.0",
		"modelId": "TinyLlama/TinyLlama-1.1B-Chat-v1.0",
		"likes": 1347,
		"downloads": 512338,
		"gated": false,
		"pipeline_tag": "text-generation",
		"library_name": "transformers",
		"tags": ["transformers","safetensors","llama","text-generation","conversational"],
		"config": {
			"architectures": ["LlamaForCausalLM"],
			"model_type": "llama",
			"max_position_embeddings": 2048
		},
		"safetensors": {"parameters": {"BF16": 1100048384}, "total": 1100048384}
	}`

	// A mixture of experts, where the repository NAME and the weights disagree:
	// "8x7B" reads as 56e9 parameters, and the checkpoint holds 46.7e9 because
	// the experts share everything that is not an expert.
	mixtral8x7B = `{
		"id": "mistralai/Mixtral-8x7B-Instruct-v0.1",
		"modelId": "mistralai/Mixtral-8x7B-Instruct-v0.1",
		"likes": 4300,
		"downloads": 421760,
		"gated": "manual",
		"pipeline_tag": "text-generation",
		"library_name": "transformers",
		"tags": ["transformers","safetensors","mixtral","text-generation","conversational"],
		"config": {
			"architectures": ["MixtralForCausalLM"],
			"model_type": "mixtral",
			"max_position_embeddings": 32768
		},
		"safetensors": {"parameters": {"BF16": 46702792704}, "total": 46702792704}
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

	// The model the unfiltered picker offered and no runtime here can load. It
	// out-downloads almost everything on the Hub, it sizes cleanly at ~3.9 GB,
	// and it fits every card the fit check knows about — which is why it reached
	// a deploy and only failed once vLLM tried to load it.
	whisperLargeV3 = `{
		"id": "openai/whisper-large-v3",
		"modelId": "openai/whisper-large-v3",
		"likes": 4000,
		"downloads": 9000000,
		"gated": false,
		"pipeline_tag": "automatic-speech-recognition",
		"library_name": "transformers",
		"tags": ["transformers","safetensors","whisper","automatic-speech-recognition"],
		"safetensors": {"parameters": {"F32": 1543304960}, "total": 1543304960}
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

	mu      sync.Mutex
	calls   int
	auths   []string
	queries []url.Values
}

func (h *stubHub) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		h.mu.Lock()
		h.calls++
		h.auths = append(h.auths, auth)
		h.queries = append(h.queries, r.URL.Query())
		h.mu.Unlock()

		if h.wantToken != "" && auth != "Bearer "+h.wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		// The repository's real config.json, served off the git tree exactly as
		// the Hub serves it — NOT out of the model record, because the Hub's
		// record does not carry it. The fixtures keep their "config" block and
		// this hands it back from the path the client now has to ask on, so a
		// client that went on reading the record would find nothing, which is
		// precisely what happened in production.
		if repo, ok := strings.CutSuffix(r.URL.Path, "/resolve/main/config.json"); ok {
			id := strings.TrimPrefix(repo, "/")
			if h.gated[id] && auth == "" {
				// A gated repository's config is gated too: this is why a
				// tokenless control plane can never learn Llama's ceiling.
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			config, found := configBlockOf(h.models[id])
			if !found {
				// A GGUF-only repository publishes no config.json at all.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprint(w, config)
			return
		}

		rest := strings.TrimPrefix(r.URL.Path, "/api/models")

		if rest == "" || rest == "/" {
			query := strings.ToLower(r.URL.Query().Get("search"))
			// The Hub applies pipeline_tag server-side, and so does this: a stub
			// that ignored it could not tell a filter that is sent from one that
			// is merely written down.
			task := r.URL.Query().Get("pipeline_tag")
			var matched []string
			for id, raw := range h.models {
				if query != "" && !strings.Contains(strings.ToLower(id), query) {
					continue
				}
				if task != "" && taskOf(raw) != task {
					continue
				}
				matched = append(matched, raw)
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

func (h *stubHub) sentQueries() []url.Values {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]url.Values(nil), h.queries...)
}

// taskOf reads a fixture's pipeline_tag the way the Hub reads a repository's.
func taskOf(raw string) string {
	var m apiModel
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	return m.PipelineTag
}

// configBlockOf lifts a fixture's "config" object back out so the stub can
// serve it as the repository's config.json. The fixtures state it once and both
// endpoints are fed from that one statement, so a test cannot accidentally
// describe a Hub whose two answers disagree.
func configBlockOf(raw string) (string, bool) {
	var record struct {
		Config json.RawMessage `json:"config"`
	}
	if raw == "" || json.Unmarshal([]byte(raw), &record) != nil || len(record.Config) == 0 {
		return "", false
	}
	return string(record.Config), true
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
	if safetensors.VRAMText != "~21.4 GB" {
		t.Errorf("vramText = %q, want ~21.4 GB", safetensors.VRAMText)
	}
	if safetensors.PipelineTag != "text-generation" {
		t.Errorf("pipelineTag = %q — the wizard cannot refuse what it was not told", safetensors.PipelineTag)
	}

	// The GGUF repository is sized off its name for want of a safetensors index,
	// and it carries the format that gets it refused at the step. It does NOT
	// carry a second engine: ollama could not resolve "owner/name" and served
	// nothing, so no repository derives it any more.
	gguf := byID["TheBloke/Llama-2-7B-Chat-GGUF"]
	if gguf.Engine != "vllm" {
		t.Errorf("engine = %q, want vllm — a derived ollama endpoint pulled nothing and 404'd", gguf.Engine)
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

// The model's own context ceiling has to travel on the card, because the
// runtime's --max-model-len is measured against it: vLLM's
// _get_and_verify_max_len RAISES when the window it is given is longer than the
// model's, it does not truncate. A card that did not carry the ceiling left
// every endpoint pinned to SizedContextTokens, and TinyLlama — 2048 tokens, the
// smallest model in the catalogue and a green tick against every card in the
// product — exited at startup on a host billed at GPU rates.
//
// It is read from the repository's config.json and NOT from the model record,
// which carries a field called "config" that does not contain it. This test
// goes through Resolve because Resolve is what the store sizes from; the
// companion below pins the deliberate absence on the search path.
func TestAModelCarriesTheContextCeilingItsOwnConfigDeclares(t *testing.T) {
	for _, tc := range []struct {
		name        string
		id          string
		raw         string
		wantCeiling int
		wantServed  int
	}{
		{"a long-context model is clamped to the window the sizing paid for",
			"meta-llama/Llama-3.1-8B-Instruct", llama8B, 131072, SizedContextTokens},
		{"a short-context model is served at its own maximum",
			"TinyLlama/TinyLlama-1.1B-Chat-v1.0", tinyLlama, 2048, 2048},
		// No config.json to read. Unknown falls back to the window the VRAM
		// estimate budgeted, so the flag and the arithmetic still agree.
		{"a repository that publishes no config is served the window that was estimated",
			"TheBloke/Llama-2-7B-Chat-GGUF", llama7BGGUF, 0, SizedContextTokens},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := &stubHub{models: map[string]string{tc.id: tc.raw}}
			card, err := newClient(t, hub, "").Resolve(context.Background(), tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if card.MaxPositionEmbeddings != tc.wantCeiling {
				t.Fatalf("maxPositionEmbeddings = %d, want %d — the store cannot pin a window it was not told about",
					card.MaxPositionEmbeddings, tc.wantCeiling)
			}
			if got := ServedContextTokens(card.MaxPositionEmbeddings); got != tc.wantServed {
				t.Errorf("served context = %d, want %d", got, tc.wantServed)
			}
		})
	}
}

// Search does NOT pay for the ceiling, and that is a budget decision worth
// pinning: it costs one request per repository, and a twenty-row
// search-as-you-type box would turn every keystroke into twenty-one calls to a
// third party. The list needs a size and a name; only the model actually being
// deployed needs a window, and that one goes through Resolve.
func TestSearchDoesNotSpendARequestPerRowOnTheContextCeiling(t *testing.T) {
	hub := &stubHub{models: map[string]string{
		"meta-llama/Llama-3.1-8B-Instruct":   llama8B,
		"TinyLlama/TinyLlama-1.1B-Chat-v1.0": tinyLlama,
	}}
	cards, err := newClient(t, hub, "").Search(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("got %d cards, want 2", len(cards))
	}
	for _, card := range cards {
		if card.MaxPositionEmbeddings != 0 {
			t.Errorf("%s carried a ceiling of %d from a search row; the record cannot answer that and a search must not fan out to fetch it",
				card.ID, card.MaxPositionEmbeddings)
		}
	}
	if hub.requests() != 1 {
		t.Errorf("%d requests reached the Hub for one search, want 1", hub.requests())
	}
}

// A gated repository's config.json is gated too, so a control plane with no
// token cannot learn Llama's ceiling — and must not be stopped by that. The
// unknown falls back to the estimated window, which is the whole reason zero is
// no longer an instruction to render nothing: gated repositories are the long
// context ones, and leaving them unpinned is the 131072-token crash.
func TestAGatedModelWithoutATokenStillGetsTheWindowTheEstimateBudgeted(t *testing.T) {
	hub := &stubHub{
		models: map[string]string{"meta-llama/Llama-3.1-8B-Instruct": llama8B},
		gated:  map[string]bool{"meta-llama/Llama-3.1-8B-Instruct": true},
	}
	card, err := newClient(t, hub, "").Resolve(context.Background(), "meta-llama/Llama-3.1-8B-Instruct")
	if err != nil {
		t.Fatal(err)
	}
	if card.MaxPositionEmbeddings != 0 {
		t.Fatalf("maxPositionEmbeddings = %d, want 0 — a 401 carries no config", card.MaxPositionEmbeddings)
	}
	if got := ServedContextTokens(card.MaxPositionEmbeddings); got != SizedContextTokens {
		t.Errorf("served context = %d, want %d — an unpinned gated model is the crash this exists to prevent",
			got, SizedContextTokens)
	}
}

// Where the ceiling is written is the architecture's business, not the Hub's:
// a decoder-only repository states it at the top of its config and a multimodal
// one states it inside the text half's. Both are read, and anything that is not
// a context length is UNKNOWN rather than a number this package made up.
func TestTheContextCeilingIsFoundWhereverTheArchitecturePutsIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   int
	}{
		{"a decoder-only model states it at the top level", `{"model_type":"llama","max_position_embeddings":2048}`, 2048},
		{"a multimodal model states it in the text half's own config",
			`{"model_type":"mllama","text_config":{"max_position_embeddings":131072},"vision_config":{"image_size":560}}`, 131072},
		{"the top level wins over a nested block",
			`{"max_position_embeddings":8192,"text_config":{"max_position_embeddings":131072}}`, 8192},
		// Cautious AND deterministic: the shorter window only serves fewer
		// tokens, where the longer one is a runtime that will not start.
		{"the smallest nested block wins, and wins every time",
			`{"text_config":{"max_position_embeddings":32768},"decoder":{"max_position_embeddings":4096}}`, 4096},
		{"a config with no ceiling in it", `{"architectures":["LlamaForCausalLM"],"model_type":"llama"}`, 0},
		{"no config block at all", ``, 0},
		{"a null config block", `null`, 0},
		{"a config that is not an object", `"llama"`, 0},
		{"a ceiling that is not a number", `{"max_position_embeddings":"many"}`, 0},
		{"a ceiling of zero is not a window", `{"max_position_embeddings":0}`, 0},
		{"a ceiling too large to be one", `{"max_position_embeddings":1e18}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Read repeatedly, because Go randomises map iteration: an
			// implementation that took whichever nested block it happened to see
			// first would pass a single read about half the time, and size one
			// repository two ways on two identical requests.
			for i := 0; i < 32; i++ {
				if got := configContextCeiling(json.RawMessage(tc.config)); got != tc.want {
					t.Fatalf("configContextCeiling(%s) = %d, want %d", tc.config, got, tc.want)
				}
			}
		})
	}
}

// The Hub ranks /api/models by downloads across every task, so an unfiltered
// picker offers embedding, ASR and diffusion repositories — models it can size,
// fit and deploy, and no configured runtime can load. The filter has to be part
// of the QUERY rather than a pass over the results: the Hub decides which 20
// rows come back, and dropping half of them here would leave the operator
// scrolling for a model they are allowed to have.
func TestSearchAsksTheHubOnlyForModelsARuntimeCanServe(t *testing.T) {
	hub := &stubHub{models: map[string]string{
		"meta-llama/Llama-3.1-8B-Instruct": llama8B,
		"openai/whisper-large-v3":          whisperLargeV3,
	}}
	c := newClient(t, hub, "")

	cards, err := c.Search(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := hub.sentQueries()[0].Get("pipeline_tag"); got != "text-generation" {
		t.Fatalf("pipeline_tag sent = %q, want text-generation — the Hub, not this process, decides which rows come back", got)
	}
	if len(cards) != 1 || cards[0].ID != "meta-llama/Llama-3.1-8B-Instruct" {
		t.Fatalf("cards = %+v, want the text-generation model alone", cards)
	}

	// And searching for the wrong kind of model by name finds nothing, rather
	// than finding it and failing an hour later inside a GPU container.
	asr, err := c.Search(context.Background(), "whisper", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(asr) != 0 {
		t.Fatalf("searching for whisper returned %+v — the picker exists to remove this deploy", asr)
	}
}

// Resolve is deliberately NOT filtered. An operator who types a repository id
// has decided already, and a lookup that refuses to LOOK cannot explain what is
// wrong with the answer. The task rides back on the card so the wizard can
// refuse the pick at the step it was made, with the model still on screen.
func TestResolveAnswersForAnyTaskAndSaysWhichOneItIs(t *testing.T) {
	hub := &stubHub{models: map[string]string{
		"meta-llama/Llama-3.1-8B-Instruct": llama8B,
		"openai/whisper-large-v3":          whisperLargeV3,
	}}
	c := newClient(t, hub, "")
	ctx := context.Background()

	asr, err := c.Resolve(ctx, "openai/whisper-large-v3")
	if err != nil {
		t.Fatalf("an id typed by hand must still resolve: %v", err)
	}
	if asr.PipelineTag != "automatic-speech-recognition" {
		t.Errorf("pipelineTag = %q, want the task the repository declares", asr.PipelineTag)
	}
	for _, q := range hub.sentQueries() {
		if q.Get("pipeline_tag") != "" {
			t.Errorf("resolve sent pipeline_tag=%q — a named repository is not filtered", q.Get("pipeline_tag"))
		}
	}

	text, err := c.Resolve(ctx, "meta-llama/Llama-3.1-8B-Instruct")
	if err != nil {
		t.Fatal(err)
	}
	if text.PipelineTag != "text-generation" {
		t.Errorf("pipelineTag = %q, want text-generation on the resolve path too", text.PipelineTag)
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
// they have to accept. So the card survives the 401 carrying what the id alone
// establishes — which repository it is, that it is gated, and which runtime
// would serve it — and no size, because the id does not establish one.
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
	// Deployable, but not SIZED: see the Mixtral test below for the repository
	// whose name would have refused a machine it fits on.
	if card.Engine != "vllm" {
		t.Errorf("engine = %q, want vllm — a gated model is still runnable once the licence is accepted", card.Engine)
	}
	if card.ParametersKnown || card.VRAMText != "" {
		t.Errorf("card = %+v, want no size at all until a token produces one", card)
	}
	// The one field a 401 cannot produce. Empty is UNKNOWN, and the wizard must
	// read it that way: refusing on an absent task would refuse every gated
	// model on a control plane with no Hub token — which is precisely the
	// control plane whose operator can do least about it.
	if card.PipelineTag != "" {
		t.Errorf("pipelineTag = %q, want empty — a 401 carries no metadata to fill it with", card.PipelineTag)
	}

	// With a token the same repository resolves fully, and the exact safetensors
	// count is where the size comes from. The token is the fix the product names
	// for a gated model, and it is the fix for the missing size too.
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

// A repository name is not a parameter count, and for a mixture of experts it
// is a wrong one: "Mixtral-8x7B" multiplies out to 56e9 parameters and ~150 GB,
// while the checkpoint holds 46.7e9 and needs ~125 GB. On a 141 GB H200 the
// gap is the whole decision — the name-sized card REFUSED, by 25 GB, a model
// that fits. store.checkModelFits states the rule that breaks: the fit check is
// only allowed to be wrong in the permissive direction. So a gated repository
// nothing could read is an unsized card, and an unsized card gates nothing.
func TestAGatedMixtureOfExpertsIsNotSizedFromANameThatOverstatesIt(t *testing.T) {
	const id = "mistralai/Mixtral-8x7B-Instruct-v0.1"
	hub := &stubHub{
		models: map[string]string{id: mixtral8x7B},
		gated:  map[string]bool{id: true},
	}
	c := newClient(t, hub, "")

	card, err := c.Resolve(context.Background(), id)
	if err != nil {
		t.Fatalf("a gated model must not fail to resolve: %v", err)
	}
	if !card.Gated {
		t.Error("gated = false — the user is never told why the weights will not download")
	}
	if card.ParametersKnown || card.Parameters != 0 {
		t.Fatalf("parameters = %d (known %v), want an unsized card — the name says 56e9 and the weights say 46.7e9",
			card.Parameters, card.ParametersKnown)
	}
	if card.VRAMBytesRequired != 0 || card.VRAMText != "" {
		t.Errorf("vram = %d/%q, want nothing — zero is what switches every fit check off",
			card.VRAMBytesRequired, card.VRAMText)
	}
	if card.SizingBasis != "unknown" {
		t.Errorf("sizingBasis = %q, want unknown — nothing sized this card", card.SizingBasis)
	}
	// Still a card the wizard can render and the operator can deploy: refusing to
	// GUESS a size is not refusing the model.
	if card.Engine != "vllm" || card.Name != "Mixtral 8x7B Instruct v0.1" {
		t.Errorf("card = %+v, want a runnable, named card", card)
	}

	// And the count it declined to use is not merely unused, it is wrong by 25 GB
	// in the refusing direction, which is the direction this check may not be
	// wrong in.
	hub2 := &stubHub{models: map[string]string{id: mixtral8x7B}, wantToken: "hf_secret"}
	c2 := newClient(t, hub2, "hf_secret")
	full, err := c2.Resolve(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if full.SizingBasis != "safetensors" || full.Parameters != 46_702_792_704 {
		t.Fatalf("sizing = %s/%d, want safetensors/46.7e9 once a token can read the index", full.SizingBasis, full.Parameters)
	}
	if full.VRAMText != "~125 GB" {
		t.Errorf("vramText = %q, want ~125 GB — the figure that fits a 141 GB H200", full.VRAMText)
	}
	named, ok := ParseParameterCount(id)
	if !ok || named <= full.Parameters {
		t.Fatalf("the name reads %d parameters against the index's %d — this test is asserting a gap that has closed",
			named, full.Parameters)
	}
	if fromName := FormatVRAM(RequiredVRAMBytes(named, full.BytesPerParam)); fromName != "~150 GB" {
		t.Errorf("the name would have demanded %q; the recorded defect is ~150 GB against a 141 GB card", fromName)
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
	// Three: one search, and one resolve that costs a model record plus the
	// repository's config.json. Both of the resolve's requests are behind the
	// same cache entry, which is the point — the ceiling lookup must not turn a
	// cached model into an uncached one.
	if got := hub.requests(); got != 3 {
		t.Fatalf("%d requests reached the Hub, want 3 (one search; one resolve costing a record and a config)", got)
	}

	// A different limit is a different answer and must not be served from the
	// first one's entry.
	if _, err := c.Search(ctx, "llama", 5); err != nil {
		t.Fatal(err)
	}
	if got := hub.requests(); got != 4 {
		t.Fatalf("%d requests, want 4 — a different limit is a different query", got)
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
