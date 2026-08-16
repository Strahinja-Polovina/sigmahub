package billingsync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

type fakeStore struct {
	drift    []store.SubscriptionDrift
	driftErr error
	recorded map[string]int
	recErr   error
}

func (f *fakeStore) SubscriptionsNeedingQuantitySync(context.Context, time.Time) ([]store.SubscriptionDrift, error) {
	return f.drift, f.driftErr
}

func (f *fakeStore) RecordQuantitySynced(_ context.Context, orgID string, quantity int, _ string) error {
	if f.recErr != nil {
		return f.recErr
	}
	if f.recorded == nil {
		f.recorded = map[string]int{}
	}
	f.recorded[orgID] = quantity
	return nil
}

type fakePaddle struct {
	calls    map[string]int
	attempts map[string]int
	failFor  string
	failAll  bool
}

func (f *fakePaddle) UpdateSubscriptionQuantity(_ context.Context, subscriptionID, _ string, quantity int) error {
	if f.attempts == nil {
		f.attempts = map[string]int{}
	}
	f.attempts[subscriptionID]++
	if f.failAll || subscriptionID == f.failFor {
		return errors.New("paddle 500")
	}
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[subscriptionID] = quantity
	return nil
}

func TestSyncPushesDriftAndRecords(t *testing.T) {
	st := &fakeStore{drift: []store.SubscriptionDrift{
		{OrgID: "org_a", SubscriptionID: "sub_a", Billed: 1, Want: 21},
	}}
	pd := &fakePaddle{}
	s := &Syncer{Store: st, Paddle: pd, PriceID: "pri_1"}

	n, err := s.Sync(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("synced = %d, want 1", n)
	}
	if pd.calls["sub_a"] != 21 {
		t.Errorf("paddle got quantity %d, want 21", pd.calls["sub_a"])
	}
	if st.recorded["org_a"] != 21 {
		t.Errorf("recorded quantity %d, want 21", st.recorded["org_a"])
	}
}

// A Paddle failure on one org must not stop the others, and must NOT record a
// synced quantity — recording would debounce a push that never landed, leaving
// that org mis-billed until the debounce expired.
func TestSyncSkipsFailedOrgWithoutRecording(t *testing.T) {
	st := &fakeStore{drift: []store.SubscriptionDrift{
		{OrgID: "org_a", SubscriptionID: "sub_a", Billed: 1, Want: 4},
		{OrgID: "org_b", SubscriptionID: "sub_b", Billed: 9, Want: 2},
	}}
	pd := &fakePaddle{failFor: "sub_a"}
	s := &Syncer{Store: st, Paddle: pd, PriceID: "pri_1"}

	n, err := s.Sync(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("synced = %d, want 1 (org_b only)", n)
	}
	if _, ok := st.recorded["org_a"]; ok {
		t.Error("a failed push must not be recorded as synced")
	}
	if st.recorded["org_b"] != 2 {
		t.Errorf("org_b recorded %d, want 2", st.recorded["org_b"])
	}
}

// A pass in which every push failed, over a population big enough to mean
// something, on enough consecutive passes — the shape of a rotated Paddle key or
// a wrong price id — returns a non-nil error so the usage-sweep heartbeat's
// last-success clock can go stale instead of reporting green over silent revenue
// drift (SIGMA-365). A single failure amid successes stays non-fatal
// (TestSyncSkipsFailedOrgWithoutRecording).
func TestSyncSignalsSystemicFailure(t *testing.T) {
	drift := make([]store.SubscriptionDrift, 0, 3)
	for _, id := range []string{"a", "b", "c"} {
		drift = append(drift, store.SubscriptionDrift{
			OrgID: "org_" + id, SubscriptionID: "sub_" + id, Billed: 1, Want: 4,
		})
	}
	st := &fakeStore{drift: drift}
	// failAll → every update fails, which is what a rotated key looks like.
	s := &Syncer{Store: st, Paddle: &fakePaddle{failAll: true}, PriceID: "pri_1"}

	// It takes persistence: a Paddle blip that clears must not flap the
	// heartbeat, so the first passes are quiet.
	for i := 1; i < systemicPasses; i++ {
		if n, err := s.Sync(context.Background(), time.Now()); err != nil || n != 0 {
			t.Fatalf("pass %d = (%d, %v), want (0, nil) — the signal needs %d consecutive passes with evidence",
				i, n, err, systemicPasses)
		}
	}
	// ...and then it reports. Not necessarily on the very next pass: the per-org
	// backoff can defer a pass, and a deferred pass attempts nothing and so
	// proves nothing, deliberately neither advancing the streak nor clearing it.
	// What is asserted is that the outage reaches the heartbeat promptly, not
	// that it lands on an exact tick.
	var reported bool
	for i := 0; i < systemicPasses+3 && !reported; i++ {
		_, err := s.Sync(context.Background(), time.Now())
		reported = err != nil
	}
	if !reported {
		t.Fatalf("a fleet-wide outage must be reported within a few passes of %d consecutive failures", systemicPasses)
	}
	if _, ok := st.recorded["org_a"]; ok {
		t.Error("a failed push must not be recorded")
	}
}

