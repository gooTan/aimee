package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	delegateapi "github.com/JBailes/aimee/server-go/delegate"
	"github.com/JBailes/aimee/server-go/internal/db1"
)

const (
	modelHeartbeatInterval = 15 * time.Second
)

const maxObservableDiagnosticBytes = 1024

func observableDiagnostic(detail string) string {
	return delegateapi.SafeDiagnosticSummary(detail, maxObservableDiagnosticBytes)
}

// observableAgents decorates the single resource-plane boundary used by both
// ordinary delegates and roundtable seats. It records only safe operational
// metadata; prompts, responses, tool arguments, and hidden reasoning stay out
// of the lifecycle log.
type observableAgents struct {
	next               AgentClient
	db                 *db1.Store
	heartbeatEvery     time.Duration
	activityStaleAfter time.Duration
}

func modelActor(request DelegateRequest) string {
	if request.Delegate != "" {
		return request.Delegate
	}
	if request.Participant != "" {
		return request.Participant
	}
	return "unassigned"
}

func modelDetail(request DelegateRequest, extra string) string {
	wf := modelWorkflowContext(request)
	detail := delegateapi.FormatToolDetail(wf, delegateapi.ToolEvent{}, 0)
	if detail != "" {
		detail += " "
	}
	detail += fmt.Sprintf("tools=%t", request.Tools)
	if extra != "" {
		detail += " " + extra
	}
	return detail
}

func modelWorkflowContext(request DelegateRequest) *delegateapi.WorkflowContext {
	return &delegateapi.WorkflowContext{
		WorkItemID: request.WorkItemID,
		Stage:      request.Stage,
		Model:      request.Delegate,
		Role:       request.Role,
		Persona:    request.Persona,
		Phase:      durablePhase(request.DurableSlot),
		Invocation: request.ExecutionVersion,
	}
}

func (o observableAgents) record(request DelegateRequest, kind, actor, detail string) {
	if request.WorkItemID == "" || o.db == nil {
		return
	}
	_ = o.db.RecordEvent(context.Background(), request.WorkItemID, request.Stage, kind, actor, detail)
}

func durablePhase(slot string) string {
	switch {
	case strings.Contains(slot, ":chairman"):
		return "chairman"
	case strings.Contains(slot, ":discussion:"):
		return "discussion"
	case strings.Contains(slot, ":analysis"):
		return "analysis"
	default:
		if slot != "" {
			return "analysis"
		}
		return ""
	}
}

func (o observableAgents) recordToolEvents(request DelegateRequest, result DelegateResult) {
	if len(result.ToolEvents) == 0 || request.WorkItemID == "" || o.db == nil {
		return
	}
	wf := modelWorkflowContext(request)
	actor := result.Agent
	if actor == "" {
		actor = modelActor(request)
	}
	for _, ev := range result.ToolEvents {
		kind := delegateapi.ToolEventKind(ev.Status)
		detail := delegateapi.FormatToolDetail(wf, ev, 0)
		_, _ = o.db.RecordToolEventIfAbsent(context.Background(), request.WorkItemID, request.Stage, kind, actor, detail)
	}
}

func (o observableAgents) recordToolEventsGroup(request DelegateRequest, result DelegateGroupResult) {
	if len(result.ToolEvents) == 0 || request.WorkItemID == "" || o.db == nil {
		return
	}
	wf := modelWorkflowContext(request)
	actor := result.Participant
	if actor == "" {
		actor = modelActor(request)
	}
	for _, ev := range result.ToolEvents {
		kind := delegateapi.ToolEventKind(ev.Status)
		detail := delegateapi.FormatToolDetail(wf, ev, 0)
		_, _ = o.db.RecordToolEventIfAbsent(context.Background(), request.WorkItemID, request.Stage, kind, actor, detail)
	}
}

