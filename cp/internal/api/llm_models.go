package api

// The Hugging Face model picker's HTTP boundary (SIGMA-213/214).
//
// The wizard's LLM step used to be a text input labelled "model". Everything
// that could go wrong with it went wrong somewhere else and much later: a typo
// became a container that pulled for ninety seconds and exited, a gated repo
// became a 401 in `docker logs`, and a 70B model on a 24 GB card became an OOM
// on a host already billed at GPU rates. None of that is knowable to the person
// filling in the form, and all of it is knowable to the control plane — which
// is why these two routes exist at all.
//
// The control plane is the one that asks huggingface.co, for three reasons that
// are worth stating because each one is a defect that would otherwise be
// reintroduced by "the browser could just call the Hub directly":
//
//  1. The Hub token is a CREDENTIAL. It lists an org's private and gated repos;
//     it cannot be shipped to a browser.
//  2. The VRAM estimate must be computed ONCE. It lives in cp/internal/hf and
//     is rendered into every card as both bytes and a sentence, so the wizard's
//     "needs ~21 GB" and the store's create-time refusal cannot disagree — the
//     web never re-derives the formula, it compares the number it was handed.
//  3. The engine a model gets is a control-plane decision (the llm_engines
//     catalog), so the card carries the runtime the CP would actually render
//     rather than the runtime the dashboard guesses.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/hf"
)

// ModelCatalog is the Hugging Face slice the picker needs, narrowed to three
// calls the way RepoInspector narrows the GitHub client. Narrow on purpose: the
// handlers must not be able to reach the Hub for anything the picker does not
// display, and a fake in a test is three methods rather than an HTTP server.
type ModelCatalog interface {
	Search(ctx context.Context, query string, limit int) ([]hf.ModelCard, error)
	Resolve(ctx context.Context, repoID string) (hf.ModelCard, error)
	// TokenConfigured reports whether this control plane holds a Hub token for
	// its own metadata calls. It shapes what a SEARCH can return — without one
	// the results are public repos only — and it is deliberately NOT what the
	// response's tokenConfigured field carries: see handleSearchModels.
	TokenConfigured() bool
}

// Search result bounds. The default is what the picker renders without
// scrolling; the ceiling exists because `limit` is caller-supplied and one
// request must not be able to ask the Hub — or this process's JSON encoder —
// for ten thousand cards.
const (
	defaultModelLimit = 20
	maxModelLimit     = 50
)

// modelLimit clamps rather than rejects. A limit outside the range is not a
// mistake worth a 400: every value it could hold still describes a picker page,
// so the useful answer is the nearest one we will serve. Junk and absence both
// mean "you choose", which is the default.
func modelLimit(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return defaultModelLimit
	}
	if n > maxModelLimit {
		return maxModelLimit
	}
	return n
}

// handleSearchModels answers the picker's typeahead.
//
// With no catalog wired the answer is an EMPTY list, not a 503. That is a
// deliberate product choice and the difference between a degraded step and a
// dead one: the dashboard renders "no catalog — type a model id" and the
// operator finishes the wizard, whereas a 503 leaves them on a step with
// nothing to click. The same shape already covers "the Hub returned nothing for
// `qwn3`", so the dashboard needs no second branch for it.
//
// `tokenConfigured` travels with the results and is NOT the catalog's own
// token. The wizard uses it for one decision — may this operator deploy a GATED
// model — and that decision is about the DOWNLOAD the agent performs on a GPU
// host, not about the metadata lookup this process just made. Reporting the
// picker's token there produced both wrong answers at once: with
// CP_HUGGING_FACE_TOKEN set the wizard approved a gated model whose weights
// nothing was authenticated to fetch, and with only the org's
// HUGGING_FACE_HUB_TOKEN secret set it blocked the model step and told the
// operator to go configure a variable that would not have helped. The wire name
// is kept because it is what the dashboard already reads; only its meaning is
// now the true one — see store.WeightsTokenAvailable.
func (s *Server) handleSearchModels(w http.ResponseWriter, r *http.Request) {
	limit := modelLimit(r.URL.Query().Get("limit"))
	tokenConfigured := s.weightsTokenAvailable(r)
	if s.models == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"models": []hf.ModelCard{}, "tokenConfigured": tokenConfigured,
		})
		return
	}
	cards, err := s.models.Search(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")), limit)
	if err != nil {
		s.writeHubErr(w, err, "search models")
		return
	}
	// An empty result must encode as [] and never null: the dashboard maps over
	// this array, and a null here is a runtime error in the picker rather than
	// the empty state it already knows how to draw.
	if cards == nil {
		cards = []hf.ModelCard{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models": cards, "tokenConfigured": tokenConfigured,
	})
}

