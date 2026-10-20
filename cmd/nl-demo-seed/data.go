package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dantalabs/northern-lights/internal/bootstrap"
)

// Row is one data row of an ESG CSV file.
type Row struct{ Key, Label, Unit, Value string }

var (
	csvHeader    = []string{"key", "label", "unit", "value"}
	decimalRegex = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
)

// LoadCSV reads an ESG CSV file (header key,label,unit,value). A value is
// empty (missing) or a plain decimal; duplicate keys are rejected. Errors
// name the file and line.
func LoadCSV(path string) ([]Row, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = len(csvHeader)
	var rows []Row
	seen := map[string]int{}
	for first := true; ; first = false {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		line, _ := r.FieldPos(0)
		if first {
			if strings.Join(rec, ",") != strings.Join(csvHeader, ",") {
				return nil, fmt.Errorf("%s:%d: header must be %q, got %q", path, line, strings.Join(csvHeader, ","), strings.Join(rec, ","))
			}
			continue
		}
		row := Row{Key: rec[0], Label: rec[1], Unit: rec[2], Value: rec[3]}
		if row.Key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, line)
		}
		if row.Value != "" && !decimalRegex.MatchString(row.Value) {
			return nil, fmt.Errorf("%s:%d: value %q of %s is not a plain decimal", path, line, row.Value, row.Key)
		}
		if prev, dup := seen[row.Key]; dup {
			return nil, fmt.Errorf("%s:%d: duplicate key %s (first on line %d)", path, line, row.Key, prev)
		}
		seen[row.Key] = line
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: no data rows", path)
	}
	return rows, nil
}

// Recipe mirrors the section recipe schema.
type Recipe struct {
	ID             string        `yaml:"id"`
	Title          string        `yaml:"title"`
	Period         string        `yaml:"period"`
	PriorPeriod    string        `yaml:"prior_period"`
	DocumentID     string        `yaml:"document_id"`
	MaterialityPct int           `yaml:"materiality_pct"`
	Fields         []RecipeField `yaml:"fields"`
	Verbatim       []VerbatimRef `yaml:"verbatim"`
}

// RecipeField is one field of a section recipe. Min and Max stay strings so
// decimals are compared exactly.
type RecipeField struct {
	Key         string  `yaml:"key"`
	Label       string  `yaml:"label"`
	Unit        string  `yaml:"unit"`
	DisplayUnit string  `yaml:"display_unit"`
	Decimals    int     `yaml:"decimals"`
	Source      string  `yaml:"source"`
	Target      string  `yaml:"target"`
	Prior       string  `yaml:"prior"`
	Required    bool    `yaml:"required"`
	Min         *string `yaml:"min"`
	Max         *string `yaml:"max"`
}

// VerbatimRef names a verbatim block file, relative to the recipe directory.
type VerbatimRef struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
	File  string `yaml:"file"`
}

// LoadRecipes reads every *.yaml section recipe in dir, rejecting unknown
// keys, and returns the recipes sorted by ID.
func LoadRecipes(dir string) ([]Recipe, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var out []Recipe
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		var rc Recipe
		if err := dec.Decode(&rc); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		for name, v := range map[string]string{"id": rc.ID, "title": rc.Title, "period": rc.Period, "prior_period": rc.PriorPeriod, "document_id": rc.DocumentID} {
			if v == "" {
				return nil, fmt.Errorf("%s: %s is required", p, name)
			}
		}
		if len(rc.Fields) == 0 {
			return nil, fmt.Errorf("%s: at least one field is required", p)
		}
		out = append(out, rc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Mappings is a mappings file plus the report document ID that the server's
// loader ignores.
type Mappings struct {
	bootstrap.File `yaml:",inline"`
	DocumentID     string `yaml:"document_id"`
}

// FieldRef is a mapped field with the spreadsheet and sheet that hold it.
type FieldRef struct{ Name, FieldType, SpreadsheetID, SpreadsheetName, SheetID, SheetName, Range string }

// LoadMappings reads a mappings file.
func LoadMappings(path string) (Mappings, error) {
	var m Mappings
	data, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Field looks a mapped field up by name.
func (m Mappings) Field(name string) (FieldRef, bool) {
	for _, sp := range m.Spreadsheets {
		for _, sh := range sp.Sheets {
			for _, f := range sh.Fields {
				if f.Name == name {
					return FieldRef{Name: f.Name, FieldType: f.FieldType, SpreadsheetID: sp.ID, SpreadsheetName: sp.Name,
						SheetID: sh.ID, SheetName: sh.Name, Range: f.Range}, true
				}
			}
		}
	}
	return FieldRef{}, false
}

// DecimalText returns the exact decimal text of a cell value. nil and ""
// are an empty cell ("", true); text that is not a number is not ok.
func DecimalText(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", true
	case json.Number:
		return decimalOrEmpty(x.String())
	case string:
		return decimalOrEmpty(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	default:
		return "", false
	}
}

func decimalOrEmpty(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	if _, ok := new(big.Rat).SetString(s); !ok {
		return "", false
	}
	return s, true
}
