package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	required := []Seat{
		{Selector: "sol", Persona: "reviewer"},
		{Selector: "sol", Persona: "qa"},
		{Selector: "opus-ui", Persona: "reviewer"},
		{Selector: "opus-ui", Persona: "qa"},
		{Selector: "antigravity", Persona: "reviewer"},
		{Selector: "antigravity", Persona: "qa"},
	}
	assertPanel := func(name string, want []Seat) Panel {
		t.Helper()
		got, resolveErr := store.Resolve(name, nil, nil)
		if resolveErr != nil {
			t.Fatalf("%s preset: %v", name, resolveErr)
		}
		if got.Name != name || !got.Acquired || !got.ChairmanEnabled || got.MinSuccessful != 4 || got.DeadlineMS != 0 || !got.Discussion || got.Chairman != "fable" || got.ChairmanFallback != "sol" || !reflect.DeepEqual(got.Seats, want) {
			t.Fatalf("%s panel mismatch: %+v", name, got)
		}
		return got
	}

	assertPanel("plan", required)
	implementation := append(append([]Seat(nil), required...),
		Seat{Selector: "sol", Persona: "architect", Optional: true},
		Seat{Selector: "opus-ui", Persona: "architect", Optional: true},
		Seat{Selector: "antigravity", Persona: "architect", Optional: true})
	assertPanel("implementation", implementation)
	assertPanel("documentation", required)

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
