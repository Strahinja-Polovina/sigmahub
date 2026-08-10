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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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

// TextGenerationTask is the Hub's name for the task the picker offers, and the
// one the configured runtime is known to serve. It is both what Search asks the
// Hub for and what a ModelCard's PipelineTag carries back, so the query and the
// wizard's refusal compare one string rather than two spellings of one idea.
const TextGenerationTask = "text-generation"

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
	ID        string `json:"id"`
	Name      string `json:"name"`
	Gated     bool   `json:"gated"`
	Downloads int    `json:"downloads"`
	Likes     int    `json:"likes"`
	// PipelineTag is the Hub's task for the repository, and it travels on every
	// card so the wizard can refuse a model no runtime can serve AT the model
	// step, with the model still on screen. Search only ever returns
	// TextGenerationTask (it asks the Hub for that task); Resolve returns
	// whatever the repository declares, because an id typed by hand is not
	// filtered and the refusal has to be able to name what was picked.
	//
	// Empty means the HUB did not say — a gated repository read without a token
	// has no metadata at all (see gatedCard). Empty is UNKNOWN and must never be
	// read as "not a text model": refusing on it would refuse every gated model
	// on a control plane with no Hub token, which is the one case where the
	// operator can do least about it.
	PipelineTag string `json:"pipelineTag"`
	Library     string `json:"library"`
	// License is the Hub's licence identifier for the repository ("llama3.1",
	// "apache-2.0", "gemma"…) and URL is its model-card page.
	//
	// Both are on the card because of what a deploy actually does (SIGMA-302):
	// the weights land on the CUSTOMER's own machine, under terms the customer
	// was never shown. The Llama Community Licence is the clearest case — an
	// acceptable-use policy, an attribution requirement and a 700M-MAU clause,
	// all of which bind the party running the model — and the product used to
	// pull those weights without naming the licence anywhere in the wizard or on
	// the resource afterwards.
	//
	// Empty License means the Hub did not say, which is the ordinary answer for a
	// gated repository read without a token (see gatedCard): unknown, never
	// "unlicensed". URL is derived from the id and is therefore ALWAYS present —
	// for exactly the gated case where nothing else is, it is the only route to
	// the page the terms are stated and accepted on.
	License string `json:"license"`
	URL     string `json:"url"`
	// Engine is the runtime the control plane would render for this model. The
	// wizard used to ask; the metadata already knows (see EngineForModel).
	Engine string `json:"engine"`
	// MaxPositionEmbeddings is the model's OWN context ceiling, as the Hub's
	// copy of its config.json states it. 0 means the Hub did not say, and that
	// is a common answer rather than an error case — a gated repository read
	// without a token carries no config at all.
	//
	// It rides on the card because the context window a runtime is started at is
	// not ours alone to pick: vLLM REFUSES to start when --max-model-len is
	// longer than this number, so the window is the smaller of what the sizing
	// paid for and what the model has. ServedContextTokens is that decision, and
	// a 0 here makes a 0 there — which is an instruction to render no flag at
	// all, not a length. Read its comment before rendering anything from this.
	MaxPositionEmbeddings int `json:"maxPositionEmbeddings"`
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
	// SizingBasis names where the parameter count came from — "safetensors",
	// "name" or "unknown" — and it is DIAGNOSTIC only. Nothing reads it to
	// decide anything, and that is the design: a name-derived count is used or
	// it is not, and a third confidence tier ("probably fits") is a decision
	// nobody asked for and nobody could act on. It exists so a support question
	// about a surprising number can be answered without re-running the sizer.
	SizingBasis string `json:"sizingBasis"`
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
	// The task filter is the picker's own purpose applied to its own list. The
	// Hub ranks /api/models by downloads across EVERY task, so unfiltered the
	// empty query is BERT, embedding, ASR and diffusion repositories, and typing
	// "whisper" returns whisper: openai/whisper-large-v3 sizes cleanly at ~3.9
	// GB, fits every card the fit check knows about, and crash-loops vLLM. That
	// is the late failure this whole feature exists to delete, offered with a
	// green checkmark in front of it.
	//
	// It costs a handful of repositories vLLM can in fact serve —
	// text2text-generation and image-text-to-text are separate tasks on the Hub
	// — and that trade is deliberate, because Resolve is NOT filtered: an
	// operator who names a repository by hand has already decided, and the id
	// still resolves (see Resolve).
	q.Set("pipeline_tag", TextGenerationTask)

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
		// The context ceiling costs a second request, and it is taken HERE and
		// not in Search on purpose: it is needed once, for the model actually
		// being deployed, and a twenty-row search would otherwise fan out into
		// twenty-one requests per keystroke. The card that reaches the store —
		// and so the rendered --max-model-len — comes through this path.
		card.MaxPositionEmbeddings = c.contextCeiling(ctx, id, segments)
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

