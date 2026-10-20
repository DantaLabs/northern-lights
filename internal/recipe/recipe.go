package recipe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/mapping"
)

// recipeDef is one validated section recipe.
type recipeDef struct {
	ID, Title, Period, PriorPeriod, DocumentID string
	MaterialityPct                             string // decimal text
	Fields                                     []fieldDef
	Verbatim                                   []verbatimDef
	root                                       *os.Root // sections directory; verbatim files are read only through it
	file                                       string   // base file name, for messages
}

// fieldDef is one field of a section recipe.
type fieldDef struct {
	Key, Label, Unit, DisplayUnit string
	Decimals                      int
	Source, Target, Prior         string // Prior "" when absent
	Required                      bool
	// Min and Max are decimal text, "" when absent. For a "%" field the recipe
	// author writes them in percent (min: 50 means 50%); the value they are
	// compared against is percent-scaled too.
	Min, Max string
}

// verbatimDef names an approved text file, relative to the sections directory.
type verbatimDef struct{ ID, Title, File string }

// fieldLookup resolves mapped field names; *mapping.Store satisfies it.
type fieldLookup interface {
	GetField(ctx context.Context, name string) (*mapping.Field, error)
}

// decimalText captures a YAML scalar as its source text so numbers never pass
// through a binary floating-point type. It is the only type here with a custom
// unmarshaler, because yaml.v3 does not apply KnownFields inside those.
type decimalText string

// UnmarshalYAML keeps the scalar's literal text.
func (d *decimalText) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a decimal number", n.Line)
	}
	*d = decimalText(strings.TrimSpace(n.Value))
	return nil
}

type rawRecipe struct {
	ID             string        `yaml:"id"`
	Title          string        `yaml:"title"`
	Period         string        `yaml:"period"`
	PriorPeriod    string        `yaml:"prior_period"`
	DocumentID     string        `yaml:"document_id"`
	MaterialityPct decimalText   `yaml:"materiality_pct"`
	Fields         []rawField    `yaml:"fields"`
	Verbatim       []verbatimDef `yaml:"verbatim"`
}

type rawField struct {
	Key         string      `yaml:"key"`
	Label       string      `yaml:"label"`
	Unit        string      `yaml:"unit"`
	DisplayUnit string      `yaml:"display_unit"`
	Decimals    int         `yaml:"decimals"`
	Source      string      `yaml:"source"`
	Target      string      `yaml:"target"`
	Prior       string      `yaml:"prior"`
	Required    bool        `yaml:"required"`
	Min         decimalText `yaml:"min"`
	Max         decimalText `yaml:"max"`
}

// loadRecipes reads every *.yaml file (not recursive, not .yml) in dir, sorted by name.
// A missing directory loads zero recipes; a path that is not a directory is an error.
func loadRecipes(ctx context.Context, dir string, fields fieldLookup) ([]recipeDef, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("section recipes directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("section recipes directory %q is not a directory", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("section recipes directory: %w", err)
	}
	out, err := loadRecipesFromRoot(ctx, root, fields)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if len(out) == 0 {
		_ = root.Close()
	}
	return out, nil
}

// loadRecipesFromRoot loads every recipe file at the top level of root. The
// returned recipes keep root open for reading their verbatim files.
func loadRecipesFromRoot(ctx context.Context, root *os.Root, fields fieldLookup) ([]recipeDef, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("section recipes directory: %w", err)
	}
	var out []recipeDef
	seen := map[string]string{}
	for _, e := range entries { // ReadDir returns entries sorted by name
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		r, err := loadRecipeFile(ctx, root, e.Name(), fields)
		if err != nil {
			return nil, fmt.Errorf("section recipe %s: %w", e.Name(), err)
		}
		if first, dup := seen[r.ID]; dup {
			return nil, fmt.Errorf("section recipe %s: duplicate id %q also in %s", e.Name(), r.ID, first)
		}
		seen[r.ID] = e.Name()
		out = append(out, r)
	}
	return out, nil
}

func loadRecipeFile(ctx context.Context, root *os.Root, name string, fields fieldLookup) (recipeDef, error) {
	data, err := root.ReadFile(name)
	if err != nil {
		return recipeDef{}, err
	}
	var raw rawRecipe
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return recipeDef{}, errors.New("file is empty: id is required")
		}
		return recipeDef{}, err
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return recipeDef{}, errors.New("multiple YAML documents are not allowed")
	}
	r := recipeDef{
		ID: raw.ID, Title: raw.Title, Period: raw.Period, PriorPeriod: raw.PriorPeriod,
		DocumentID: raw.DocumentID, MaterialityPct: string(raw.MaterialityPct),
		Verbatim: raw.Verbatim, root: root, file: name,
	}
	for _, f := range raw.Fields {
		r.Fields = append(r.Fields, fieldDef{
			Key: f.Key, Label: f.Label, Unit: f.Unit, DisplayUnit: f.DisplayUnit, Decimals: f.Decimals,
			Source: f.Source, Target: f.Target, Prior: f.Prior, Required: f.Required,
			Min: string(f.Min), Max: string(f.Max),
		})
	}
	if err := r.validate(ctx, fields); err != nil {
		return recipeDef{}, err
	}
	return r, nil
}

