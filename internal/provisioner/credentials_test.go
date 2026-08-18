package provisioner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gcp-emulator-provisioner/internal/config"
)

func credentialConfig(path string, cred config.Credential) *config.Config {
	cred.Path = path
	return &config.Config{ProjectID: "local-dev", Credentials: []config.Credential{cred}}
}

func TestProvisionCredentialsWritesAKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sa.json")
	cfg := credentialConfig(path, config.Credential{})

	p := New(nil, nil, cfg, DefaultOptions())
	if err := p.ProvisionCredentials(context.Background()); err != nil {
		t.Fatalf("ProvisionCredentials: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading key: %v", err)
	}
	var key map[string]any
	if err := json.Unmarshal(raw, &key); err != nil {
		t.Fatalf("key is not valid JSON: %v", err)
	}
	if key["type"] != "service_account" {
		t.Errorf("type = %v", key["type"])
	}
	if key["client_email"] != "local-emulator@local-dev.iam.gserviceaccount.com" {
		t.Errorf("client_email = %v", key["client_email"])
	}
}

// TestProvisionCredentialsLeavesAnExistingKeyAlone is the important one: the
// contents are a fresh keypair, so rewriting would rotate the credential
// underneath whatever already loaded it, and would never converge.
func TestProvisionCredentialsLeavesAnExistingKeyAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sa.json")
	cfg := credentialConfig(path, config.Credential{})
	p := New(nil, nil, cfg, DefaultOptions())

	if err := p.ProvisionCredentials(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading key: %v", err)
	}

	if err := p.ProvisionCredentials(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading key: %v", err)
	}

	if string(first) != string(second) {
		t.Error("a second run rotated the private key; the file must be left alone")
	}
}

// A global strategy of "update" must not reach credentials — only an explicit
// per-credential strategy regenerates.
func TestGlobalUpdateStrategyDoesNotRotateKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sa.json")
	cfg := credentialConfig(path, config.Credential{})
	cfg.Strategy = "update"

	p := New(nil, nil, cfg, DefaultOptions())
	if err := p.ProvisionCredentials(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first, _ := os.ReadFile(path)

	if err := p.ProvisionCredentials(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second, _ := os.ReadFile(path)

	if string(first) != string(second) {
		t.Error("the global update strategy must not rotate a key file")
	}
}

func TestCredentialStrategyUpdateRegenerates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sa.json")
	cfg := credentialConfig(path, config.Credential{Strategy: "update"})

	p := New(nil, nil, cfg, DefaultOptions())
	if err := p.ProvisionCredentials(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first, _ := os.ReadFile(path)

	if err := p.ProvisionCredentials(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second, _ := os.ReadFile(path)

	if string(first) == string(second) {
		t.Error("an explicit strategy=update must regenerate the key")
	}
}

func TestProvisionCredentialsDryRunWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sa.json")
	cfg := credentialConfig(path, config.Credential{})

	p := New(nil, nil, cfg, Options{DryRun: true})
	if err := p.ProvisionCredentials(context.Background()); err != nil {
		t.Fatalf("ProvisionCredentials: %v", err)
	}

	if _, err := os.Stat(path); err == nil {
		t.Error("dry run wrote a key file")
	}
}

func TestProvisionCredentialsHonoursAccountAndTokenURI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sa.json")
	cfg := credentialConfig(path, config.Credential{
		Account:  "orders-service",
		TokenURI: "http://oauth2-stub:8080/token",
	})

	p := New(nil, nil, cfg, DefaultOptions())
	if err := p.ProvisionCredentials(context.Background()); err != nil {
		t.Fatalf("ProvisionCredentials: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var key map[string]any
	if err := json.Unmarshal(raw, &key); err != nil {
		t.Fatalf("key is not valid JSON: %v", err)
	}
	if key["client_email"] != "orders-service@local-dev.iam.gserviceaccount.com" {
		t.Errorf("client_email = %v", key["client_email"])
	}
	if key["token_uri"] != "http://oauth2-stub:8080/token" {
		t.Errorf("token_uri = %v", key["token_uri"])
	}
}

func TestCredentialsSectionSkipsWhenNotConfigured(t *testing.T) {
	p := New(nil, nil, &config.Config{ProjectID: "local-dev"}, DefaultOptions())
	if err := p.ProvisionCredentials(context.Background()); err != nil {
		t.Fatalf("an empty credentials section must be a no-op: %v", err)
	}
}

func TestRunWritesCredentialsBeforeProvisioning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sa.json")

	ps := newMockPubSub()
	cfg := credentialConfig(path, config.Credential{})
	cfg.PubSub = config.PubSub{Topics: []config.Topic{{Name: "orders"}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("Run did not write the credential file: %v", err)
	}
	if len(ps.createdTopics) != 1 {
		t.Errorf("Run did not continue to the Pub/Sub section: %+v", ps.createdTopics)
	}
}

func TestCredentialErrorNamesThePath(t *testing.T) {
	// A directory where the file should go makes the write fail.
	dir := t.TempDir()
	path := filepath.Join(dir, "sa.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("setting up: %v", err)
	}

	cfg := credentialConfig(path, config.Credential{Strategy: "update"})
	p := New(nil, nil, cfg, DefaultOptions())

	err := p.ProvisionCredentials(context.Background())
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should name the path, got %v", err)
	}
}
