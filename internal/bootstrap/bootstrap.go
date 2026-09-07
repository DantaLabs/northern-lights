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

	region := f.Region
	if region == "" {
		region = "eu"
	}

	for _, sp := range f.Spreadsheets {
		if sp.ID == "" {
			return fmt.Errorf("bootstrap: mappings file %s: spreadsheet entry without id", path)
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
				return fmt.Errorf("bootstrap: mappings file %s: sheet entry without id in spreadsheet %q", path, sp.ID)
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
					return fmt.Errorf("bootstrap: mappings file %s: field in %s/%s needs name and range", path, sp.ID, sh.ID)
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
