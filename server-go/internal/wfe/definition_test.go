package wfe

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWatchDirectoryTriggerParamsAreValidatedWhenWorkflowIsSaved(t *testing.T) {
	definition := func(params string) []byte {
		return []byte(fmt.Sprintf(`
name: watcher
start: watch
nodes:
  - id: watch
    block: trigger.watch-dir
    params:
%s
`, params))
	}

	for _, tc := range []struct {
		name, params, want string
	}{
		{"workspace-type", "      workspace: 12", `param "workspace" must be a string`},
		{"workspace-relative", "      workspace: repos/demo", "absolute server path"},
		{"directory-type", "      dir: 12", `param "dir" must be a string`},
		{"directory-traversal", "      dir: ../private", "confined repository-relative directory"},
		{"directory-backslash", `      dir: 'docs\\private'`, "confined repository-relative directory"},
		{"ref-option", "      ref: --all", "start with '-'"},
		{"mode", "      mode: manual", "autonomous or interactive"},
		{"spend-negative", "      max_spend_usd: -1", "finite and non-negative"},
		{"spend-type", "      max_spend_usd: lots", "finite and non-negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDefinition(definition(tc.params))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}

	for _, params := range []string{
		"      workspace: /srv/repos/demo\n      dir: docs/proposals/pending\n      ref: testing\n      mode: interactive\n      max_spend_usd: 1.5",
		// A definition without a workspace is intentionally saved but disarmed;
		// the operator can fill it in later without an invalid workflow blocking boot.
		"      dir: docs/proposals/pending",
	} {
		if _, err := ParseDefinition(definition(params)); err != nil {
			t.Fatalf("valid watch trigger rejected: %v", err)
		}
	}
}

func TestCurrentBuildWorkflowParses(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "workflows", "build.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	def, err := ParseDefinition(content)
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != "build" || def.Version == "" {
		t.Fatalf("unexpected definition: name=%q version=%q", def.Name, def.Version)
	}
	gate, ok := def.Node("plan_gate")
	if !ok || gate.OnFail != "plan" {
		t.Fatalf("plan gate not preserved: %+v", gate)
	}
}

func TestDefinitionRejectsMissingProducer(t *testing.T) {
	_, err := ParseDefinition([]byte(`
name: broken
start: gate
nodes:
  - id: gate
    block: gate.roundtable
    in:
      src: missing.out
`))
	if err == nil {
		t.Fatal("missing producer was accepted")
	}
}

// A roundtable review that names no roundtable used to resolve an implicit
// panel nobody configured. The name is part of the block contract now, so a
// workflow that omits it must not parse at all.
func TestRoundtableGateWithoutANamedRoundtableIsRejected(t *testing.T) {
	definition := []byte(`
name: unnamed
start: plan
nodes:
  - id: plan
    block: author.plan
    in: {proposal: plan.out}
    next: gate
  - id: gate
    block: gate.roundtable
    in: {src: plan.out}
    params: {panel: {required: [qa]}}
`)
	_, err := ParseDefinition(definition)
	if err == nil {
		t.Fatal("gate.roundtable parsed with no roundtable named")
	}
	if !strings.Contains(err.Error(), `requires param "roundtable"`) {
		t.Fatalf("unexpected error: %v", err)
	}
	// A blank name is the same omission, not a different one.
	_, err = ParseDefinition(bytes.Replace(definition,
		[]byte("params: {panel:"), []byte(`params: {roundtable: "  ", panel:`), 1))
	if err == nil || !strings.Contains(err.Error(), `requires param "roundtable"`) {
		t.Fatalf("blank roundtable name accepted: %v", err)
	}
}

// Every workflow the image ships must name a roundtable that the image also
// ships, or the gate parks on a preset that does not exist.
func TestShippedWorkflowsNameAShippedRoundtable(t *testing.T) {
	workflowDir := filepath.Join("..", "..", "..", "config", "workflows")
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		t.Fatal(err)
	}
	gates := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(workflowDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		def, err := ParseDefinition(content)
		if err != nil {
			t.Fatalf("shipped workflow %s does not validate: %v", entry.Name(), err)
		}
		for _, node := range def.Nodes {
			if node.Block != "gate.roundtable" {
				continue
			}
			gates++
			name, _ := node.Params["roundtable"].(string)
			preset := filepath.Join("..", "..", "..", "config", "roundtables", name+".json")
			if _, err := os.Stat(preset); err != nil {
				t.Errorf("%s node %q names roundtable %q, which ships no preset: %v",
					entry.Name(), node.ID, name, err)
			}
		}
	}
	if gates == 0 {
		t.Fatal("no shipped roundtable gates found; this guard would pass vacuously")
	}
}

func TestCanonicalBuildWorkflowBindings(t *testing.T) {
	buildPath := filepath.Join("..", "..", "..", "config", "workflows", "build.yaml")
	content, err := os.ReadFile(buildPath)
	if err != nil {
		t.Fatal(err)
	}
	def, err := ParseDefinition(content)
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != "build" {
		t.Fatalf("build workflow name=%q, want build", def.Name)
	}
	if def.Start != "draft" {
		t.Fatalf("build start=%q, want draft", def.Start)
	}
	find := func(id string) (Node, bool) {
		for _, n := range def.Nodes {
			if n.ID == id {
				return n, true
			}
		}
		return Node{}, false
	}
	mustParamString := func(node Node, key string) string {
		v, _ := node.Params[key].(string)
		return v
	}
	mustParamBool := func(node Node, key string) bool {
		v, _ := node.Params[key].(bool)
		return v
	}
	mustParamInt := func(node Node, key string) int {
		if v, ok := numericInt(node.Params[key]); ok {
			return v
		}
		if v, ok := node.Params[key].(int); ok {
			return v
		}
		return 0
	}

	feature, ok := find("feature")
	if !ok {
		t.Fatal("build missing node feature")
	}
	if feature.Next != "prep" {
		t.Fatalf("feature.next=%q, want prep", feature.Next)
	}
	if feature.Block != "branch.open" {
		t.Fatalf("feature block=%q, want branch.open", feature.Block)
	}

	prep, ok := find("prep")
	if !ok {
		t.Fatal("build missing node prep")
	}
	if prep.Block != "understand" {
		t.Fatalf("prep block=%q, want understand", prep.Block)
	}
	if !mustParamBool(prep, "brief") {
		t.Fatalf("prep brief=%v, want true", prep.Params["brief"])
	}
	if mustParamString(prep, "delegate") != "luna" {
		t.Fatalf("prep delegate=%q, want luna", prep.Params["delegate"])
	}
	if mustParamInt(prep, "max_rounds") != 2 {
		t.Fatalf("prep max_rounds=%v, want 2", prep.Params["max_rounds"])
	}
	if prep.Next != "plan" || prep.OnFail != "prep" {
		t.Fatalf("prep edges next=%q on_fail=%q, want plan/prep", prep.Next, prep.OnFail)
	}

	plan, ok := find("plan")
	if !ok {
		t.Fatal("build missing node plan")
	}
	if plan.Block != "author.plan" {
		t.Fatalf("plan block=%q, want author.plan", plan.Block)
	}
	if got := plan.In["proposal"]; got != "prep.out" {
		t.Fatalf("plan proposal binding=%q, want prep.out", got)
	}
	if mustParamString(plan, "delegate") != "fable" {
		t.Fatalf("plan delegate=%q, want fable", plan.Params["delegate"])
	}
	if mustParamString(plan, "persona") != "architect" {
		t.Fatalf("plan persona=%q, want architect", plan.Params["persona"])
	}
	if !mustParamBool(plan, "require_brief") {
		t.Fatalf("plan require_brief=%v, want true", plan.Params["require_brief"])
	}
	if mustParamInt(plan, "max_rounds") != 6 {
		t.Fatalf("plan max_rounds=%v, want 6", plan.Params["max_rounds"])
	}
	if plan.Next != "plan_gate" || plan.OnFail != "prep" {
		t.Fatalf("plan edges next=%q on_fail=%q, want plan_gate/prep", plan.Next, plan.OnFail)
	}

	planGate, ok := find("plan_gate")
	if !ok {
		t.Fatal("build missing plan_gate")
	}
	if mustParamString(planGate, "roundtable") != "plan" {
		t.Fatalf("plan_gate roundtable=%q, want plan", planGate.Params["roundtable"])
	}
	if mustParamInt(planGate, "max_rounds") != 6 {
		t.Fatalf("plan_gate max_rounds=%v, want 6", planGate.Params["max_rounds"])
	}
	if planGate.OnPass != "split" || planGate.OnFail != "plan" {
		t.Fatalf("plan_gate edges on_pass=%q on_fail=%q, want split/plan", planGate.OnPass, planGate.OnFail)
	}
	if !strings.Contains(mustParamString(planGate, "focus"), "does this implementation plan") {
		t.Fatalf("plan_gate focus missing: %v", planGate.Params["focus"])
	}

	split, ok := find("split")
	if !ok {
		t.Fatal("build missing split")
	}
	if split.Block != "split" {
		t.Fatalf("split block=%q, want split", split.Block)
	}
	if mustParamString(split, "delegate") != "fable" {
		t.Fatalf("split delegate=%q, want fable", split.Params["delegate"])
	}
	if mustParamInt(split, "max_rounds") != 3 {
		t.Fatalf("split max_rounds=%v, want 3", split.Params["max_rounds"])
	}
	if split.Next != "slices" || split.OnFail != "split" {
		t.Fatalf("split edges next=%q on_fail=%q, want slices/split", split.Next, split.OnFail)
	}

	acceptGate, ok := find("accept_gate")
	if !ok {
		t.Fatal("build missing accept_gate")
	}
	if mustParamString(acceptGate, "roundtable") != "implementation" {
		t.Fatalf("accept_gate roundtable=%q, want implementation", acceptGate.Params["roundtable"])
	}

	docGate, ok := find("doc_gate")
	if !ok {
		t.Fatal("build missing doc_gate")
	}
	if mustParamString(docGate, "roundtable") != "documentation" {
		t.Fatalf("doc_gate roundtable=%q, want documentation", docGate.Params["roundtable"])
	}

	// Slice workflow: rt_gate must name implementation and impl must not pin a delegate.
	sliceContent, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "workflows", "slice.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sliceDef, err := ParseDefinition(sliceContent)
	if err != nil {
		t.Fatal(err)
	}
	var sliceGate Node
	found := false
	for _, n := range sliceDef.Nodes {
		if n.ID == "rt_gate" {
			sliceGate = n
			found = true
			break
		}
	}
	if !found {
		t.Fatal("slice missing rt_gate")
	}
	if got, _ := sliceGate.Params["roundtable"].(string); got != "implementation" {
		t.Fatalf("slice rt_gate roundtable=%q, want implementation", sliceGate.Params["roundtable"])
	}
	for _, n := range sliceDef.Nodes {
		if n.ID == "impl" {
			if _, has := n.Params["delegate"]; has {
				t.Fatalf("slice impl must not pin a delegate; native runner owns routing, got %v", n.Params["delegate"])
			}
		}
	}
}
