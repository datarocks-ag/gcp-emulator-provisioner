package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gcp-emulator-provisioner/internal/config"
	"gcp-emulator-provisioner/internal/provisioner"
)

// writeConfig writes a config file and points GCP_CONFIG_PATH at it.
func writeConfig(t *testing.T, contents string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	t.Setenv("GCP_CONFIG_PATH", path)
}

// TestRunWithNothingConfigured covers the whole run() path without a network:
// a config declaring no resources skips both sections, so neither client is
// ever constructed.
func TestRunWithNothingConfigured(t *testing.T) {
	writeConfig(t, "project_id: local-dev\n")
	t.Setenv("GCP_PROJECT_ID", "")

	for _, section := range []string{sectionAll, sectionPubSub, sectionStorage} {
		t.Run(section, func(t *testing.T) {
			if err := run(context.Background(), section, provisioner.DefaultOptions()); err != nil {
				t.Errorf("run(%q) = %v, want nil", section, err)
			}
		})
	}
}

func TestRunReportsAMissingConfigFile(t *testing.T) {
	t.Setenv("GCP_CONFIG_PATH", filepath.Join(t.TempDir(), "does-not-exist.yaml"))

	err := run(context.Background(), sectionAll, provisioner.DefaultOptions())
	if err == nil {
		t.Fatal("expected a missing config file to fail the run")
	}
	if !strings.Contains(err.Error(), "loading configuration") {
		t.Errorf("error = %v, want it to name the config load", err)
	}
}

func TestRunRequiresAProjectID(t *testing.T) {
	writeConfig(t, "pubsub:\n  topics:\n    - name: orders\n")
	t.Setenv("GCP_PROJECT_ID", "")

	err := run(context.Background(), sectionAll, provisioner.DefaultOptions())
	if err == nil {
		t.Fatal("expected a missing project id to fail the run")
	}
	if !strings.Contains(err.Error(), "project id is required") {
		t.Errorf("error = %v, want it to name the missing project id", err)
	}
}

func TestSetupLogging(t *testing.T) {
	// Every level, plus an unrecognised value that must fall back rather than fail.
	for _, level := range []string{"debug", "info", "warn", "error", "nonsense", ""} {
		t.Setenv("LOG_LEVEL", level)
		setupLogging()
	}
}

type failingCloser struct{ err error }

func (f failingCloser) Close() error { return f.err }

func TestCloseAndLogSwallowsFailures(t *testing.T) {
	// Provisioning has already finished by the time this runs, so a close
	// failure must not change the outcome.
	closeAndLog("Pub/Sub", failingCloser{err: errors.New("connection reset")})
	closeAndLog("Pub/Sub", failingCloser{})
}

func TestResolveDryRun(t *testing.T) {
	tests := []struct {
		name      string
		flagValue bool
		flagSet   bool
		env       string
		want      bool
		wantErr   bool
	}{
		{name: "unset defaults to false"},
		{name: "flag wins over env", flagValue: false, flagSet: true, env: "true", want: false},
		{name: "flag true", flagValue: true, flagSet: true, want: true},
		{name: "env true", env: "true", want: true},
		{name: "env 1", env: "1", want: true},
		{name: "env false", env: "false", want: false},
		{name: "env garbage", env: "yes-please", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("DRY_RUN", tt.env)
			}

			got, err := resolveDryRun(tt.flagValue, tt.flagSet)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDryRun: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveDryRun = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveProjectID(t *testing.T) {
	t.Run("env wins over config", func(t *testing.T) {
		t.Setenv("GCP_PROJECT_ID", "from-env")
		got, err := resolveProjectID(&config.Config{ProjectID: "from-file"})
		if err != nil {
			t.Fatalf("resolveProjectID: %v", err)
		}
		if got != "from-env" {
			t.Errorf("got %q, want from-env", got)
		}
	})

	t.Run("falls back to config", func(t *testing.T) {
		got, err := resolveProjectID(&config.Config{ProjectID: "from-file"})
		if err != nil {
			t.Fatalf("resolveProjectID: %v", err)
		}
		if got != "from-file" {
			t.Errorf("got %q, want from-file", got)
		}
	})

	t.Run("errors when neither is set", func(t *testing.T) {
		_, err := resolveProjectID(&config.Config{})
		if err == nil {
			t.Fatal("expected an error when no project id is configured")
		}
		if !strings.Contains(err.Error(), "GCP_PROJECT_ID") {
			t.Errorf("error should name the env var, got: %v", err)
		}
	})
}

func TestWants(t *testing.T) {
	tests := []struct {
		section string
		want    string
		ok      bool
	}{
		{sectionAll, sectionPubSub, true},
		{sectionAll, sectionStorage, true},
		{sectionPubSub, sectionPubSub, true},
		{sectionPubSub, sectionStorage, false},
		{sectionStorage, sectionPubSub, false},
	}

	for _, tt := range tests {
		if got := wants(tt.section, tt.want); got != tt.ok {
			t.Errorf("wants(%q, %q) = %v, want %v", tt.section, tt.want, got, tt.ok)
		}
	}
}

func TestCompareCreateOnlyBucketAttrs(t *testing.T) {
	t.Setenv("STORAGE_EMULATOR_HOST", "gcs:4443")
	if compareCreateOnlyBucketAttrs() {
		t.Error("an emulator fabricates location and storage class, so the comparison must be off")
	}

	t.Setenv("STORAGE_EMULATOR_HOST", "")
	if !compareCreateOnlyBucketAttrs() {
		t.Error("against real Cloud Storage the comparison must be on")
	}
}

func TestEnvOrDefault(t *testing.T) {
	if got := envOrDefault("DEFINITELY_NOT_SET_XYZ", "fallback"); got != "fallback" {
		t.Errorf("got %q, want fallback", got)
	}

	t.Setenv("GCP_TEST_VAR", "value")
	if got := envOrDefault("GCP_TEST_VAR", "fallback"); got != "value" {
		t.Errorf("got %q, want value", got)
	}
}
