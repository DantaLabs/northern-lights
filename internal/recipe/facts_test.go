package recipe

import (
	"reflect"
	"strings"
	"testing"
)

func factsTestCompute(t *testing.T, in factInput) Fact {
	t.Helper()
	fact, err := computeFact(in)
	if err != nil {
		t.Fatalf("computeFact(%+v): %v", in, err)
	}
	return fact
}

func factsTestKWh() factInput {
	return factInput{Key: "elec", Label: "Electricity", Unit: "kWh", Decimals: 2,
		Current: "1240000", Prior: "1350000", MaterialityPct: "10"}
}

func TestScenario2FactArithmetic(t *testing.T) {
	fact := factsTestCompute(t, factsTestKWh())
	if fact.Change != "-110000" {
		t.Errorf("Change = %q, want -110000", fact.Change)
	}
	if !strings.HasPrefix(fact.ChangePct, "-8.148148") {
		t.Errorf("ChangePct = %q, want prefix -8.148148", fact.ChangePct)
	}
	if fact.Direction != "down" {
		t.Errorf("Direction = %q, want down", fact.Direction)
	}
	if fact.Material {
		t.Errorf("Material = true, want false (8.1%% < 10%%)")
	}
	if fact.Current != "1240000" || fact.Prior != "1350000" || fact.Unit != "kWh" {
		t.Errorf("values stay in stored unit, got %q %q %q", fact.Current, fact.Prior, fact.Unit)
	}
	canon := factsTestCompute(t, factInput{Key: "k", Unit: "t", Current: "585.0", Prior: "500.50", MaterialityPct: "10"})
	if canon.Current != "585" || canon.Prior != "500.5" {
		t.Errorf("canonical form = %q %q", canon.Current, canon.Prior)
	}
	material := factsTestKWh()
	material.MaterialityPct = "8"
	if !factsTestCompute(t, material).Material {
		t.Errorf("Material = false, want true at 8%% threshold")
	}
}

func TestScenario3NewDirectionWhenPriorZeroOrMissing(t *testing.T) {
	for _, prior := range []string{"", "0"} {
		in := factsTestKWh()
		in.Prior = prior
		fact := factsTestCompute(t, in)
		if fact.Direction != "new" || fact.Change != "" || fact.ChangePct != "" || fact.Material {
			t.Errorf("prior %q: got %+v", prior, fact)
		}
		for _, form := range fact.Display {
			if strings.Contains(form, "%") {
				t.Errorf("prior %q: display %q has a percent form", prior, form)
			}
		}
	}
	missing := factsTestKWh()
	missing.Prior = ""
	if got := factsTestCompute(t, missing).Display; len(got) != 1 || got[0] != "1,240,000.00 kWh" {
		t.Errorf("missing prior display = %q", got)
	}
}

func TestScenario4DisplayForms(t *testing.T) {
	gwh := factsTestKWh()
	gwh.DisplayUnit = "GWh"
	cases := []struct {
		name string
		in   factInput
		dir  string
		want []string
	}{
		{"negative", gwh, "down", []string{"1.24 GWh", "1.35 GWh", "-0.11 GWh", "-8%", "-8.1%", "0.11 GWh", "8%", "8.1%"}},
		{"positive", factInput{Key: "co2", Unit: "tCO2e", Decimals: 1, Current: "165", Prior: "140", MaterialityPct: "10"},
			"up", []string{"165.0 tCO2e", "140.0 tCO2e", "25.0 tCO2e", "18%", "17.9%"}},
		{"flat", factInput{Key: "w", Unit: "m3", Decimals: 0, Current: "100", Prior: "100", MaterialityPct: "10"},
			"flat", []string{"100 m3", "100 m3", "0 m3", "0%", "0.0%"}},
		{"number unit has no suffix", factInput{Key: "n", Unit: "number", Decimals: 0, Current: "1200", Prior: "1000", MaterialityPct: "10"},
			"up", []string{"1,200", "1,000", "200", "20%", "20.0%"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fact := factsTestCompute(t, tc.in)
			if fact.Direction != tc.dir {
				t.Errorf("Direction = %q, want %q", fact.Direction, tc.dir)
			}
			if !reflect.DeepEqual(fact.Display, tc.want) {
				t.Errorf("Display = %q, want %q", fact.Display, tc.want)
			}
		})
	}
}