// contextCeiling reads the model's own context ceiling from its config.json.
//
// Every failure returns 0 — "the Hub did not tell us" — and 0 is not a short
// window: ServedContextTokens turns it into the context the estimate was
// actually paid for. That is why this cannot fail the Resolve it is called
// from. A GGUF-only repository publishes no config.json at all (404), a gated
// one answers 401 to a control plane with no token, and neither is a reason to
// refuse a model the rest of the record described perfectly well.
func (c *Client) contextCeiling(ctx context.Context, id string, segments []string) int {
	body, status, err := c.do(ctx, "/"+strings.Join(segments, "/")+configJSONPath, "the model config for "+id)
	if err != nil || status != http.StatusOK || len(body) > maxConfigBytes {
		return 0
	}
	return configContextCeiling(body)
}

// gatedCard is everything that can be honestly said about a repository the Hub
// will not describe to us: it exists, it is gated, and the format spelled out in
// its id decides which runtime would serve it. It is NOT sized.
//
// It used to be, from the name, and the name lies about mixture-of-experts
// repositories: "mistralai/Mixtral-8x7B-Instruct-v0.1" multiplies out to 56e9
// parameters and ~150 GB, where the real checkpoint is 46.7e9 and ~125 GB. On a
// 141 GB H200 that gated card REFUSED a model that fits, by 25 GB — and
// store.checkModelFits states the rule it broke: the fit check "is only allowed
// to be wrong in the permissive direction". An unsized gated model is a KNOWN
// unknown, and refusing on a guess is the one direction this code may not be
// wrong in, so ParametersKnown stays false, VRAMBytesRequired stays 0, and every
// consumer of the card fails open exactly as it does for a model nobody can
// size.
//
// The name path is not gone; it still sizes repositories the Hub DID describe
// but published no readable index for (see applySizing). What it no longer does
// is stand in for metadata that a token would have produced — the fix for a
// gated model is a token, and the product says so.
//
// PipelineTag is empty here and cannot be otherwise: a 401 carries no metadata.
// Empty is unknown, and unknown is not a refusal.
func gatedCard(id string) ModelCard {
	// URL, and only URL, survives a 401: it is built from the id rather than read
	// from a body there isn't one of. That matters more here than anywhere else
	// (SIGMA-302) — a gated repository is precisely the one whose terms someone
	// has to go and accept, and this is the link to the page that states them.
	card := ModelCard{ID: id, Name: displayName(id), Gated: true, URL: modelCardURL(id)}
	// A 401 carries no tags and no dtype either, so the format is whatever the
	// id itself spells out — "…-AWQ" is still AWQ when the Hub will not say so.
	applyRuntime(&card, nil, "")
	card.SizingBasis = "unknown"
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
	// CardData is the repository's README front-matter as the Hub parsed it. Its
	// `license` key is the authoritative statement, and it arrives as a STRING
	// or as an ARRAY of strings (a repository may be dual-licensed), which is
	// why it is raw here — decoding into either shape errors on the other, and
	// the mistake would silently drop the licence on whichever kind of repo it
	// was not written for. Search rows carry no cardData at all; they carry the
	// same fact as a "license:<id>" tag, which licenseOf falls back to.
	CardData struct {
		License json.RawMessage `json:"license"`
	} `json:"cardData"`
	Safetensors struct {
		// Parameters is element counts per dtype, e.g. {"BF16": 8030261248}.
		Parameters map[string]uint64 `json:"parameters"`
		Total      uint64            `json:"total"`
	} `json:"safetensors"`
}

// configJSONPath is where a repository's ACTUAL config.json lives — the same
// file transformers and vLLM read, served straight out of the git repo.
//
// It has to be fetched separately, and that is the correction to a wrong
// assumption that cost a release. `/api/models/{id}` carries a field called
// `config`, which reads like the answer and is not: the Hub returns a curated
// three-key subset — architectures, model_type, tokenizer_config — and
// max_position_embeddings is not among them, on either the search rows or the
// single-model record. Reading the ceiling from there returned 0 for EVERY
// model, which made ServedContextTokens' unknown branch the only branch and
// restored the exact 131072-token crash --max-model-len exists to prevent, with
// the fit check certifying it green. The stub in this package's own tests said
// otherwise, because the stub was written from the same assumption as the code.
//
// Pinned to `main` rather than the record's sha: the sha names a commit whose
// config a model card may not have been built from, and a context ceiling that
// changes between revisions of the same repository is not a thing that happens.
const configJSONPath = "/resolve/main/config.json"

// maxConfigBytes caps the config.json read. A model config is a few kilobytes;
// anything approaching this is not one, and an unbounded read of a third
// party's file is how a control plane dies of someone else's mistake.
const maxConfigBytes = 256 << 10

// contextCeilingKeys are the config.json fields an architecture may declare its
// context ceiling in, in the order vLLM's own _get_and_verify_max_len consults
// them. There is more than one because the field is the ARCHITECTURE's, not a
// standard: GPT-2 and its descendants say n_positions, the ChatGLM family says
// seq_length, and reading only the transformers spelling means silently
// treating those as unknown.
var contextCeilingKeys = []string{
	"max_position_embeddings",
	"n_positions",
	"max_seq_len",
	"seq_length",
	"model_max_length",
	"n_ctx",
}

