package provisioner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"gcp-emulator-provisioner/internal/config"
	"gcp-emulator-provisioner/internal/credentials"
)

// ProvisionCredentials writes the configured service account key files.
//
// This section touches no endpoint, so it runs whether or not an emulator is
// up — which matters, because the applications that need the key file usually
// start alongside the provisioner rather than after it.
func (p *Provisioner) ProvisionCredentials(_ context.Context) error {
	creds := p.cfg.Credentials
	if len(creds) == 0 {
		slog.Debug("No credentials configured, skipping section")
		return nil
	}

	slog.Info("Provisioning credentials", "files", len(creds))

	for _, cred := range creds {
		strategy := config.EffectiveCredentialStrategy(cred.Strategy)
		if err := p.ensureCredential(cred, strategy); err != nil {
			return fmt.Errorf("writing credential %q: %w", cred.Path, err)
		}
	}

	return nil
}

// ensureCredential writes one key file, leaving an existing one alone.
//
// A key file is create-only by nature: the contents are a freshly generated
// keypair, so rewriting it on every run would rotate the credential underneath
// whatever is already using it, and would never converge. Only an explicit
// strategy of "update" on the credential regenerates.
func (p *Provisioner) ensureCredential(cred config.Credential, strategy string) error {
	// main.go writes the resolved project id back into the config, so
	// GCP_PROJECT_ID overriding project_id is already accounted for here.
	projectID := p.cfg.ProjectID
	account := cred.Account
	if account == "" {
		account = credentials.DefaultAccount
	}

	exists, err := fileExists(cred.Path)
	if err != nil {
		return err
	}

	if exists && strategy != "update" {
		slog.Info("Skipping existing credential file (a key file is never rewritten unless strategy=update)",
			"path", cred.Path)
		return nil
	}

	if p.opts.DryRun {
		action := "Would write credential file"
		if exists {
			action = "Would replace credential file"
		}
		slog.Info(action,
			"path", cred.Path,
			"account", credentials.Email(projectID, account),
			"token_uri", cred.TokenURI)
		return nil
	}

	key, err := credentials.Generate(projectID, account, cred.TokenURI)
	if err != nil {
		return err
	}
	if err := credentials.Write(cred.Path, key); err != nil {
		return err
	}

	slog.Info("Wrote credential file",
		"path", cred.Path,
		"account", key.ClientEmail,
		"token_uri", key.TokenURI,
		"replaced", exists,
		"note", "generated locally, authenticates to nothing")
	return nil
}

// fileExists reports whether path is already present, treating anything other
// than a plain "not found" as a real failure.
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("checking %q: %w", path, err)
}