func (r recipeDef) validate(ctx context.Context, fields fieldLookup) error {
	for _, req := range []struct{ key, val string }{
		{"id", r.ID}, {"title", r.Title}, {"period", r.Period},
		{"prior_period", r.PriorPeriod}, {"document_id", r.DocumentID},
		{"materiality_pct", r.MaterialityPct},
	} {
		if strings.TrimSpace(req.val) == "" {
			return fmt.Errorf("%s is required", req.key)
		}
	}
	coeff, _, err := assurance.ParseDecimal(r.MaterialityPct)
	if err != nil {
		return fmt.Errorf("materiality_pct: %w", err)
	}
	if coeff.Sign() < 0 {
		return errors.New("materiality_pct must be >= 0")
	}
	if len(r.Fields) == 0 {
		return errors.New("fields: at least one field is required")
	}
	keys := map[string]bool{}
	for i, f := range r.Fields {
		if err := f.validate(ctx, fields); err != nil {
			label := f.Key
			if label == "" {
				label = fmt.Sprintf("#%d", i+1)
			}
			return fmt.Errorf("field %s: %w", label, err)
		}
		if keys[f.Key] {
			return fmt.Errorf("field %s: duplicate key", f.Key)
		}
		keys[f.Key] = true
	}
	for i, v := range r.Verbatim {
		if err := v.validate(r.root); err != nil {
			label := v.ID
			if label == "" {
				label = fmt.Sprintf("#%d", i+1)
			}
			return fmt.Errorf("verbatim %s: %w", label, err)
		}
	}
	return nil
}

func (f fieldDef) validate(ctx context.Context, fields fieldLookup) error {
	for _, req := range []struct{ key, val string }{
		{"key", f.Key}, {"label", f.Label}, {"unit", f.Unit},
		{"source", f.Source}, {"target", f.Target},
	} {
		if strings.TrimSpace(req.val) == "" {
			return fmt.Errorf("%s is required", req.key)
		}
	}
	if f.Decimals < 0 || f.Decimals > 6 {
		return fmt.Errorf("decimals must be between 0 and 6, got %d", f.Decimals)
	}
	if f.DisplayUnit != "" {
		if _, err := unitShift(f.Unit, f.DisplayUnit); err != nil {
			return fmt.Errorf("display_unit: %w", err)
		}
	}
	for _, b := range []struct{ key, val string }{{"min", f.Min}, {"max", f.Max}} {
		if b.val == "" {
			continue
		}
		if _, _, err := assurance.ParseDecimal(b.val); err != nil {
			return fmt.Errorf("%s: %w", b.key, err)
		}
	}
	if fields == nil {
		return errors.New("mapping store is not available")
	}
	for _, m := range []struct{ key, val string }{{"source", f.Source}, {"target", f.Target}, {"prior", f.Prior}} {
		if m.val == "" {
			continue
		}
		got, err := fields.GetField(ctx, m.val)
		if err != nil {
			return fmt.Errorf("%s: look up mapped field %s: %w", m.key, m.val, err)
		}
		if got == nil {
			return fmt.Errorf("%s: unknown mapped field %s", m.key, m.val)
		}
	}
	return nil
}

// validate stats the file through root, which refuses '..' and symlink escapes
// out of the sections directory.
func (v verbatimDef) validate(root *os.Root) error {
	for _, req := range []struct{ key, val string }{{"id", v.ID}, {"title", v.Title}, {"file", v.File}} {
		if strings.TrimSpace(req.val) == "" {
			return fmt.Errorf("%s is required", req.key)
		}
	}
	if !filepath.IsLocal(v.File) {
		return fmt.Errorf("file %q must be relative to the sections directory and must not contain a parent-directory element", v.File)
	}
	info, err := root.Stat(v.File)
	if err != nil {
		return fmt.Errorf("file %q: %w", v.File, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("file %q is not a regular file", v.File)
	}
	if info.Size() == 0 {
		return fmt.Errorf("file %q is empty", v.File)
	}
	return nil
}
