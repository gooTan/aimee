/* wfe_autonomous_route.h -- S4 autonomous-parity routing policy (PURE).
 *
 * S4 of primary-as-manager: route the AUTONOMOUS surfaces (/v1/dev/submit with an
 * omitted workflow, and sweep filing) through the same request->workflow router
 * as the interactive surface. This is the pure decision policy; the catalog load + the
 * wfe_router_decide call + the lifecycle_event logging are integration layers in
 * the server (rh_dev_submit / server_sweep) built ON TOP of this core, kept out
 * of here so the policy is unit-testable with no I/O.
 *
 * Rulings folded from the S4 design roundtable (2026-07-03, non-degraded 6/6):
 *  - The omitted-workflow floor is the canonical `build` workflow.
 *  - Q5 (the fourth invariant): the autonomous router's selectable set is a
 *    fixed FULL-SPINE allowlist that excludes read-only AND review-bypassing
 *    lanes -- a shaped proposal can at worst pick among spine-carrying lanes,
 *    never bypass review.
 *  - Sweep filing is disabled until a canonical human-review workflow exists.
 */
#ifndef DEC_WFE_AUTONOMOUS_ROUTE_H
#define DEC_WFE_AUTONOMOUS_ROUTE_H 1

/* The autonomous omitted-workflow floor: a full-spine enforced workflow.
 * Mirrored as AutonomousFloor in server-go/modules/workflows/autonomous.go; both
 * sides assert the value so they cannot drift. */
#define WFE_AUTONOMOUS_FLOOR "build"

/* The sweep human-gate floor, mirrored as SweepWorkflowFloor module-side. */
#define WFE_SWEEP_WORKFLOW_FLOOR ""

/* Longest workflow id the clamp will hold. A name that does not fit is treated
 * as not selectable rather than truncated into a different lane. */
#define WFE_AUTONOMOUS_ID_MAX 128

/* 1 if a router-selected workflow may be AUTO-selected for an autonomous run.
 * The predicate is the catalog `enforced` flag: by the I2 loader invariant an
 * enforced workflow MUST terminate in gate.deliver, so `enforced` lanes are
 * exactly the full-spine (review + roundtable + deliver) lanes -- read-only
 * (converse/research) and pre-gate.deliver (build) lanes are enforced:false and
 * are never auto-selected. A defensive name check additionally rejects the
 * (I2-impossible) enforced-but-known-read-only case. `id` may be NULL. */
int wfe_autonomous_selectable(const char *id, int enforced);

/* Clamp a router decision to the autonomous selectable set. Returns `router_id`
 * when it is selectable, else WFE_AUTONOMOUS_FLOOR. *out_clamped (nullable) is
 * set to 1 when the floor was substituted, 0 otherwise. */
const char *wfe_autonomous_clamp(const char *router_id, int enforced, int *out_clamped);

/* The sweep human-gate floor. Empty disables filing rather than routing unvetted
 * candidates into an auto-executing workflow. */
const char *wfe_sweep_workflow_floor(void);

#endif /* DEC_WFE_AUTONOMOUS_ROUTE_H */
