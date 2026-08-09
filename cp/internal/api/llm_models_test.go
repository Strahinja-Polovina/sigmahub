package api

// The HTTP boundary of the Hugging Face model picker (SIGMA-213/214).
//
// These are handler tests, so what they pin is the WIRING and the failure
// vocabulary — the parts that have no test anywhere else and that every one of
// them would leave green if deleted:
//
//   - a control plane with no catalog answers a WORKING picker, not a 503. That
//     is the single most important assertion in this file: a 503 is a wizard
//     step nobody can finish, and it is what a "not configured" reflex would
//     have produced by analogy with every other optional dependency here;
//   - huggingface.co being unreachable is 502 and not 500, because 500 sends an
//     operator to our logs to hunt a bug that is in their egress rules;
//   - the caller's `limit` is clamped rather than rejected, and `models` is
//     always an array, because the dashboard maps over it.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/hf"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// llama8B is a card as cp/internal/hf renders one: an exactly-sized model whose
// VRAM figure the wizard shows and the store's create-time check re-uses.
var llama8B = hf.ModelCard{
	ID: "meta-llama/Llama-3.1-8B-Instruct", Name: "Llama 3.1 8B Instruct",
	Gated: true, Downloads: 1234567, Likes: 4321,
	PipelineTag: "text-generation", Library: "transformers", Engine: "vllm",
	Parameters: 8030261248, ParametersKnown: true,
	Quantization: "none", BytesPerParam: 2,
	VRAMBytesRequired: 21281019494, VRAMText: "~21 GB", SizingBasis: "safetensors",
}

// fakeCatalog implements ModelCatalog. It records the arguments because the
// clamping and trimming this handler does are invisible from the response.
type fakeCatalog struct {
	cards      []hf.ModelCard
	searchErr  error
	resolveErr error
	token      bool

	queries []string
	limits  []int
	ids     []string
}

func (f *fakeCatalog) Search(_ context.Context, query string, limit int) ([]hf.ModelCard, error) {
	f.queries = append(f.queries, query)
	f.limits = append(f.limits, limit)
	return f.cards, f.searchErr
}

func (f *fakeCatalog) Resolve(_ context.Context, repoID string) (hf.ModelCard, error) {
	f.ids = append(f.ids, repoID)
	if f.resolveErr != nil {
		return hf.ModelCard{}, f.resolveErr
	}
	if len(f.cards) == 0 {
		return hf.ModelCard{}, hf.ErrNotFound
	}
	return f.cards[0], nil
}

func (f *fakeCatalog) TokenConfigured() bool { return f.token }

// newModelServer builds the API with a caller-supplied catalog; a nil one is the
// unconfigured control plane, which is a state under test rather than a gap.
func newModelServer(t *testing.T, cat ModelCatalog) *Server {
	t.Helper()
	return New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken: testServiceToken,
		Models:          cat,
	})
}

