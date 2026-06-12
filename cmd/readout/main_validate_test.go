package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunConfigValidate covers the `config validate` exit-code contract: 0 for a
// config that would pass startup, 1 (with the loader's own error text) for one
// that would fail, and 2 for usage errors. The loader is the same one startup
// uses, so the assertions pin faithful-to-startup behavior.
func TestRunConfigValidate(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantCode int
		wantOut  string // substring expected on stdout
		wantErr  string // substring expected on stderr
	}{
		{
			name:     "valid config",
			content:  "port: 8080\n",
			wantCode: 0,
			wantOut:  "config OK",
		},
		{
			name:     "broken yaml",
			content:  "port: 8080\n  bad: : indent\n",
			wantCode: 1,
			wantErr:  "parse config",
		},
		{
			name:     "unknown key",
			content:  "notAKey: true\n",
			wantCode: 1,
			wantErr:  "parse config",
		},
		{
			// OIDC settings present while auth.mode stays "none" must report the
			// implicit-promotion-removed error verbatim, proving validate runs the
			// full semantic check, not just a YAML shape check.
			name:     "promotion error",
			content:  "auth:\n  oidc:\n    issuerUrl: https://issuer.example\n",
			wantCode: 1,
			wantErr:  `oidc settings present but auth.mode is "none"; set auth.mode: oidc (implicit promotion was removed)`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath := writeConfig(t, tt.content)
			var stdout, stderr bytes.Buffer
			code := run([]string{"config", "validate", "--config", cfgPath}, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d; stdout=%q stderr=%q", code, tt.wantCode, stdout.String(), stderr.String())
			}
			if tt.wantOut != "" && !strings.Contains(stdout.String(), tt.wantOut) {
				t.Fatalf("stdout = %q, want substring %q", stdout.String(), tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

// TestRunConfigValidateMissingFile pins that a missing --config path fails
// validate with the loader's read error (exit 1), matching what startup prints.
func TestRunConfigValidateMissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "validate", "--config", "/no/such/readout.yaml"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "read config") {
		t.Fatalf("stderr = %q, want substring %q", stderr.String(), "read config")
	}
}

// TestRunConfigUsage pins the usage exit code (2) for a bare `config`, an
// unknown sub-subcommand, and stray positional arguments after `validate` --
// all writing usage to stderr and nothing to stdout.
func TestRunConfigUsage(t *testing.T) {
	for _, args := range [][]string{
		{"config"},
		{"config", "bogus"},
		{"config", "validate", "stray"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("args %v: exit code = %d, want 2", args, code)
		}
		if stdout.Len() != 0 {
			t.Fatalf("args %v: stdout = %q, want empty", args, stdout.String())
		}
		if !strings.Contains(stderr.String(), "validate") {
			t.Fatalf("args %v: stderr = %q, want usage mentioning validate", args, stderr.String())
		}
	}
}

// TestRunConfigValidateEnvOverlay pins the faithful-to-startup contract: a
// config that is invalid on file alone (OIDC mode without redirectUrl or
// publicUrl) validates clean when the READOUT_* env overlay supplies the
// missing value, exactly as startup would. Without env the same file fails.
// This guards against validate ever switching to a file-only parse path.
func TestRunConfigValidateEnvOverlay(t *testing.T) {
	content := "auth:\n  mode: oidc\n  oidc:\n    issuerUrl: https://issuer.example\n    clientId: client\n"
	cfgPath := writeConfig(t, content)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "validate", "--config", cfgPath}, &stdout, &stderr); code != 1 {
		t.Fatalf("without env: exit code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "redirectUrl or publicUrl is required") {
		t.Fatalf("without env: stderr = %q, want redirect-required error", stderr.String())
	}

	t.Setenv("READOUT_OIDC_REDIRECT_URL", "https://readout.example/oauth2/callback")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"config", "validate", "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("with env: exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "config OK") {
		t.Fatalf("with env: stdout = %q, want %q", stdout.String(), "config OK")
	}
}

// TestRunConfigValidateNoEnvFlag pins that validate accepts no --config (env-only
// configs are loadable) and returns 0; an empty config is valid at startup too.
func TestRunConfigValidateNoConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "validate"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "config OK") {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "config OK")
	}
}