func TestPercentFieldsUsePercentAndPercentagePoints(t *testing.T) {
	in := factInput{Key: "turnover", Unit: "%", Decimals: 0, Current: "0.46", Prior: "0.38", MaterialityPct: "10"}
	fact := factsTestCompute(t, in)
	if fact.Current != "46" || fact.Prior != "38" || fact.Change != "8" {
		t.Errorf("got %q %q %q, want 46 38 8", fact.Current, fact.Prior, fact.Change)
	}
	want := []string{"46%", "38%", "8%", "8 percentage points", "21%", "21.1%"}
	if !reflect.DeepEqual(fact.Display, want) {
		t.Errorf("Display = %q, want %q", fact.Display, want)
	}
	if fact.Material {
		t.Errorf("Material = true, want false (8 pp < 10)")
	}
	in.Current = "0.50"
	if !factsTestCompute(t, in).Material {
		t.Errorf("Material = false, want true (12 pp)")
	}
	down := in
	down.Current, down.Prior = "0.37", "0.38"
	got := factsTestCompute(t, down).Display
	wantDown := []string{"37%", "38%", "-1%", "-1 percentage point", "-3%", "-2.6%", "1%", "1 percentage point", "3%", "2.6%"}
	if !reflect.DeepEqual(got, wantDown) {
		t.Errorf("down Display = %q, want %q", got, wantDown)
	}
}

func TestDisplayRoundingHalfAwayFromZero(t *testing.T) {
	rounding := []struct {
		value  string
		places int
		want   string
	}{{"2.5", 0, "3"}, {"-2.5", 0, "-3"}, {"0.125", 2, "0.13"}, {"-0.004", 2, "0.00"}, {"165", 1, "165.0"}}
	for _, tc := range rounding {
		got, err := roundHalfAway(tc.value, tc.places)
		if err != nil || got != tc.want {
			t.Errorf("roundHalfAway(%q,%d) = %q, %v; want %q", tc.value, tc.places, got, err, tc.want)
		}
	}
	for in, want := range map[string]string{"18400": "18,400", "-4050.0": "-4,050.0", "-1240000": "-1,240,000", "18400.5": "18,400.5", "12": "12"} {
		if got := groupThousands(in); got != want {
			t.Errorf("groupThousands(%q) = %q, want %q", in, got, want)
		}
	}
	fact := factsTestCompute(t, factInput{Key: "w", Unit: "m3", Decimals: 0, Current: "18100", MaterialityPct: "10"})
	if len(fact.Display) != 1 || fact.Display[0] != "18,100 m3" {
		t.Errorf("Display = %q", fact.Display)
	}
}

func TestUnitShiftFamilies(t *testing.T) {
	ok := []struct {
		from, to string
		want     int
	}{{"kWh", "GWh", -2}, {"Wh", "MWh", -2}, {"GWh", "kWh", 2}, {"t", "kt", -1}, {"kt", "t", 1}, {"kWh", "", 0}, {"m3", "m3", 0}}
	for _, tc := range ok {
		if got, err := unitShift(tc.from, tc.to); err != nil || got != tc.want {
			t.Errorf("unitShift(%q,%q) = %d, %v; want %d", tc.from, tc.to, got, err, tc.want)
		}
	}
	if _, err := unitShift("kWh", "t"); err == nil {
		t.Errorf("unitShift(kWh,t) should fail")
	}
}

func TestComputeFactRejectsBadInput(t *testing.T) {
	bad := []factInput{
		{Key: "k1", Unit: "kWh", Current: "abc", MaterialityPct: "10"},
		{Key: "k2", Unit: "kWh", Current: "1", Decimals: 7, MaterialityPct: "10"},
		{Key: "k3", Unit: "kWh", Current: "1", Decimals: -1, MaterialityPct: "10"},
		{Key: "k4", Unit: "kWh", DisplayUnit: "t", Current: "1", MaterialityPct: "10"},
		{Key: "k5", Unit: "kWh", Current: "1", Prior: "x", MaterialityPct: "10"},
	}
	for _, in := range bad {
		_, err := computeFact(in)
		if err == nil || !strings.Contains(err.Error(), in.Key) {
			t.Errorf("computeFact(%s) error = %v, want error naming key", in.Key, err)
		}
	}
}

func TestMaterialityEqualityBoundaryIsMaterial(t *testing.T) {
	relative := factInput{Key: "k", Unit: "kWh", Current: "110", Prior: "100", MaterialityPct: "10"}
	if !factsTestCompute(t, relative).Material {
		t.Errorf("relative 110 vs 100 at threshold 10 (exactly 10%%): Material = false, want true")
	}
	percent := factInput{Key: "k", Unit: "%", Current: "0.48", Prior: "0.38", MaterialityPct: "10"}
	if !factsTestCompute(t, percent).Material {
		t.Errorf("percent field 0.48 vs 0.38 at threshold 10 (exactly 10 pp): Material = false, want true")
	}
}

func TestDisplayFormsAreNotDuplicated(t *testing.T) {
	// Change (-10%) and percent change (-10%) coincide: each form appears once.
	in := factInput{Key: "k", Unit: "%", Decimals: 0, Current: "0.90", Prior: "1.00", MaterialityPct: "10"}
	got := factsTestCompute(t, in).Display
	want := []string{"90%", "100%", "-10%", "-10 percentage points", "-10%", "-10.0%", "10%", "10 percentage points", "10.0%"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Display = %q, want %q", got, want)
	}
}
