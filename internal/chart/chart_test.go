package chart

import (
	"bytes"
	"image/png"
	"os"
	"testing"
)

func TestBarsDrawsAPNG(t *testing.T) {
	data, w, h, err := Bars("Environmental indicators", "2025 compared with 2024; bars scaled per indicator", "2025", "2024", []Row{
		{Label: "6. Business Travel", Current: 165, Prior: 140, CurrentText: "165.0 tCO2e", PriorText: "140.0 tCO2e", Tag: "+17.9%, material"},
		{Label: "Total Energy Consumption", Current: 1978, Prior: 2030, CurrentText: "1,978 MWh", PriorText: "2,030 MWh"},
		{Label: "Missing value", Current: 0, Prior: 2300, CurrentText: "missing", PriorText: "2,300 m3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil || w != 900 || img.Bounds().Dx() != w*Scale || img.Bounds().Dy() != h*Scale {
		t.Fatalf("w=%d h=%d err=%v", w, h, err)
	}
	if out := os.Getenv("NL_CHART_OUT"); out != "" {
		_ = os.WriteFile(out, data, 0o600)
	}
}
