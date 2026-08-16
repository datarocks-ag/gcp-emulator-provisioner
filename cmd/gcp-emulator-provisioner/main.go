// Command gcp-emulator-provisioner idempotently provisions Pub/Sub and Cloud Storage
// resources from a YAML config file, against an emulator or Google Cloud.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"gcp-emulator-provisioner/internal/client"
	"gcp-emulator-provisioner/internal/config"
	"gcp-emulator-provisioner/internal/provisioner"
)

var version = "dev"

// Provisioning sections. Pub/Sub and Cloud Storage are independent services
// reached over different endpoints, so a deployment whose emulators live in
// separate stacks can run one section per container.
const (
	sectionAll     = "all"
	sectionPubSub  = "pubsub"
	sectionStorage = "storage"
)

func main() {
	sectionFlag := flag.String("section", "",
		`Provisioning section: "pubsub", "storage", or "all" (default). Overrides GCP_SECTION.`)
	dryRunFlag := flag.Bool("dry-run", false,
		"Log every mutation as a preview without applying it. Overrides DRY_RUN.")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	// Distinguishes "-dry-run not passed" from "-dry-run=false", so the flag can
	// take precedence over the env var only when it was actually given.
	dryRunSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "dry-run" {
			dryRunSet = true
		}
	})

	if *showVersion {
		fmt.Println(version)
		return
	}

	setupLogging()
	slog.Info("Starting gcp-emulator-provisioner", "version", version)

	// The CLI flag takes precedence over the env var, matching the sibling provisioners.
	section := *sectionFlag
	if section == "" {
		section = envOrDefault("GCP_SECTION", sectionAll)
	}
	if section != sectionAll && section != sectionPubSub && section != sectionStorage {
		slog.Error("Invalid section",
			"section", section,
			"valid", []string{sectionAll, sectionPubSub, sectionStorage},
		)
		os.Exit(1)
	}

	dryRun, err := resolveDryRun(*dryRunFlag, dryRunSet)
	if err != nil {
		slog.Error("Invalid dry-run setting", "error", err)
		os.Exit(1)
	}
	if dryRun {
		slog.Info("Dry run: no changes will be applied")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, section, provisioner.Options{DryRun: dryRun}); err != nil {
		slog.Error("Provisioning failed", "error", err)
		os.Exit(1)
	}

	slog.Info("gcp-emulator-provisioner finished successfully")
}

// resolveDryRun resolves the dry-run setting, with the CLI flag taking
// precedence over DRY_RUN when it was explicitly passed.
func resolveDryRun(flagValue, flagSet bool) (bool, error) {
	if flagSet {
		return flagValue, nil
	}

	raw := os.Getenv("DRY_RUN")
	if raw == "" {
		return false, nil
	}

	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("parsing DRY_RUN %q as a boolean: %w", raw, err)
	}
	return parsed, nil
}

func run(ctx context.Context, section string, opts provisioner.Options) error {
	configPath := envOrDefault("GCP_CONFIG_PATH", "./config.yaml")

	slog.Info("Loading configuration", "path", configPath)
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	projectID, err := resolveProjectID(cfg)
	if err != nil {
		return err
	}

	slog.Info("Configuration loaded",
		"section", section,
		"project", projectID,
		"topics", len(cfg.PubSub.Topics),
		"buckets", len(cfg.Storage.Buckets),
	)

	// Declared as interfaces rather than concrete pointers so that skipping a
	// section leaves a nil interface instead of one wrapping a nil pointer.
	var pubsubAdmin provisioner.PubSubAdmin
	if wants(section, sectionPubSub) && len(cfg.PubSub.Topics) > 0 {
		pubsubClient, err := connectPubSub(ctx, projectID)
		if err != nil {
			return err
		}
		defer closeAndLog("Pub/Sub", pubsubClient)
		pubsubAdmin = pubsubClient
	}

	var storageAdmin provisioner.StorageAdmin
	if wants(section, sectionStorage) && len(cfg.Storage.Buckets) > 0 {
		storageClient, err := connectStorage(ctx, projectID)
		if err != nil {
			return err
		}
		defer closeAndLog("Cloud Storage", storageClient)
		storageAdmin = storageClient
		opts.CompareCreateOnlyBucketAttrs = compareCreateOnlyBucketAttrs()
	}

	p := provisioner.New(pubsubAdmin, storageAdmin, cfg, opts)

	switch section {
	case sectionPubSub:
		return p.ProvisionPubSub(ctx)
	case sectionStorage:
		return p.ProvisionStorage(ctx)
	default:
		return p.Run(ctx)
	}
}

// compareCreateOnlyBucketAttrs reports whether a bucket's create-time
// attributes are worth comparing against the config.
//
// Only real Cloud Storage answers the question truthfully: fake-gcs-server
// reports US-CENTRAL1 and STANDARD for every bucket whatever it was created
// with, so comparing against an emulator would warn on every run about a
// difference that is not real.
func compareCreateOnlyBucketAttrs() bool {
	return os.Getenv("STORAGE_EMULATOR_HOST") == ""
}

// wants reports whether the selected section includes the given one.
func wants(section, want string) bool {
	return section == sectionAll || section == want
}

// resolveProjectID reads the project from the environment, falling back to the
// config file. Every resource is namespaced by it, so there is no default.
func resolveProjectID(cfg *config.Config) (string, error) {
	if id := os.Getenv("GCP_PROJECT_ID"); id != "" {
		return id, nil
	}
	if cfg.ProjectID != "" {
		return cfg.ProjectID, nil
	}
	return "", errors.New("project id is required: set GCP_PROJECT_ID or project_id in the config file")
}

func connectPubSub(ctx context.Context, projectID string) (*client.PubSubClient, error) {
	// PUBSUB_EMULATOR_HOST is the variable the Google client libraries and the
	// gcloud emulator itself already agree on, so applications and provisioner
	// read the same setting.
	emulatorHost := os.Getenv("PUBSUB_EMULATOR_HOST")

	slog.Info("Connecting to Pub/Sub", "project", projectID, "emulator_host", emulatorHost)
	c, err := client.ConnectPubSub(ctx, projectID, emulatorHost)
	if err != nil {
		return nil, fmt.Errorf("connecting to Pub/Sub: %w", err)
	}
	return c, nil
}

func connectStorage(ctx context.Context, projectID string) (*client.StorageClient, error) {
	emulatorHost := os.Getenv("STORAGE_EMULATOR_HOST")

	slog.Info("Connecting to Cloud Storage", "project", projectID, "emulator_host", emulatorHost)
	c, err := client.ConnectStorage(ctx, projectID, emulatorHost)
	if err != nil {
		return nil, fmt.Errorf("connecting to Cloud Storage: %w", err)
	}
	return c, nil
}

// closeAndLog closes a client, reporting failures without changing the exit
// code — provisioning has already finished by the time this runs.
func closeAndLog(service string, c interface{ Close() error }) {
	if err := c.Close(); err != nil {
		slog.Warn("Failed to close client cleanly", "service", service, "error", err)
	}
}

func setupLogging() {
	level := slog.LevelInfo
	switch envOrDefault("LOG_LEVEL", "info") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}

func envOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
