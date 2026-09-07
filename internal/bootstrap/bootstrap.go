// Package bootstrap seeds the mapping store from a declarative YAML
// file so teams can version-control their spreadsheet/sheet/field
// mappings instead of (or before) discovering them via sync_mapping.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dantalabs/northern-lights/internal/mapping"
)

// File is the root of the declarative mappings YAML.
type File struct {
	Region       string             `yaml:"region"`
	Spreadsheets []SpreadsheetEntry `yaml:"spreadsheets"`
}

// SpreadsheetEntry declares one spreadsheet and its sheets.
type SpreadsheetEntry struct {
	ID     string       `yaml:"id"`
	Name   string       `yaml:"name"`
	Sheets []SheetEntry `yaml:"sheets"`
}

// SheetEntry declares one sheet and its semantic fields.
type SheetEntry struct {
	ID     string       `yaml:"id"`
	Name   string       `yaml:"name"`
	Fields []FieldEntry `yaml:"fields"`
}

// FieldEntry binds a semantic name to an A1 range.
type FieldEntry struct {
	Name        string `yaml:"name"`
	Range       string `yaml:"range"`
	Aliases     string `yaml:"aliases"`
	FieldType   string `yaml:"field_type"`
	Description string `yaml:"description"`
}

// DemoMappings returns the static mapping seeded when NL_DEMO_MODE is
// enabled. It mirrors the VSME energy-report fixture so every demo field
// resolves to a cell in the synthetic sheetdata response.
func DemoMappings() File {
	return File{
		Region: "eu",
		Spreadsheets: []SpreadsheetEntry{
			{
				ID:   "demo-sp-energy-2026",
				Name: "VSME Energy Report 2026",
				Sheets: []SheetEntry{
					{
						ID:   "demo-sh-b3-energy",
						Name: "B3 Energy",
						Fields: []FieldEntry{
							{Name: "scope1_stationary_combustion_kwh", Range: "B2", Aliases: "scope 1,direct emissions", FieldType: "number", Description: "Scope 1 stationary combustion energy consumption"},
							{Name: "scope2_purchased_electricity_kwh", Range: "B3", Aliases: "scope 2,purchased electricity", FieldType: "number", Description: "Scope 2 purchased electricity energy consumption"},
							{Name: "total_energy_consumption_kwh", Range: "B4", Aliases: "total energy,energy consumption", FieldType: "number", Description: "Total energy consumption across scopes 1 and 2"},
							{Name: "renewable_energy_share_pct", Range: "B5", Aliases: "renewable share,renewable percentage", FieldType: "percent", Description: "Share of renewable energy in total consumption"},
							{Name: "energy_intensity_kwh_fte", Range: "B6", Aliases: "energy intensity,intensity", FieldType: "number", Description: "Energy intensity per full-time equivalent"},
						},
					},
					{
						ID:   "demo-sh-reference",
						Name: "Reference data",
						Fields: []FieldEntry{
							{Name: "employee_count", Range: "B2", FieldType: "number", Description: "Number of full-time equivalents used for intensity calculation"},
							{Name: "electricity_rate_eur_kwh", Range: "B3", FieldType: "currency", Description: "Average electricity rate applied to energy cost estimates"},
						},
					},
				},
			},
			{
				ID:   "demo-sp-energy-2025",
				Name: "VSME Energy Report 2025",
				Sheets: []SheetEntry{
					{
						ID:   "demo-sh-b3-energy-2025",
						Name: "B3 Energy",
						Fields: []FieldEntry{
							{Name: "scope1_stationary_combustion_kwh_2025", Range: "B2", FieldType: "number", Description: "2025 Scope 1 stationary combustion energy consumption"},
							{Name: "scope2_purchased_electricity_kwh_2025", Range: "B3", FieldType: "number", Description: "2025 Scope 2 purchased electricity energy consumption"},
							{Name: "total_energy_consumption_kwh_2025", Range: "B4", FieldType: "number", Description: "2025 total energy consumption across scopes 1 and 2"},
						},
					},
				},
			},
		},
	}
}

// LoadMappings parses the YAML file at path and upserts every
// spreadsheet, sheet, and field into store. It is idempotent: re-loading
// the same file updates existing rows instead of duplicating them. A
// missing file is a no-op returning nil, so servers without a mappings
// file start cleanly. A spreadsheet entry without an explicit region
// inherits the file-level region, defaulting to eu.
func LoadMappings(ctx context.Context, path string, store *mapping.Store) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("bootstrap: read mappings file: %w", err)
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("bootstrap: parse mappings file %s: %w", path, err)
	}
	return applyFile(ctx, f, store)
}

// LoadDemoMappings seeds the mapping store with the static demo fixture
// metadata.
func LoadDemoMappings(ctx context.Context, store *mapping.Store) error {
	return applyFile(ctx, DemoMappings(), store)
}

// applyFile upserts the spreadsheets, sheets, and fields from f into
// store. It is shared by file-based and demo-based bootstrap.
func applyFile(ctx context.Context, f File, store *mapping.Store) error {
	region := f.Region
	if region == "" {
		region = "eu"
	}

	for _, sp := range f.Spreadsheets {
		if sp.ID == "" {
			return fmt.Errorf("bootstrap: spreadsheet entry without id")
		}
		if err := store.UpsertSpreadsheet(ctx, mapping.Spreadsheet{
			ID:       sp.ID,
			Name:     sp.Name,
			Region:   region,
			SyncedAt: time.Now(),
		}); err != nil {
			return err
		}
		for _, sh := range sp.Sheets {
			if sh.ID == "" {
				return fmt.Errorf("bootstrap: sheet entry without id in spreadsheet %q", sp.ID)
			}
			if err := store.UpsertSheet(ctx, mapping.Sheet{
				ID:            sh.ID,
				SpreadsheetID: sp.ID,
				Name:          sh.Name,
			}); err != nil {
				return err
			}
			for _, fe := range sh.Fields {
				if fe.Name == "" || fe.Range == "" {
					return fmt.Errorf("bootstrap: field in %s/%s needs name and range", sp.ID, sh.ID)
				}
				fieldType := fe.FieldType
				if fieldType == "" {
					fieldType = "text"
				}
				if _, err := store.UpsertField(ctx, mapping.Field{
					SpreadsheetID: sp.ID,
					SheetID:       sh.ID,
					Name:          fe.Name,
					Aliases:       fe.Aliases,
					CellRange:     fe.Range,
					FieldType:     fieldType,
					Description:   fe.Description,
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
