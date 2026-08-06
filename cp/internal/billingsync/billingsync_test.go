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
	calls   map[string]int
	failFor string
}

func (f *fakePaddle) UpdateSubscriptionQuantity(_ context.Context, subscriptionID, _ string, quantity int) error {
	if subscriptionID == f.failFor {
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
