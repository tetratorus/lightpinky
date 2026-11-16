package netmux

import (
	"context"
	"errors"
	"testing"

	"github.com/tetratorus/lightpinky/internal/mdns"
)

type testSource struct {
	err error
}

func (s *testSource) Services() []mdns.Service { return nil }
func (s *testSource) Err() error               { return s.err }
func (s *testSource) Close() error             { return nil }

func TestDiscoveryErrorAndRecovery(t *testing.T) {
	src := &testSource{err: mdns.ErrNotAllowed}
	f := &Finder{Mux: New("", nil), BrowseFn: func() (Source, error) { return src, nil }}
	s := newFindState(f, f.logOrDiscard())
	s.tick(context.Background())
	if !errors.Is(f.Err(), mdns.ErrNotAllowed) {
		t.Fatalf("discovery error hidden: %v", f.Err())
	}
	src.err = nil
	s.tick(context.Background())
	if err := f.Err(); err != nil {
		t.Fatalf("stale discovery error after recovery: %v", err)
	}
}

func TestDiscoveryStartError(t *testing.T) {
	f := &Finder{Mux: New("", nil), BrowseFn: func() (Source, error) {
		return nil, mdns.ErrNoNetwork
	}}
	s := newFindState(f, f.logOrDiscard())
	s.tick(context.Background())
	if !errors.Is(f.Err(), mdns.ErrNoNetwork) {
		t.Fatalf("discovery startup error hidden: %v", f.Err())
	}
}
