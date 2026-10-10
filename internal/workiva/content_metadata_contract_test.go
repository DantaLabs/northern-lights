package workiva

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestContentMetadataOfficialSheetDataContract(t *testing.T) {
	var calls int
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/spreadsheets/sp-1/sheets/sh-1/sheetdata" {
			t.Errorf("request = %s %s; expected official sheetdata GET", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Version"); got != "2026-01-01" {
			t.Errorf("X-Version = %q", got)
		}
		if calls == 1 {
			if got := r.URL.Query().Get("$cellrange"); got != "B3" {
				t.Errorf("$cellrange = %q", got)
			}
			if got := r.URL.Query().Get("$fields"); got != "cells.value,cells.calculatedValue,cells.formats,cells.effectiveFormats,range" {
				t.Errorf("$fields = %q", got)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = fmt.Fprintf(w, `{"data":{"range":{"startRow":2,"startColumn":1,"stopRow":2,"stopColumn":1},"cells":[[{"value":"=1+1","calculatedValue":2,"formats":{"valueFormat":{"precision":1.2300},"textFormat":{"bold":false}},"effectiveFormats":{"textFormat":{"bold":true}},"extension":{"kept":true}}]]},"@nextLink":%q}`, "http://"+r.Host+"/spreadsheets/sp-1/sheets/sh-1/sheetdata?$page=2")
			return
		}
		_, _ = fmt.Fprint(w, `{"data":{"range":{"startRow":3,"startColumn":1,"stopRow":3,"stopColumn":1},"cells":[[{"calculatedValue":null,"formats":null,"effectiveFormats":{}}]]}}`)
	}))
	data, err := c.GetSheetDataTyped(context.Background(), "sp-1", "sh-1", "B3", []string{"cells.value", "cells.calculatedValue", "cells.formats", "cells.effectiveFormats"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(data.Cells) != 2 {
		t.Fatalf("calls=%d rows=%d; want two paginated rows", calls, len(data.Cells))
	}
	formula := data.Cells[0][0]
	if !formula.ValuePresent || formula.Value != "=1+1" || !formula.CalculatedValuePresent || fmt.Sprint(formula.CalculatedValue) != "2" {
		t.Fatalf("formula cell = %+v", formula)
	}
	if !formula.FormatsPresent || !formula.EffectiveFormatsPresent || string(formula.Formats) != `{"valueFormat":{"precision":1.2300},"textFormat":{"bold":false}}` {
		t.Fatalf("formula formats = %+v", formula)
	}
	empty := data.Cells[1][0]
	if empty.ValuePresent || !empty.CalculatedValuePresent || empty.CalculatedValue != nil || !empty.FormatsPresent || string(empty.Formats) != "null" || !empty.EffectiveFormatsPresent || string(empty.EffectiveFormats) != "{}" {
		t.Fatalf("missing/null/type distinctions lost: %+v", empty)
	}
}

func TestContentMetadataPreservesExplicitNullAndMissingFormatFields(t *testing.T) {
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{"range":{"startRow":0,"startColumn":0,"stopRow":0,"stopColumn":0},"cells":[[{"value":null,"calculatedValue":false}]]}}`)
	}))
	data, err := c.GetSheetDataTyped(context.Background(), "sp", "sh", "A1", []string{"cells.value", "cells.calculatedValue", "cells.formats", "cells.effectiveFormats"})
	if err != nil {
		t.Fatal(err)
	}
	cell := data.Cells[0][0]
	if !cell.ValuePresent || cell.Value != nil || !cell.CalculatedValuePresent || cell.CalculatedValue != false {
		t.Fatalf("null and boolean fields = %+v", cell)
	}
	if cell.FormatsPresent || cell.EffectiveFormatsPresent {
		t.Fatalf("omitted formats marked present: %+v", cell)
	}
}