// getAsDev issues the member-level read the dashboard makes.
func getAsDev(s *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestModelSearchAnswersCardsAndWhetherATokenIsConfigured(t *testing.T) {
	cat := &fakeCatalog{cards: []hf.ModelCard{llama8B}, token: true}
	rec := getAsDev(newModelServer(t, cat), "/v1/orgs/org_1/llm/models?q=llama+3.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("search → %d, want 200; body %s", rec.Code, rec.Body)
	}
	var out struct {
		Models          []hf.ModelCard `json:"models"`
		TokenConfigured bool           `json:"tokenConfigured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0].ID != llama8B.ID {
		t.Fatalf("models = %+v, want the catalog's card passed through", out.Models)
	}
	// The sizing numbers are the whole reason the CP does this lookup instead of
	// the browser: the wizard renders vramText and compares vramBytesRequired,
	// and a handler that dropped either would leave the picker with no verdict.
	if out.Models[0].VRAMBytesRequired != llama8B.VRAMBytesRequired || out.Models[0].VRAMText != "~21 GB" {
		t.Fatalf("card lost its sizing: %+v", out.Models[0])
	}
	// Whether a token is configured changes what the absence of a model MEANS —
	// without it, a gated repo the operator can see on huggingface.co simply is
	// not here, and the picker has to say so rather than imply it does not exist.
	if !out.TokenConfigured {
		t.Error("tokenConfigured = false with a token-holding catalog")
	}
	if len(cat.queries) != 1 || cat.queries[0] != "llama 3.1" {
		t.Fatalf("catalog saw queries %v, want the decoded q", cat.queries)
	}
}

func TestModelSearchClampsTheLimitInsteadOfRejectingIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{"absent limit is the picker page", "?q=llama", defaultModelLimit},
		{"empty limit is the picker page", "?q=llama&limit=", defaultModelLimit},
		{"junk is not an error, it is the default", "?q=llama&limit=lots", defaultModelLimit},
		{"zero and negative are the default", "?q=llama&limit=0", defaultModelLimit},
		{"a negative limit is the default", "?q=llama&limit=-5", defaultModelLimit},
		{"a sensible limit is honoured", "?q=llama&limit=7", 7},
		{"the ceiling is honoured exactly", "?q=llama&limit=50", maxModelLimit},
		// The ceiling is the point: `limit` is caller-supplied, and one request
		// must not be able to ask the Hub — or this process's encoder — for ten
		// thousand cards.
		{"an absurd limit is clamped, not refused", "?q=llama&limit=10000", maxModelLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := &fakeCatalog{cards: []hf.ModelCard{llama8B}}
			rec := getAsDev(newModelServer(t, cat), "/v1/orgs/org_1/llm/models"+tc.query)
			if rec.Code != http.StatusOK {
				t.Fatalf("search → %d, want 200 (a limit is never a 400); body %s", rec.Code, rec.Body)
			}
			if len(cat.limits) != 1 || cat.limits[0] != tc.want {
				t.Fatalf("catalog saw limits %v, want [%d]", cat.limits, tc.want)
			}
		})
	}
}

// The state that decides whether this feature is safe to ship to self-hosters:
// nothing configured. Both routes must leave the wizard finishable.
func TestAControlPlaneWithNoCatalogStillHasAWorkingPicker(t *testing.T) {
	s := newModelServer(t, nil)

	rec := getAsDev(s, "/v1/orgs/org_1/llm/models?q=llama")
	if rec.Code != http.StatusOK {
		t.Fatalf("search with no catalog → %d, want 200 — a 503 here is a wizard step nobody can finish", rec.Code)
	}
	// Decoded into a typed slice on purpose: `"models":null` unmarshals happily
	// into nil here, so the assertion that catches it is on the raw body.
	if body := rec.Body.String(); !strings.Contains(body, `"models":[]`) {
		t.Fatalf("body = %s, want an empty ARRAY — the dashboard maps over this", body)
	}
	var out struct {
		TokenConfigured bool `json:"tokenConfigured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.TokenConfigured {
		t.Error("tokenConfigured = true with no catalog at all")
	}

	// Resolve cannot confirm anything, so it 404s — within the wire contract,
	// and specifically NOT a 5xx, which would read as "the product is broken"
	// for a configuration that is merely minimal. The wizard then uses the id
	// as typed, and the store's fit check fails open for the same reason.
	rec = getAsDev(s, "/v1/orgs/org_1/llm/models/resolve?id=meta-llama/Llama-3.1-8B-Instruct")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("resolve with no catalog → %d, want 404; body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "used exactly as typed") {
		t.Fatalf("404 body does not tell the operator what happens next: %s", rec.Body)
	}
}

func TestResolveAnswersTheSameCardTheSearchWould(t *testing.T) {
	cat := &fakeCatalog{cards: []hf.ModelCard{llama8B}}
	rec := getAsDev(newModelServer(t, cat), "/v1/orgs/org_1/llm/models/resolve?id=meta-llama/Llama-3.1-8B-Instruct")
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve → %d, want 200; body %s", rec.Code, rec.Body)
	}
	var card hf.ModelCard
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatal(err)
	}
	// A bare card, not wrapped in an envelope: the contract is 200 ModelCard,
	// and the wizard feeds this object to the same renderer the search list uses.
	if card.ID != llama8B.ID || card.VRAMBytesRequired != llama8B.VRAMBytesRequired || card.Engine != "vllm" {
		t.Fatalf("card = %+v, want the search card unwrapped", card)
	}
	if len(cat.ids) != 1 || cat.ids[0] != "meta-llama/Llama-3.1-8B-Instruct" {
		t.Fatalf("catalog saw ids %v", cat.ids)
	}
}

