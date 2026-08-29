package db1

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrPremiumCallLimit means the run tree has already spent its premium-call
// allowance. It is a durable policy stop, not a transient outage: the engine
// parks the item for a human instead of retrying the dispatch.
var ErrPremiumCallLimit = errors.New("premium delegate call limit reached for this workflow run")

// RecordPremiumCall admits one premium-delegate dispatch for the run tree that
// contains workItemID, or refuses with ErrPremiumCallLimit. The count and the
// insert happen inside one transaction so concurrent siblings cannot overshoot
// the cap. The ledger is written before dispatch on purpose: a dispatch whose
// result is lost still spent the caller's subscription quota, so it must count.
func (s *Store) RecordPremiumCall(ctx context.Context, workItemID, stage, delegate string, maxCalls int) (int, error) {
	if maxCalls <= 0 {
		return 0, ErrPremiumCallLimit
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rootID, err := premiumRoot(ctx, tx, workItemID)
	if err != nil {
		return 0, err
	}
	var used int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wfe_premium_call WHERE root_id=?`, rootID).Scan(&used); err != nil {
		return 0, err
	}
	if used >= maxCalls {
		return used, fmt.Errorf("%w: %d of %d used", ErrPremiumCallLimit, used, maxCalls)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO wfe_premium_call(root_id, work_item_id, stage, delegate) VALUES(?,?,?,?)`,
		rootID, workItemID, stage, delegate); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return used + 1, nil
}

// PremiumCallCount reports how many premium dispatches the run tree containing
// workItemID has recorded.
func (s *Store) PremiumCallCount(ctx context.Context, workItemID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rootID, err := premiumRoot(ctx, tx, workItemID)
	if err != nil {
		return 0, err
	}
	var used int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wfe_premium_call WHERE root_id=?`, rootID).Scan(&used); err != nil {
		return 0, err
	}
	return used, tx.Commit()
}

// premiumRoot resolves the root work item so the cap covers a whole run tree,
// including slices spawned by foreach.workflow. A missing row degrades to the
// item's own ID: the caller may be recording against an item admitted outside
// the lifecycle store (tests, ad hoc runs), and a premium call must never be
// dropped from the ledger because ancestry lookup failed.
func premiumRoot(ctx context.Context, tx *sql.Tx, workItemID string) (string, error) {
	var rootID string
	err := tx.QueryRowContext(ctx, `WITH RECURSIVE ancestors(id,parent_id) AS (
  SELECT work_item_id,parent_id FROM lifecycle_work_item WHERE work_item_id=?
  UNION ALL
  SELECT parent.work_item_id,parent.parent_id
  FROM lifecycle_work_item parent JOIN ancestors child ON child.parent_id=parent.work_item_id
)
SELECT id FROM ancestors WHERE parent_id='' LIMIT 1`, workItemID).Scan(&rootID)
	if errors.Is(err, sql.ErrNoRows) {
		return workItemID, nil
	}
	if err != nil {
		return "", err
	}
	return rootID, nil
}