// configContextCeiling reads a model's own context ceiling out of the Hub's
// config block. 0 means the Hub did not say — an ordinary answer, not a
// failure — and it must never be read as a short window: see
// ServedContextTokens for what a caller does with it.
//
// Two nestings are searched. The top level is where a plain decoder-only
// repository states it; the nested pass is for the multimodal repositories that
// state it inside the text half's own config ("text_config" on the Llama and
// Qwen vision families, other spellings elsewhere), and it matches on the KEY so
// that a list of architecture names is not something this file has to keep
// current.
//
// A top-level value wins outright, and when only nested blocks answer the
// SMALLEST does. That tie-break has to be deterministic — Go's map iteration is
// not, and a card whose context differed between two identical requests would be
// unexplainable — and small is the safe direction, because a window shorter than
// the model allows serves fewer tokens while one longer than it allows is a
// runtime that refuses to start at all.
func configContextCeiling(raw json.RawMessage) int {
	var config map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &config) != nil {
		return 0
	}
	if n, ok := firstContextLength(config); ok {
		return n
	}
	var best int
	for _, value := range config {
		var nested map[string]json.RawMessage
		if json.Unmarshal(value, &nested) != nil {
			// Not an object: architectures, model_type and the rest of the file.
			continue
		}
		if n, ok := firstContextLength(nested); ok && (best == 0 || n < best) {
			best = n
		}
	}
	return best
}

// firstContextLength returns the ceiling from the earliest key in
// contextCeilingKeys this block answers. Order is the tie-break, not size,
// because these are not competing estimates of one quantity — they are
// different architectures' names for it, and a repository that carries two
// carries the transformers spelling as the real one.
func firstContextLength(block map[string]json.RawMessage) (int, bool) {
	for _, key := range contextCeilingKeys {
		if n, ok := contextLength(block[key]); ok {
			return n, true
		}
	}
	return 0, false
}

// contextLength decodes one config value as a token count.
//
// Anything that is not a positive number this platform can hold is reported
// ABSENT rather than clamped into range: it is a field we misread, and the
// honest answer for something we cannot read is "unknown", which downstream
// leaves the runtime on its own ceiling instead of on ours.
func contextLength(raw json.RawMessage) (int, bool) {
	// float64, not int, because these arrive as JSON numbers and a config that
	// writes 2048 as "2048.0" is not wrong about its context window.
	var n float64
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	if n < 1 || n > math.MaxInt32 {
		return 0, false
	}
	return int(n), true
}

func cardFromAPI(m apiModel) (ModelCard, bool) {
	id := m.ID
	if id == "" {
		id = m.ModelID
	}
	if id == "" {
		return ModelCard{}, false
	}
	// MaxPositionEmbeddings is deliberately absent here: nothing in the model
	// record answers it (see configJSONPath), so it is filled by Resolve, which
	// is the one path that spends a second request on the file that does.
	card := ModelCard{
		ID:          id,
		Name:        displayName(id),
		Gated:       gatedFlag(m.Gated),
		Downloads:   m.Downloads,
		Likes:       m.Likes,
		PipelineTag: m.PipelineTag,
		Library:     m.LibraryName,
		License:     licenseOf(m),
		URL:         modelCardURL(id),
	}
	applySizing(&card, m.Tags, dominantDType(m.Safetensors.Parameters), m.Safetensors.Total)
	return card, true
}

// modelCardURL is the repository's page on huggingface.co — the canonical place
// its licence is stated and, for a gated repo, accepted. Derived from the id
// because the Hub's records carry no such field and because it must be present
// even when the record could not be read at all.
func modelCardURL(id string) string {
	if id == "" {
		return ""
	}
	return "https://huggingface.co/" + id
}

// licenseOf reads the repository's licence identifier from whichever of the
// Hub's two statements of it this record carries.
//
// cardData.license is the README front-matter and wins: it is the repository's
// own declaration, and it is what the single-model record returns. Search rows
// carry no cardData, so they are read from the "license:<id>" tag the Hub adds
// to the tag list from that same front matter — the two agree by construction.
//
// A dual-licensed repository states an ARRAY, and the first entry is taken:
// this string is shown to a person next to a link to the full text, not parsed
// by anything, and "apache-2.0" with a link beats an empty field.
func licenseOf(m apiModel) string {
	raw := bytes.TrimSpace(m.CardData.License)
	if len(raw) > 0 {
		var one string
		if json.Unmarshal(raw, &one) == nil && one != "" {
			return one
		}
		var many []string
		if json.Unmarshal(raw, &many) == nil {
			for _, s := range many {
				if s != "" {
					return s
				}
			}
		}
	}
	for _, tag := range m.Tags {
		if rest, ok := strings.CutPrefix(tag, "license:"); ok && rest != "" {
			return rest
		}
	}
	return ""
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
