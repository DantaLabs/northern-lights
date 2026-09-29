package workiva

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"testing"
)

func TestWave1OfficialShapedTypedAndFormulaFixturesPreserveJSONNumbers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		rangeID string
		want    []any
	}{
		{name: "typed scalars", fixture: "testdata/phase3/sheetdata_typed_scalars.json", rangeID: "A1:F1", want: []any{nil, json.Number("0"), json.Number("123.4500"), true, "2026-09-30", "EUR"}},
		{name: "formula", fixture: "testdata/phase3/sheetdata_formula_calculated.json", rangeID: "B4", want: []any{"=SUM(B1:B3)", json.Number("12.50")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			client, _ := setupFastClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/iam/v1/oauth2/token" {
					_, _ = fmt.Fprint(w, `{"access_token":"token","expires_in":3600}`)
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/spreadsheets/sp-1/sheets/sh-1/sheetdata" || r.URL.Query().Get("$cellrange") != tc.rangeID {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(fixture)
			}))
			data, err := client.GetSheetDataTyped(t.Context(), "sp-1", "sh-1", tc.rangeID, []string{"cells.value", "cells.calculatedValue"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "formula" {
				got := []any{data.Cells[0][0].Value, data.Cells[0][0].CalculatedValue}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("formula values = %#v, want %#v", got, tc.want)
				}
				return
			}
			var got []any
			for _, cell := range data.Cells[0] {
				got = append(got, cell.Value)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("typed values = %#v, want %#v", got, tc.want)
			}
		})
	}
}