// A control plane with ONE paying customer must still learn that its billing is
// broken (SIGMA-365).
//
// The first fix for the permanently-red alert demanded three simultaneously
// drifted subscriptions before a pass could count as systemic. That is silence
// on exactly the deployments where it matters most: two customers never produce
// three drifted subscriptions, so a rotated API key would have stopped all
// billing with nothing anywhere saying so. Population was never the
// discriminator — with one subscription, "one bad tenant" and "everything is
// broken" are the same observation, and the only honest answer is to report it.
func TestASingleFailingSubscriptionStillReportsEventually(t *testing.T) {
	st := &fakeStore{drift: []store.SubscriptionDrift{
		{OrgID: "org_a", SubscriptionID: "sub_a", Billed: 1, Want: 4},
	}}
	s := &Syncer{Store: st, Paddle: &fakePaddle{failFor: "sub_a"}, PriceID: "pri_1"}

	var reported bool
	for i := 0; i < 12 && !reported; i++ {
		_, err := s.Sync(context.Background(), time.Now())
		reported = err != nil
	}
	if !reported {
		t.Fatal("a lone subscription that never syncs must reach the heartbeat — at launch scale it is the whole fleet")
	}
}

// ...but it must not hold the heartbeat red, which is what made the original
// signal useless. Most passes stay green, so the alert reads as an event rather
// than a stuck light, and the real fleet-wide outage still has somewhere to land.
func TestAPermanentFailureAlertsPeriodicallyRatherThanContinuously(t *testing.T) {
	st := &fakeStore{drift: []store.SubscriptionDrift{
		{OrgID: "org_a", SubscriptionID: "sub_a", Billed: 1, Want: 4},
	}}
	s := &Syncer{Store: st, Paddle: &fakePaddle{failFor: "sub_a"}, PriceID: "pri_1"}

	const passes = 40
	errs := 0
	for i := 0; i < passes; i++ {
		if _, err := s.Sync(context.Background(), time.Now()); err != nil {
			errs++
		}
	}
	if errs == 0 {
		t.Fatal("a subscription broken for 40 passes must report at least once")
	}
	// The original fired on every single pass. Anything near that is a channel
	// nobody reads by the time the real outage arrives.
	if errs > passes/4 {
		t.Fatalf("reported on %d of %d passes — that is a stuck light, not an alert", errs, passes)
	}
}

// ...and it stops being hammered. Ten minutes apart forever is not free — it is
// a Paddle call per pass for a subscription that will never accept one — so a
// repeatedly failing org is retried on a widening interval instead.
func TestRepeatedlyFailingOrgIsBackedOff(t *testing.T) {
	st := &fakeStore{drift: []store.SubscriptionDrift{
		{OrgID: "org_a", SubscriptionID: "sub_a", Billed: 1, Want: 4},
	}}
	pd := &fakePaddle{failFor: "sub_a"}
	s := &Syncer{Store: st, Paddle: pd, PriceID: "pri_1"}

	// The verdict is not what is under test here (a lone failing subscription
	// does eventually report — see the tests above); the ATTEMPT COUNT is.
	for i := 0; i < 12; i++ {
		_, _ = s.Sync(context.Background(), time.Now())
	}
	if pd.attempts["sub_a"] >= 12 {
		t.Fatalf("attempted %d times over 12 passes — the backoff never engaged",
			pd.attempts["sub_a"])
	}
	// ...but it is never abandoned: an org whose problem gets fixed has to
	// re-sync without an operator restarting anything.
	pd.failFor = ""
	var recovered bool
	for i := 0; i < maxCooldownPasses+1 && !recovered; i++ {
		n, _ := s.Sync(context.Background(), time.Now())
		recovered = n == 1
	}
	if !recovered {
		t.Fatalf("a recovered subscription must re-sync within %d passes", maxCooldownPasses+1)
	}
}

