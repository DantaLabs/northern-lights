package main

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// verifyMCPCase drives scripts/verify-mcp.sh against an in-process server.
type verifyMCPCase struct {
	name        string
	pipeline    bool   // NL_DEMO_PIPELINE=true on the server when set
	expectTools string // NL_EXPECT_TOOLS for the script; empty leaves it unset
	wantExit    bool   // true: script must exit 0; false: must exit non-zero
	wantOutput  []string
}

func TestVerifyMCPScript(t *testing.T) {
	for _, bin := range []string{"bash", "curl", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH: %v", bin, err)
		}
	}

	cases := []verifyMCPCase{
		{
			name:     "flag_off",
			pipeline: false,
			wantExit: true,
		},
		{
			name:        "flag_on",
			pipeline:    true,
			expectTools: "20",
			wantExit:    true,
		},
		{
			name:       "flag_on_default_expectation",
			pipeline:   true,
			wantExit:   false,
			wantOutput: []string{"tools/list has exactly 13 tools (got 20)"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NL_DEMO_MODE", "true")
			t.Setenv("NL_DB_PATH", filepath.Join(t.TempDir(), "demo.db"))
			t.Setenv("NL_API_KEY", "demo")
			t.Setenv("NL_LISTEN_ADDR", "127.0.0.1:0")
			t.Setenv("NL_READ_CACHE_TTL", "0")
			if tc.pipeline {
				t.Setenv("NL_DEMO_PIPELINE", "true")
				t.Setenv("NL_DEMO_SECTIONS_DIR", t.TempDir())
			} else {
				t.Setenv("NL_DEMO_PIPELINE", "")
				if err := os.Unsetenv("NL_DEMO_PIPELINE"); err != nil {
					t.Fatalf("unset NL_DEMO_PIPELINE: %v", err)
				}
			}

			_, handler, cleanup, err := buildServer("", "")
			if err != nil {
				t.Fatalf("buildServer: %v", err)
			}
			t.Cleanup(cleanup)
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)

			env := make([]string, 0, len(os.Environ())+3)
			for _, kv := range os.Environ() {
				if strings.HasPrefix(kv, "NL_EXPECT_TOOLS=") {
					continue
				}
				env = append(env, kv)
			}
			env = append(env, "NL_URL="+srv.URL+"/mcp", "NL_KEY=demo")
			if tc.expectTools != "" {
				env = append(env, "NL_EXPECT_TOOLS="+tc.expectTools)
			}

			var out bytes.Buffer
			cmd := exec.Command("bash", filepath.Join("..", "..", "scripts", "verify-mcp.sh"))
			cmd.Env = env
			cmd.Stdout = &out
			cmd.Stderr = &out
			runErr := cmd.Run()
			output := out.String()

			if tc.wantExit {
				if runErr != nil {
					t.Fatalf("verify-mcp.sh exited with error: %v\n%s", runErr, output)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) || exitErr.ExitCode() == 0 {
					t.Fatalf("verify-mcp.sh want non-zero exit, got %v\n%s", runErr, output)
				}
			}
			for _, want := range tc.wantOutput {
				if !strings.Contains(output, want) {
					t.Fatalf("verify-mcp.sh output missing %q\n%s", want, output)
				}
			}
		})
	}
}
