package credentials

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateProducesAParseablePrivateKey(t *testing.T) {
	key, err := Generate("local-dev", "", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// This is the whole point of the feature: libraries such as Spring Cloud GCP
	// parse the PEM eagerly at startup, so a placeholder string would fail where
	// a real keypair succeeds.
	block, _ := pem.Decode([]byte(key.PrivateKey))
	if block == nil {
		t.Fatal("private key is not valid PEM")
	}
	if block.Type != "PRIVATE KEY" {
		t.Errorf("PEM type = %q, want PKCS#8 \"PRIVATE KEY\"", block.Type)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("private key does not parse as PKCS#8: %v", err)
	}
	if _, ok := parsed.(*rsa.PrivateKey); !ok {
		t.Errorf("private key is %T, want *rsa.PrivateKey", parsed)
	}
}

func TestGenerateFieldsMatchGooglesShape(t *testing.T) {
	key, err := Generate("local-dev", "", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if key.Type != "service_account" {
		t.Errorf("type = %q", key.Type)
	}
	if key.ProjectID != "local-dev" {
		t.Errorf("project_id = %q", key.ProjectID)
	}
	if key.ClientEmail != "local-emulator@local-dev.iam.gserviceaccount.com" {
		t.Errorf("client_email = %q", key.ClientEmail)
	}
	// Fixed markers, so the file cannot be mistaken for a Google-issued key.
	if key.PrivateKeyID != "local-emulator-key" || key.ClientID != "000000000000000000000" {
		t.Errorf("identifiers should be obviously fake: %+v", key)
	}
	if key.TokenURI != googleTokenURI {
		t.Errorf("token_uri = %q, want Google's endpoint when no override is given", key.TokenURI)
	}
}

func TestGenerateHonoursAccountAndTokenURI(t *testing.T) {
	key, err := Generate("local-dev", "orders-service", "http://oauth2-stub:8080/token")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if key.ClientEmail != "orders-service@local-dev.iam.gserviceaccount.com" {
		t.Errorf("client_email = %q", key.ClientEmail)
	}
	// Every OAuth URL points at the stub, so nothing reaches accounts.google.com.
	for name, got := range map[string]string{
		"token_uri":                   key.TokenURI,
		"auth_uri":                    key.AuthURI,
		"auth_provider_x509_cert_url": key.AuthProviderX509CertURL,
		"client_x509_cert_url":        key.ClientX509CertURL,
	} {
		if got != "http://oauth2-stub:8080/token" {
			t.Errorf("%s = %q, want the stub", name, got)
		}
	}
}

func TestGenerateRequiresAProject(t *testing.T) {
	if _, err := Generate("", "", ""); err == nil {
		t.Error("a key without a project id must be rejected")
	}
}

func TestGenerateIsNotDeterministic(t *testing.T) {
	a, err := Generate("local-dev", "", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := Generate("local-dev", "", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Why the file is create-only: rewriting it would rotate the key underneath
	// whatever already loaded it.
	if a.PrivateKey == b.PrivateKey {
		t.Error("two generated keys must not share a private key")
	}
}

func TestWriteEmitsGooglesOnDiskForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "sa.json")

	key, err := Generate("local-dev", "", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Write(path, key); err != nil {
		t.Fatalf("Write: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading key: %v", err)
	}

	// Newlines have to be escaped in the JSON string, which is how Google emits
	// it and what the client libraries expect.
	if !strings.Contains(string(raw), `\n`) {
		t.Error("PEM newlines should be escaped in the JSON")
	}

	var round Key
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("written key is not valid JSON: %v", err)
	}
	if round.PrivateKey != key.PrivateKey {
		t.Error("private key did not survive the round trip")
	}

	// Pinned to the literal rather than to FileMode, so tightening the constant
	// has to be a deliberate decision: the key is read by other containers,
	// usually running as a different UID, and 0600 would deny them at startup.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("key permissions = %v, want 0644", info.Mode().Perm())
	}

	// The parent directory must be traversable for the same reason.
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o755 {
		t.Errorf("directory permissions = %v, want 0755", dirInfo.Mode().Perm())
	}
}
