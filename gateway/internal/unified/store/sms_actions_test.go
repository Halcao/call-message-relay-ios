package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSMSActionDurabilityTupleAndIsolation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(1000)
	apply := func(key, id, payload, kind, through string) {
		t.Helper()
		if _, err := s.ApplyKeyThreadAction(ctx, key, "device", id, payload, kind, "line", "thread", at, through); err != nil {
			t.Fatal(err)
		}
	}
	apply("a", "read", "r", "read", "m2")
	state, err := s.KeyThreadState(ctx, "a", "line", "thread")
	if err != nil {
		t.Fatal(err)
	}
	if !SMSBoundaryCovers(state.ReadHorizon, state.ReadThroughID, 1000, "m1") || SMSBoundaryCovers(state.ReadHorizon, state.ReadThroughID, 1000, "m3") {
		t.Fatal("read tuple incorrect")
	}
	apply("a", "delete", "d", "delete", "m2")
	if _, err = s.ApplyKeyThreadAction(ctx, "a", "device", "delete", "different", "delete", "line", "thread", at, "m3"); !errors.Is(err, ErrSMSActionConflict) {
		t.Fatalf("conflict %v", err)
	}
	apply("b", "delete", "independent", "delete", "m1")
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if applied, err := s.ApplyKeyThreadAction(ctx, "a", "device", "delete", "d", "delete", "line", "thread", at, "m2"); err != nil || applied {
		t.Fatalf("replay %v %v", applied, err)
	}
	state, _ = s.KeyThreadState(ctx, "a", "line", "thread")
	if !state.Hidden || state.DeleteThroughID != "m2" || SMSBoundaryCovers(state.DeleteHorizon, state.DeleteThroughID, 1000, "m3") || SMSBoundaryCovers(state.DeleteHorizon, state.DeleteThroughID, 1001, "m0") {
		t.Fatalf("delete tuple %+v", state)
	}
	if err = s.HideKeyThread(ctx, "a", "line", "thread", "device", at); err != nil {
		t.Fatal(err)
	}
	state, _ = s.KeyThreadState(ctx, "a", "line", "thread")
	if !SMSBoundaryCovers(state.DeleteHorizon, state.DeleteThroughID, 1000, "zz") {
		t.Fatal("legacy timestamp compatibility")
	}
}