func TestResolveOfAModelThatIsNotOnTheHubIs404(t *testing.T) {
	cat := &fakeCatalog{resolveErr: hf.ErrNotFound}
	rec := getAsDev(newModelServer(t, cat), "/v1/orgs/org_1/llm/models/resolve?id=meta-llama/Llama-9")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("resolve of an unknown repo → %d, want 404; body %s", rec.Code, rec.Body)
	}
	// The two things that actually explain a 404 here, and neither is guessable:
	// repo ids are case-sensitive, and a gated repo 404s until access is granted.
	body := rec.Body.String()
	if !strings.Contains(body, "meta-llama/Llama-9") || !strings.Contains(body, "case-sensitive") || !strings.Contains(body, "gated") {
		t.Fatalf("404 body does not name the id or the two reasons: %s", body)
	}
}

func TestResolveWithoutAnIdIs400NamingTheParameter(t *testing.T) {
	cat := &fakeCatalog{cards: []hf.ModelCard{llama8B}}
	s := newModelServer(t, cat)
	for _, path := range []string{
		"/v1/orgs/org_1/llm/models/resolve",
		"/v1/orgs/org_1/llm/models/resolve?id=",
		"/v1/orgs/org_1/llm/models/resolve?id=%20%20",
	} {
		rec := getAsDev(s, path)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s → %d, want 400; body %s", path, rec.Code, rec.Body)
		}
		// Decoded, not string-matched on the raw body: the parameter name is
		// quoted in the sentence and the encoder escapes those quotes, so a raw
		// match would be asserting on JSON escaping rather than on the message.
		var out struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.Error, `"id"`) {
			t.Fatalf("400 body does not name the missing parameter: %s", out.Error)
		}
	}
	if len(cat.ids) != 0 {
		t.Fatalf("an empty id was sent to the Hub anyway: %v", cat.ids)
	}
}

// A Hub outage is a bad GATEWAY, not a bad control plane. 500 "internal error"
// costs an operator an hour in our logs looking for a bug that is in their
// egress rules — and hides the fact that the wizard still works by hand.
func TestAHubOutageIs502AndSaysWhatToCheck(t *testing.T) {
	down := errors.New("Get \"https://huggingface.co/api/models\": dial tcp: i/o timeout")
	for _, tc := range []struct {
		name string
		cat  *fakeCatalog
		path string
	}{
		{"search", &fakeCatalog{searchErr: down}, "/v1/orgs/org_1/llm/models?q=llama"},
		{"resolve", &fakeCatalog{resolveErr: down}, "/v1/orgs/org_1/llm/models/resolve?id=meta-llama/Llama-3.1-8B-Instruct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := getAsDev(newModelServer(t, tc.cat), tc.path)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("%s during a Hub outage → %d, want 502; body %s", tc.name, rec.Code, rec.Body)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "huggingface.co") || !strings.Contains(body, "CP_HUGGING_FACE_TOKEN") {
				t.Errorf("502 body names neither the dependency nor the setting: %s", body)
			}
			// And it must say the wizard is not dead, because it isn't: the id can
			// be typed and the create-time fit check fails open on this same error.
			if !strings.Contains(body, "by hand") {
				t.Errorf("502 body does not say the operator can still proceed: %s", body)
			}
		})
	}
}

// Both routes sit at the same Developer bar as the engine list, so completing
// the wizard's LLM step needs exactly one role — and neither is public.
func TestTheModelRoutesAreMemberVisibleReads(t *testing.T) {
	s := New(slog.Default(), fakePinger{}, &fakeStore{serviceTokens: map[string]store.ServicePrincipal{
		"dev-token": {OrgID: "org_1", Name: "reader", Role: store.RoleDeveloper},
	}}, &fakeDomain{}, Options{Models: &fakeCatalog{cards: []hf.ModelCard{llama8B}}})

	for _, path := range []string{
		"/v1/orgs/org_1/llm/models?q=llama",
		"/v1/orgs/org_1/llm/models/resolve?id=meta-llama/Llama-3.1-8B-Instruct",
	} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer dev-token")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s as Developer → %d, want 200; body %s", path, rec.Code, rec.Body)
		}

		rec = httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated → %d, want 401", path, rec.Code)
		}
	}
}
