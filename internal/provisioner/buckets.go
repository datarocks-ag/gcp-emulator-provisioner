package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"gcp-emulator-provisioner/internal/client"
	"gcp-emulator-provisioner/internal/config"
)

func (p *Provisioner) ensureBucket(ctx context.Context, bucket config.Bucket, strategy string) error {
	current, err := p.storage.GetBucket(ctx, bucket.Name)
	if err != nil && !errors.Is(err, client.ErrBucketNotFound) {
		return fmt.Errorf("checking bucket: %w", err)
	}

	if errors.Is(err, client.ErrBucketNotFound) {
		desired := client.Bucket{
			Name:     bucket.Name,
			Location: bucket.Location,
			// Cloud Storage reports storage classes upper-cased, and validation
			// accepts either case, so normalise before sending.
			StorageClass: strings.ToUpper(bucket.StorageClass),
		}
		if bucket.Versioning != nil {
			desired.VersioningEnabled = *bucket.Versioning
		}

		if p.opts.DryRun {
			// Settings cannot be reconciled against a bucket that does not exist
			// yet, so report the intent rather than reading live state.
			slog.Info("Would create bucket", "bucket", bucket.Name,
				"versioning", desired.VersioningEnabled,
				"location", bucket.Location, "storage_class", bucket.StorageClass)
			return nil
		}

		slog.Info("Creating bucket", "bucket", bucket.Name)
		if err := p.storage.CreateBucket(ctx, desired); err != nil {
			return annotateVersioningError(err, desired.VersioningEnabled)
		}
		return nil
	}

	if strategy == "create" {
		// Matching the sibling provisioners: "create" on an existing resource
		// skips its settings too.
		slog.Info("Skipping existing bucket and its settings (strategy=create)", "bucket", bucket.Name)
		return nil
	}

	slog.Debug("Bucket already exists", "bucket", bucket.Name)

	if p.opts.CompareCreateOnlyBucketAttrs {
		for _, d := range createOnlyDrift(bucket, current) {
			slog.Warn("Bucket attribute cannot be changed after creation, leaving it as is",
				"bucket", bucket.Name, "field", d.field, "current", d.current, "configured", d.desired,
				"remedy", "create a new bucket and migrate the objects if the new value is required")
		}
	}

	return p.ensureVersioning(ctx, bucket, current)
}

// createOnlyDrift returns the create-time attributes the config now disagrees
// with. Cloud Storage cannot move a bucket or rewrite its default storage class
// in place, so this is reported and never applied — the same call
// immutableDrift makes for a subscription.
//
// An empty live value counts as agreement: an endpoint that does not model the
// attribute must not produce a warning. Comparison is case-insensitive because
// Cloud Storage reports both upper-cased while the config accepts either.
func createOnlyDrift(cfg config.Bucket, current *client.Bucket) []drift {
	var drifts []drift

	if cfg.Location != "" && current.Location != "" &&
		!strings.EqualFold(cfg.Location, current.Location) {
		drifts = append(drifts, drift{"location", current.Location, cfg.Location})
	}
	if cfg.StorageClass != "" && current.StorageClass != "" &&
		!strings.EqualFold(cfg.StorageClass, current.StorageClass) {
		drifts = append(drifts, drift{"storage_class", current.StorageClass, cfg.StorageClass})
	}

	return drifts
}

// ensureVersioning reconciles the bucket's versioning status. An omitted
// `versioning` field leaves whatever is configured on the bucket alone.
func (p *Provisioner) ensureVersioning(ctx context.Context, bucket config.Bucket, current *client.Bucket) error {
	if bucket.Versioning == nil {
		return nil
	}
	desired := *bucket.Versioning

	if current.VersioningEnabled == desired {
		slog.Debug("Bucket versioning up to date", "bucket", bucket.Name, "enabled", desired)
		return nil
	}

	if p.opts.DryRun {
		slog.Info("Would set bucket versioning",
			"bucket", bucket.Name, "current", current.VersioningEnabled, "enabled", desired)
		return nil
	}

	slog.Info("Setting bucket versioning", "bucket", bucket.Name, "enabled", desired)
	if err := p.storage.SetVersioning(ctx, bucket.Name, desired); err != nil {
		return annotateVersioningError(err, desired)
	}
	return nil
}

// annotateVersioningError turns fake-gcs-server's opaque "does not support
// versioning" 500 into an error that names the fix, since the cause is how the
// emulator was started rather than anything in the config.
func annotateVersioningError(err error, versioningWanted bool) error {
	if versioningWanted && client.IsVersioningUnsupported(err) {
		return fmt.Errorf(
			"%w (fake-gcs-server only implements versioning on its in-memory backend: "+
				"start it with -backend memory, or drop `versioning` from the bucket)", err)
	}
	return err
}
