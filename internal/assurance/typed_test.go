package assurance

import (
	"encoding/json"
	"testing"
)

func TestC012TypedValuePreservesBlankZeroFormulaErrorUnitAndScale(t *testing.T) {
	tests := []struct {
		name  string
		in    ProviderValue
		kind  ValueKind
		unit  string
		scale string
		want  TypedValue
	}{
		{name: "blank", in: ProviderValue{}, kind: ValueBlank, want: TypedValue{Kind: ValueBlank}},
		{name: "zero", in: ProviderValue{Value: json.Number("0.000")}, kind: ValueNumber, want: TypedValue{Kind: ValueNumber, Number: "0"}},
		{name: "formula", in: ProviderValue{Formula: "=SUM(A1:A2)", CalculatedValue: json.Number("12.50")}, kind: ValueNumber, unit: "kWh", scale: "ones", want: TypedValue{Kind: ValueNumber, Number: "12.5", Unit: "kWh", Scale: "ones", Formula: true, FormulaText: "=SUM(A1:A2)", Calculated: true, CalculatedSource: "provider"}},
		{name: "provider error", in: ProviderValue{ErrorCode: "REF"}, kind: ValueError, want: TypedValue{Kind: ValueError, ErrorCode: "REF"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeProviderValue(tc.in, FieldDefinition{Kind: tc.kind, Unit: tc.unit, Scale: tc.scale})
			if err != nil {
				t.Fatalf("NormalizeProviderValue: %v", err)
			}
			if got != tc.want {
				t.Fatalf("typed value = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestC013LocaleDateCurrencyAndPercentAreDeterministic(t *testing.T) {
	for _, value := range []string{"1,234.50", "1.234,50", "01/02/2026"} {
		if _, err := NormalizeProviderValue(ProviderValue{Value: value}, FieldDefinition{Kind: ValueNumber}); err == nil {
			t.Fatalf("locale-dependent number %q accepted", value)
		}
	}
	if _, err := NormalizeProviderValue(ProviderValue{Value: "45292"}, FieldDefinition{Kind: ValueDate}); err == nil {
		t.Fatal("undeclared serial date epoch accepted")
	}
	if got, err := NormalizeProviderValue(ProviderValue{Value: "2026-02-28"}, FieldDefinition{Kind: ValueDate}); err != nil || got.Date != "2026-02-28" {
		t.Fatalf("ISO date = %#v err=%v", got, err)
	}
	if _, err := NormalizeProviderValue(ProviderValue{Value: "2026-02-30"}, FieldDefinition{Kind: ValueDate}); err == nil {
		t.Fatal("invalid calendar date accepted")
	}
	if _, err := NormalizeProviderValue(ProviderValue{Value: "12.5"}, FieldDefinition{Kind: ValueCurrency}); err == nil {
		t.Fatal("currency without ISO unit accepted")
	}
	currency, err := NormalizeProviderValue(ProviderValue{Value: "0012.500"}, FieldDefinition{Kind: ValueCurrency, Unit: "EUR", Scale: "ones"})
	if err != nil || currency.Number != "12.5" || currency.Unit != "EUR" {
		t.Fatalf("currency = %#v err=%v", currency, err)
	}
	percent, err := NormalizeProviderValue(ProviderValue{Value: "0.1250"}, FieldDefinition{Kind: ValuePercent, PercentBasis: "0..1"})
	if err != nil || percent.Number != "0.125" || percent.PercentBasis != "0..1" {
		t.Fatalf("percent = %#v err=%v", percent, err)
	}
}

func TestC010CanonicalJSONSortsKeysAndNormalizesTypedDecimals(t *testing.T) {
	got, err := CanonicalJSON(map[string]any{"z": []any{map[string]any{"b": 2, "a": 1}}, "a": "x"})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"a":"x","z":[{"a":1,"b":2}]}`
	if string(got) != want {
		t.Fatalf("canonical JSON = %s, want %s", got, want)
	}
	if got := CanonicalDecimal("-000.1200"); got != "-0.12" {
		t.Fatalf("CanonicalDecimal = %q, want -0.12", got)
	}
}
