package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writePreset(t *testing.T, dir string, p preset) {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, p.Name+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredRoundtablePreservesExactSeatSpecifications(t *testing.T) {
	dir := t.TempDir()
	writePreset(t, dir, preset{Name: "large", MinSuccessful: 2, Discussion: true, DeadlineMS: 12345, Chairman: "$random", ChairmanFallback: "codex", ChairmanEnabled: true, Seats: []presetSeat{
		{Selector: "$random", Persona: "security"},
		{Selector: "codex", Persona: "qa"},
		{Selector: "$random", Persona: "architect"},
	}})
	store, _ := NewStore(dir)
	panel, err := store.Resolve("large", []string{"reviewer"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !panel.Acquired || panel.Name != "large" || len(panel.Seats) != 3 || panel.MinSuccessful != 2 || !panel.Discussion || panel.DeadlineMS != 12345 || !panel.ChairmanEnabled || panel.Chairman != "$random" || panel.ChairmanFallback != "codex" {
		t.Fatalf("panel=%+v", panel)
	}
	if panel.Seats[0].Selector != "$random" || panel.Seats[1].Selector != "codex" || panel.Seats[2].Selector != "$random" || panel.Seats[2].Optional {
		t.Fatalf("roundtable altered opaque delegate seat specifications: %+v", panel.Seats)
	}
}

func TestOptionalSeatsDoNotRaiseDefaultQuorum(t *testing.T) {
	dir := t.TempDir()
	writePreset(t, dir, preset{Name: "optional", Seats: []presetSeat{
		{Selector: "sol", Persona: "reviewer"},
		{Selector: "antigravity", Persona: "reviewer"},
		{Selector: "fable", Persona: "architect", Optional: true},
	}})
	store, _ := NewStore(dir)
	resolved, err := store.Resolve("optional", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.MinSuccessful != 2 || !resolved.Seats[2].Optional {
		t.Fatalf("optional seat changed quorum: %+v", resolved)
	}
}

func TestMinimumCannotExceedRequiredSeats(t *testing.T) {
	dir := t.TempDir()
	writePreset(t, dir, preset{Name: "invalid", MinSuccessful: 2, Seats: []presetSeat{
		{Selector: "sol"}, {Selector: "fable", Optional: true},
	}})
	store, _ := NewStore(dir)
	if _, err := store.Resolve("invalid", nil, nil); err == nil {
		t.Fatal("minimum above required seats was accepted")
	}
}

func TestOptionalAndChairmanOverlaysPreserveConfiguration(t *testing.T) {
	dir := t.TempDir()
	writePreset(t, dir, preset{Name: "overlay", Chairman: " antigravity ", ChairmanFallback: " sol ", ChairmanEnabled: true, Seats: []presetSeat{
		{Optional: true},
	}})
	store, _ := NewStore(dir)
	resolved, err := store.Resolve("overlay", []string{"reviewer"}, map[string]string{"reviewer": "fable"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Seats[0].Persona != "reviewer" || resolved.Seats[0].Selector != "fable" || !resolved.Seats[0].Optional || resolved.Chairman != "antigravity" || resolved.ChairmanFallback != "sol" {
		t.Fatalf("overlay lost optional or chairman configuration: %+v", resolved)
	}
}

func TestEnabledChairmanRequiresSpecification(t *testing.T) {
	for _, tc := range []struct {
		name             string
		chairman         string
		chairmanFallback string
	}{
		{name: "missing-primary", chairmanFallback: "sol"},
		{name: "missing-fallback", chairman: "antigravity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePreset(t, dir, preset{Name: tc.name, Chairman: tc.chairman, ChairmanFallback: tc.chairmanFallback, ChairmanEnabled: true, Seats: []presetSeat{{Selector: "$random"}}})
			store, _ := NewStore(dir)
			if _, err := store.Resolve(tc.name, nil, nil); err == nil {
				t.Fatal("enabled chairman silently accepted without both delegate specifications")
			}
		})
	}
}

func TestDisabledChairmanDoesNotValidateSelectors(t *testing.T) {
	dir := t.TempDir()
	writePreset(t, dir, preset{Name: "disabled", Chairman: "   ", ChairmanFallback: "   ", Seats: []presetSeat{{Selector: "$random"}}})
	store, _ := NewStore(dir)
	if _, err := store.Resolve("disabled", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredRoundtableAlwaysRequestsEverySeat(t *testing.T) {
	dir := t.TempDir()
	writePreset(t, dir, preset{Name: "five", Seats: []presetSeat{
		{Selector: "$random"}, {Selector: "$random"}, {Selector: "$random"}, {Selector: "$random"}, {Selector: "$random"},
	}})
	store, _ := NewStore(dir)
	panel, err := store.Resolve("five", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(panel.Seats) != 5 {
		t.Fatalf("configured capacity was reduced: %+v", panel.Seats)
	}
}

// An unnamed roundtable used to fall back to an implicit two-seat panel that no
// operator had configured — unanimous, chairman-less, and invisible in the
// result. Convening review authority nobody specified must be an error.
func TestUnnamedRoundtableFailsClosedInsteadOfImprovisingAPanel(t *testing.T) {
	dir := t.TempDir()
	writePreset(t, dir, preset{Name: "default", Seats: []presetSeat{{Selector: "$random"}}})
	store, _ := NewStore(dir)
	for _, requested := range []string{"", "   "} {
		if _, err := store.Resolve(requested, []string{"security", "qa", "architect"}, nil); err == nil {
			t.Fatalf("unnamed roundtable %q improvised a panel instead of failing closed", requested)
		}
	}
}

func TestConfiguredRoundtableDefaultsToUnbounded(t *testing.T) {
	dir := t.TempDir()
	writePreset(t, dir, preset{Name: "default", Seats: []presetSeat{{Selector: "$random"}}})
	store, _ := NewStore(dir)
	panel, err := store.Resolve("default", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if panel.DeadlineMS != 0 {
		t.Fatalf("deadline_ms=%d, want unbounded", panel.DeadlineMS)
	}
}

func TestNamedButAbsentRoundtableFailsClosed(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	if _, err := store.Resolve("missing", nil, nil); err == nil {
		t.Fatal("named roundtable that does not exist silently fell back")
	}
}

func TestShippedRoundtablePresetsExactAllocation(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "config", "roundtables")
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// plan: the known-good live allocation uses Luna for both required seats.
	plan, err := store.Resolve("plan", nil, nil)
	if err != nil {
		t.Fatalf("plan preset: %v", err)
	}
	if plan.Name != "plan" || len(plan.Seats) != 2 || !plan.Acquired || !plan.ChairmanEnabled || plan.MinSuccessful != 2 || plan.DeadlineMS != 0 || plan.Discussion {
		t.Fatalf("plan panel mismatch: %+v", plan)
	}
	if plan.Chairman != "luna" || plan.ChairmanFallback != "luna" {
		t.Fatalf("plan chairman mismatch: chairman=%q fallback=%q", plan.Chairman, plan.ChairmanFallback)
	}
	if plan.Seats[0].Selector != "luna" || plan.Seats[0].Persona != "reviewer" || plan.Seats[0].Optional {
		t.Fatalf("plan seat 0 mismatch: %+v", plan.Seats[0])
	}
	if plan.Seats[1].Selector != "luna" || plan.Seats[1].Persona != "qa" || plan.Seats[1].Optional {
		t.Fatalf("plan seat 1 mismatch: %+v", plan.Seats[1])
	}
	// No Fable seat in plan.
	for _, seat := range plan.Seats {
		if seat.Selector == "claude:claude-fable-5" {
			t.Fatalf("plan must not contain a Fable seat: %+v", plan.Seats)
		}
	}

	// implementation/documentation: the known-good live allocation uses Antigravity.
	implementation, err := store.Resolve("implementation", nil, nil)
	if err != nil {
		t.Fatalf("implementation preset: %v", err)
	}
	if implementation.Name != "implementation" || len(implementation.Seats) != 3 || !implementation.Acquired || !implementation.ChairmanEnabled || implementation.MinSuccessful != 2 || implementation.DeadlineMS != 0 || implementation.Discussion {
		t.Fatalf("implementation panel mismatch: %+v", implementation)
	}
	if implementation.Chairman != "antigravity" || implementation.ChairmanFallback != "antigravity" {
		t.Fatalf("implementation chairman mismatch: chairman=%q fallback=%q", implementation.Chairman, implementation.ChairmanFallback)
	}
	if implementation.Seats[0].Selector != "antigravity" || implementation.Seats[0].Persona != "reviewer" || implementation.Seats[0].Optional {
		t.Fatalf("implementation seat 0 mismatch: %+v", implementation.Seats[0])
	}
	if implementation.Seats[1].Selector != "antigravity" || implementation.Seats[1].Persona != "qa" || implementation.Seats[1].Optional {
		t.Fatalf("implementation seat 1 mismatch: %+v", implementation.Seats[1])
	}
	if implementation.Seats[2].Selector != "antigravity" || implementation.Seats[2].Persona != "architect" || !implementation.Seats[2].Optional {
		t.Fatalf("implementation seat 2 mismatch: %+v", implementation.Seats[2])
	}

	// documentation: identical allocation to implementation but name documentation.
	documentation, err := store.Resolve("documentation", nil, nil)
	if err != nil {
		t.Fatalf("documentation preset: %v", err)
	}
	if documentation.Name != "documentation" || len(documentation.Seats) != 3 || !documentation.Acquired || !documentation.ChairmanEnabled || documentation.MinSuccessful != 2 || documentation.DeadlineMS != 0 || documentation.Discussion {
		t.Fatalf("documentation panel mismatch: %+v", documentation)
	}
	if documentation.Chairman != "antigravity" || documentation.ChairmanFallback != "antigravity" {
		t.Fatalf("documentation chairman mismatch: chairman=%q fallback=%q", documentation.Chairman, documentation.ChairmanFallback)
	}
	if documentation.Seats[0].Selector != "antigravity" || documentation.Seats[0].Persona != "reviewer" || documentation.Seats[0].Optional {
		t.Fatalf("documentation seat 0 mismatch: %+v", documentation.Seats[0])
	}
	if documentation.Seats[1].Selector != "antigravity" || documentation.Seats[1].Persona != "qa" || documentation.Seats[1].Optional {
		t.Fatalf("documentation seat 1 mismatch: %+v", documentation.Seats[1])
	}
	if documentation.Seats[2].Selector != "antigravity" || documentation.Seats[2].Persona != "architect" || !documentation.Seats[2].Optional {
		t.Fatalf("documentation seat 2 mismatch: %+v", documentation.Seats[2])
	}

	// default must remain unchanged for other workflows.
	def, err := store.Resolve("default", nil, nil)
	if err != nil {
		t.Fatalf("default preset: %v", err)
	}
	if def.Name != "default" || len(def.Seats) != 3 || def.MinSuccessful != 2 || def.DeadlineMS != 0 || def.Discussion {
		t.Fatalf("default panel mismatch: %+v", def)
	}
	if def.Chairman != "antigravity" || def.ChairmanFallback != "codex:gpt-5.6-sol" || !def.ChairmanEnabled {
		t.Fatalf("default chairman mismatch: %+v", def)
	}
	if def.Seats[0].Selector != "codex:gpt-5.6-sol" || def.Seats[1].Selector != "antigravity" || def.Seats[2].Selector != "claude:claude-fable-5" || !def.Seats[2].Optional {
		t.Fatalf("default seats mismatch: %+v", def.Seats)
	}
}
