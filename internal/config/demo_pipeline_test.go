package config

import (
	"os"
	"testing"
)

// TestDemoPipelineEnabled covers the demo pipeline flag from YAML and from
// NL_DEMO_PIPELINE. The environment variable overrides YAML, and only the
// literal "true" enables the flag.
func TestDemoPipelineEnabled(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		// env is the NL_DEMO_PIPELINE value; nil means the variable is unset.
		env  *string
		want bool
	}{
		{name: "no yaml and no env", yaml: "", env: nil, want: false},
		{name: "env true", yaml: "", env: strPtr("true"), want: true},
		{name: "yaml true env unset", yaml: "demo_pipeline_enabled: true\n", env: nil, want: true},
		{name: "yaml true env false", yaml: "demo_pipeline_enabled: true\n", env: strPtr("false"), want: false},
		{name: "env 1 is not true", yaml: "", env: strPtr("1"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			// clearEnv does not cover NL_DEMO_PIPELINE. Setenv then Unsetenv
			// registers a cleanup that restores the host value.
			t.Setenv("NL_DEMO_PIPELINE", "")
			if err := os.Unsetenv("NL_DEMO_PIPELINE"); err != nil {
				t.Fatalf("unset NL_DEMO_PIPELINE: %v", err)
			}
			if tt.env != nil {
				t.Setenv("NL_DEMO_PIPELINE", *tt.env)
			}
			path := ""
			if tt.yaml != "" {
				path = writeYAML(t, tt.yaml)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DemoPipelineEnabled != tt.want {
				t.Fatalf("DemoPipelineEnabled = %t, want %t", cfg.DemoPipelineEnabled, tt.want)
			}
		})
	}
}

func strPtr(s string) *string {
	return &s
}