func (o observableAgents) observe(request DelegateRequest) func(string, string, string) {
	actor := modelActor(request)
	started := time.Now()
	identity := delegateapi.FormatToolDetail(modelWorkflowContext(request), delegateapi.ToolEvent{}, 0)
	o.record(request, "model_dispatch", actor, modelDetail(request, "status=running"))
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		interval := o.heartbeatEvery
		if interval <= 0 {
			interval = modelHeartbeatInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				status := "running"
				activity := "model_dispatch"
				activityAge := time.Since(started)
				if o.db != nil {
					event, ok, err := o.db.LatestModelActivity(context.Background(), request.WorkItemID, request.Stage, actor, identity)
					if err == nil && ok {
						activity = event.Kind
						if at, parseErr := time.ParseInLocation("2006-01-02 15:04:05", event.CreatedAt, time.UTC); parseErr == nil {
							activityAge = time.Since(at)
						}
					}
				}
				staleAfter := o.activityStaleAfter
				if staleAfter <= 0 {
					staleAfter = delegateapi.ModelActivityStaleAfter
				}
				if activityAge >= staleAfter {
					status = "possibly_stalled"
				}
				o.record(request, "model_heartbeat", actor,
					modelDetail(request, fmt.Sprintf("status=%s elapsed=%s last_activity=%s activity=%s",
						status, time.Since(started).Round(time.Second), activityAge.Round(time.Second), activity)))
			}
		}
	}()
	return func(kind, finalActor, extra string) {
		close(done)
		<-stopped
		if finalActor == "" {
			finalActor = actor
		}
		o.record(request, kind, finalActor,
			modelDetail(request, extra+" elapsed="+time.Since(started).Round(time.Millisecond).String()))
	}
}

func (o observableAgents) Delegate(ctx context.Context, request DelegateRequest) (DelegateResult, error) {
	finish := o.observe(request)
	result, err := o.next.Delegate(ctx, request)
	actor := result.Agent
	if actor == "" {
		actor = modelActor(request)
	}
	// Persist per-tool-call telemetry before the terminal model_* event so the
	// lifecycle timeline shows tool activity inside the model turn.
	o.recordToolEvents(request, result)
	if err != nil {
		availability := result.AvailabilityClass
		if availability == "" {
			availability = delegateapi.AvailabilityClassOf(err)
		}
		finish("model_error", actor, "status=failed availability="+string(availability)+" error="+observableDiagnostic(err.Error()))
		return result, err
	}
	finish("model_complete", actor, "status=complete")
	return result, nil
}

func (o observableAgents) DelegateGroup(ctx context.Context, requests []DelegateRequest) []DelegateGroupResult {
	group, ok := o.next.(DelegateGroupClient)
	if !ok {
		out := make([]DelegateGroupResult, len(requests))
		for i := range out {
			finish := o.observe(requests[i])
			out[i].Err = fmt.Errorf("delegate service does not support grouped delegation")
			finish("model_error", "", "status=failed availability=provider_cli_unavailable error=grouped_delegation_unsupported")
		}
		return out
	}
	finishes := make([]func(string, string, string), len(requests))
	for i := range requests {
		finishes[i] = o.observe(requests[i])
	}
	results := group.DelegateGroup(ctx, requests)
	for i := range requests {
		if i >= len(results) {
			finishes[i]("model_error", "", "status=failed error=missing_group_result")
			continue
		}
		result := results[i]
		o.recordToolEventsGroup(requests[i], result)
		if result.Err != nil {
			availability := result.AvailabilityClass
			if availability == "" {
				availability = delegateapi.AvailabilityClassOf(result.Err)
			}
			finishes[i]("model_error", modelActor(requests[i]), "status=failed availability="+string(availability)+" error="+observableDiagnostic(result.Err.Error()))
		} else {
			finishes[i]("model_complete", modelActor(requests[i]), "status=complete")
		}
	}
	return results
}

func (r *NativeRunner) recordModelEvent(workItemID, stage, kind, actor, detail string) {
	if workItemID != "" && r.db != nil {
		_ = r.db.RecordEvent(context.Background(), workItemID, stage, kind, actor, detail)
	}
}
