package supervise

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPassConvertsPanicToError(t *testing.T) {
	err := Pass(discard(), "sweeper", func() error {
		var m map[string]string
		m["boom"] = "x" // assignment to a nil map
		return nil
	})
	if err == nil {
		t.Fatal("a panicking pass reported success")
	}
	if !strings.Contains(err.Error(), "panic in sweeper") {
		t.Fatalf("error = %q, want it to name the loop", err)
	}
}

// The recover must not swallow the deferred cleanup a pass had in flight —
// an advisory lock released on the way out is the case that matters.
func TestPassRunsTheDefersOfThePanickingBody(t *testing.T) {
	unlocked := false
	_ = Pass(discard(), "reconciler_resync", func() error {
		defer func() { unlocked = true }()
		panic("render helper indexed past the end")
	})
	if !unlocked {
		t.Fatal("the panicking body's defers did not run")
	}
}

func TestPassPassesOrdinaryResultsThrough(t *testing.T) {
	want := errors.New("lock timeout")
	if got := Pass(discard(), "sweeper", func() error { return want }); !errors.Is(got, want) {
		t.Fatalf("err = %v, want %v", got, want)
	}
	if got := Pass(discard(), "sweeper", func() error { return nil }); got != nil {
		t.Fatalf("err = %v, want nil", got)
	}
}