// weightsTokenAvailable answers "will a model created here be able to fetch its
// weights", from the optional `projectId` and `environmentId` the picker sends.
//
// Both are optional, and their absence is not an error: without them the answer
// narrows rather than fails. With no project it is the control-plane half alone
// (a token here is seeded into every endpoint the org creates), which is right
// for every project that has not set its own; with a project but no environment
// it counts only the org-wide secrets, because a HUGGING_FACE_HUB_TOKEN scoped
// to staging is not a promise about production — the create there would seed
// nothing and the pull would 401 forty gigabytes in.
//
// Unknown answers FALSE, and that direction is chosen, not defaulted. False
// makes the wizard warn about gated models — noise for someone who has a token
// we failed to confirm. True suppresses the warning, and the failure on the
// other side of that is a gated model accepted, scheduled, and 401'd tens of
// gigabytes into a pull on a host billed at GPU rates. The dashboard's own
// catalogue call already fails this way for the same reason.
func (s *Server) weightsTokenAvailable(r *http.Request) bool {
	if s.llm == nil {
		return false
	}
	q := r.URL.Query()
	ok, err := s.llm.WeightsTokenAvailable(r.Context(), r.PathValue("orgId"),
		strings.TrimSpace(q.Get("projectId")), strings.TrimSpace(q.Get("environmentId")))
	if err != nil {
		s.log.Error("weights token lookup", "err", err)
		return false
	}
	return ok
}

// handleResolveModel answers for one repo id — the call the wizard makes when a
// model arrives from somewhere other than the picker (a pasted id, an edited
// resource, a deep link). It is the same card the search returns, so the fit
// verdict is computed from identical numbers whichever way the model got here.
func (s *Server) handleResolveModel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `the "id" query parameter is required: pass ?id=<owner>/<model>, the repo id exactly as it appears on huggingface.co`,
		})
		return
	}
	if s.models == nil {
		// 404 rather than 503 for the same reason the search answers empty: this
		// control plane cannot confirm the model, and the wizard's free-text path
		// (which tokenConfigured=false already put it on) accepts the id as typed.
		// A 5xx would read as "the product is broken" for a configuration that is
		// merely minimal.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this control plane has no Hugging Face catalog configured, so " + id +
				" cannot be looked up — the id will be used exactly as typed",
		})
		return
	}
	card, err := s.models.Resolve(r.Context(), id)
	if errors.Is(err, hf.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no model " + id + " on huggingface.co — check the repo id (it is owner/name, " +
				"case-sensitive), or request access if the repo is gated",
		})
		return
	}
	if err != nil {
		s.writeHubErr(w, err, "resolve model")
		return
	}
	writeJSON(w, http.StatusOK, card)
}

// writeHubErr maps an outbound Hugging Face failure to 502.
//
// It is a separate helper from writeStoreErr because the mistake it prevents is
// specific: a Hub timeout is not this control plane malfunctioning, and 500
// "internal error" sends the operator to the CP logs to look for a bug that is
// not there. 502 plus a sentence naming the dependency and the way out sends
// them to their egress rules instead — and tells them the wizard still works,
// because the model id can be typed and the create-time fit check fails open on
// exactly this error.
func (s *Server) writeHubErr(w http.ResponseWriter, err error, op string) {
	s.log.Error(op, "err", err)
	writeJSON(w, http.StatusBadGateway, map[string]string{
		"error": "huggingface.co could not be reached from this control plane — check its outbound " +
			"HTTPS access (and CP_HUGGING_FACE_TOKEN if the repo is private or gated). " +
			"A model id can still be entered by hand.",
	})
}
