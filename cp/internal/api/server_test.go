package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"testing"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestHealthz(t *testing.T) {
	s := New(slog.Default(), fakePinger{})
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"db up", nil, 200},
		{"db down", errors.New("nope"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(slog.Default(), fakePinger{err: tc.err})
			req := httptest.NewRequest("GET", "/readyz", nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("readyz = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
