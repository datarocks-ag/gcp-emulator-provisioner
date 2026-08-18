// Package credentials writes Google service account key files for local
// development.
//
// The key this produces is deliberately inert. The RSA keypair is generated
// here and Google holds no matching public key, so the file cannot authenticate
// to Google Cloud — it is the credential equivalent of a self-signed
// certificate for localhost. It exists because many client libraries and
// frameworks refuse to start unless GOOGLE_APPLICATION_CREDENTIALS points at a
// parseable service account key, even when every call they go on to make is
// answered by an emulator.
//
// Every identifying field is a fixed, obviously non-Google value so the file
// cannot be mistaken for a real credential at a glance.
package credentials

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultAccount is the service account id used when the config does not set one.
const DefaultAccount = "local-emulator"

// Fixed markers that make the file self-evidently fake. A real key carries a
// 40-character hex key id and a 21-digit client id issued by Google.
const (
	keyID    = "local-emulator-key"
	clientID = "000000000000000000000"
)

// Google's canonical OAuth endpoints, used when no token_uri override is given.
const (
	googleAuthURI     = "https://accounts.google.com/o/oauth2/auth"
	googleTokenURI    = "https://oauth2.googleapis.com/token"
	googleCertURI     = "https://www.googleapis.com/oauth2/v1/certs"
	googleClientCerts = "https://www.googleapis.com/robot/v1/metadata/x509/"
)

// keyBits is deliberately 2048 rather than 4096: the file never protects
// anything, and 4096 costs noticeably more on every run of a dev stack.
const keyBits = 2048

// FileMode is the permission a written key file carries. It authenticates
// nothing, but tools that read service account keys warn on loose permissions.
const FileMode os.FileMode = 0o600

// dirMode is used for parent directories created on the way to the key file.
const dirMode os.FileMode = 0o700

// Key is a Google service account key file, in the field order Google emits.
type Key struct {
	Type                    string `json:"type"`
	ProjectID               string `json:"project_id"`
	PrivateKeyID            string `json:"private_key_id"`
	PrivateKey              string `json:"private_key"`
	ClientEmail             string `json:"client_email"`
	ClientID                string `json:"client_id"`
	AuthURI                 string `json:"auth_uri"`
	TokenURI                string `json:"token_uri"`
	AuthProviderX509CertURL string `json:"auth_provider_x509_cert_url"`
	ClientX509CertURL       string `json:"client_x509_cert_url"`
}

// Email returns the service account address a key is issued to.
func Email(projectID, account string) string {
	return account + "@" + projectID + ".iam.gserviceaccount.com"
}

// Generate builds a service account key for the given project and account.
//
// An empty account falls back to DefaultAccount. An empty tokenURI uses
// Google's real endpoints; setting it points every OAuth URL at that one
// address, which is what makes the file usable offline — a library that does
// try to mint a token reaches the local stub instead of accounts.google.com.
func Generate(projectID, account, tokenURI string) (*Key, error) {
	if projectID == "" {
		return nil, fmt.Errorf("project id is required to build a service account key")
	}
	if account == "" {
		account = DefaultAccount
	}

	private, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, fmt.Errorf("generating rsa key: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, fmt.Errorf("encoding private key: %w", err)
	}
	// Google issues PKCS#8 PEM; the client libraries parse nothing else.
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	authURI, certURI, clientCerts := googleAuthURI, googleCertURI, googleClientCerts+Email(projectID, account)
	if tokenURI != "" {
		authURI, certURI, clientCerts = tokenURI, tokenURI, tokenURI
	} else {
		tokenURI = googleTokenURI
	}

	return &Key{
		Type:                    "service_account",
		ProjectID:               projectID,
		PrivateKeyID:            keyID,
		PrivateKey:              string(encoded),
		ClientEmail:             Email(projectID, account),
		ClientID:                clientID,
		AuthURI:                 authURI,
		TokenURI:                tokenURI,
		AuthProviderX509CertURL: certURI,
		ClientX509CertURL:       clientCerts,
	}, nil
}

// Write serialises the key to path, creating parent directories as needed.
//
// json.Marshal escapes the PEM newlines to \n, which is exactly the form Google
// emits and the client libraries expect.
func Write(path string, key *Key) error {
	encoded, err := json.MarshalIndent(key, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding service account key: %w", err)
	}
	encoded = append(encoded, '\n')

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return fmt.Errorf("creating directory for %q: %w", path, err)
		}
	}

	if err := os.WriteFile(path, encoded, FileMode); err != nil {
		return fmt.Errorf("writing service account key %q: %w", path, err)
	}
	return nil
}