// Backoff must not swallow the systemic case. Every org failing at once is the
// outage the signal exists for; skipping those orgs would starve the streak of
// the attempts that prove it, and the alert would never fire.
func TestSystemicFailureIsNotBackedOffIntoSilence(t *testing.T) {
	drift := []store.SubscriptionDrift{
		{OrgID: "org_a", SubscriptionID: "sub_a", Want: 4},
		{OrgID: "org_b", SubscriptionID: "sub_b", Want: 5},
		{OrgID: "org_c", SubscriptionID: "sub_c", Want: 6},
	}
	pd := &fakePaddle{failAll: true}
	s := &Syncer{Store: &fakeStore{drift: drift}, Paddle: pd, PriceID: "pri_1"}

	var reported bool
	for i := 0; i < systemicPasses+2 && !reported; i++ {
		_, err := s.Sync(context.Background(), time.Now())
		reported = err != nil
	}
	if !reported {
		t.Fatal("a fleet-wide outage must reach the heartbeat, not be backed off into silence")
	}
	// Every org attempted on every pass up to the report — no backoff applied.
	for _, d := range drift {
		if pd.attempts[d.SubscriptionID] < systemicPasses {
			t.Errorf("%s attempted %d times, want at least %d",
				d.SubscriptionID, pd.attempts[d.SubscriptionID], systemicPasses)
		}
	}
}

// A pass that pushes something clears the streak: the signal means "nothing is
// getting through", and something got through.
func TestSystemicStreakResetsOnAnySuccess(t *testing.T) {
	drift := []store.SubscriptionDrift{
		{OrgID: "org_a", SubscriptionID: "sub_a", Want: 4},
		{OrgID: "org_b", SubscriptionID: "sub_b", Want: 5},
		{OrgID: "org_c", SubscriptionID: "sub_c", Want: 6},
	}
	pd := &fakePaddle{failAll: true}
	s := &Syncer{Store: &fakeStore{drift: drift}, Paddle: pd, PriceID: "pri_1"}

	// One failing pass, then one that gets everything through, then failing
	// again. Without the reset the third pass would be the third consecutive
	// failure and would report; with it, the count starts over.
	if _, err := s.Sync(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	pd.failAll = false
	if _, err := s.Sync(context.Background(), time.Now()); err != nil {
		t.Fatalf("a pass that synced everything is not systemic: %v", err)
	}
	pd.failAll = true
	for i := 1; i < systemicPasses; i++ {
		if _, err := s.Sync(context.Background(), time.Now()); err != nil {
			t.Fatalf("pass %d after a success must not report — the streak restarts at zero, got %v", i, err)
		}
	}
}

// A store write failure must likewise leave the org un-debounced so the next
// sweep retries, and must not be counted as synced.
func TestSyncDoesNotCountUnrecordedPush(t *testing.T) {
	st := &fakeStore{
		drift:  []store.SubscriptionDrift{{OrgID: "org_a", SubscriptionID: "sub_a", Billed: 1, Want: 4}},
		recErr: errors.New("db down"),
	}
	s := &Syncer{Store: st, Paddle: &fakePaddle{}, PriceID: "pri_1"}
	n, err := s.Sync(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("synced = %d, want 0", n)
	}
}

// No Paddle credentials (or no price id) → the sweep is an honest no-op rather
// than a half-configured billing path.
func TestSyncNoopWhenUnconfigured(t *testing.T) {
	st := &fakeStore{drift: []store.SubscriptionDrift{{OrgID: "org_a", SubscriptionID: "sub_a", Want: 4}}}
	for _, s := range []*Syncer{
		{Store: st, Paddle: nil, PriceID: "pri_1"},
		{Store: st, Paddle: &fakePaddle{}, PriceID: ""},
	} {
		n, err := s.Sync(context.Background(), time.Now())
		if err != nil || n != 0 {
			t.Errorf("unconfigured sync = (%d, %v), want (0, nil)", n, err)
		}
	}
}
