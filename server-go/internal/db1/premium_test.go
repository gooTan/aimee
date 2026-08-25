package db1

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openPremiumTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestRecordPremiumCallEnforcesCapPerRunTree(t *testing.T) {
	store := openPremiumTestStore(t)
	ctx := context.Background()
	if err := store.CreateWorkItem(ctx, CreateWorkItem{ID: "wi_root", Repo: "r",
		ProposalPath: "p", WorkflowName: "orchestrated-change", StartStage: "prep"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkItem(ctx, CreateWorkItem{ID: "wi_root.g0.0", Repo: "r",
		ProposalPath: "p2", WorkflowName: "slice", StartStage: "scope", ParentID: "wi_root"}); err != nil {
		t.Fatal(err)
	}

	if count, err := store.RecordPremiumCall(ctx, "wi_root", "plan", "fable", 2); err != nil || count != 1 {
		t.Fatalf("first premium call: count=%d err=%v", count, err)
	}
	// A child's premium call charges the ROOT ledger: the cap covers the tree.
	if count, err := store.RecordPremiumCall(ctx, "wi_root.g0.0", "second_opinion", "sol", 2); err != nil || count != 2 {
		t.Fatalf("child premium call: count=%d err=%v", count, err)
	}
	if _, err := store.RecordPremiumCall(ctx, "wi_root", "plan", "fable", 2); !errors.Is(err, ErrPremiumCallLimit) {
		t.Fatalf("third premium call err=%v, want ErrPremiumCallLimit", err)
	}
	if count, err := store.PremiumCallCount(ctx, "wi_root.g0.0"); err != nil || count != 2 {
		t.Fatalf("tree count from child: count=%d err=%v", count, err)
	}
}

func TestRecordPremiumCallZeroCapRefusesEverything(t *testing.T) {
	store := openPremiumTestStore(t)
	if _, err := store.RecordPremiumCall(context.Background(), "wi_any", "plan", "fable", 0); !errors.Is(err, ErrPremiumCallLimit) {
		t.Fatalf("zero cap err=%v, want ErrPremiumCallLimit", err)
	}
}

func TestPremiumLedgerFallsBackToItemIDWithoutLifecycleRow(t *testing.T) {
	store := openPremiumTestStore(t)
	ctx := context.Background()
	if count, err := store.RecordPremiumCall(ctx, "wi_orphan", "plan", "fable", 1); err != nil || count != 1 {
		t.Fatalf("orphan premium call: count=%d err=%v", count, err)
	}
	if _, err := store.RecordPremiumCall(ctx, "wi_orphan", "plan", "fable", 1); !errors.Is(err, ErrPremiumCallLimit) {
		t.Fatalf("orphan cap err=%v, want ErrPremiumCallLimit", err)
	}
	if count, err := store.PremiumCallCount(ctx, "wi_orphan"); err != nil || count != 1 {
		t.Fatalf("orphan count=%d err=%v", count, err)
	}
}
