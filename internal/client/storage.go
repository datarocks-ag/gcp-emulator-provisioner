package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// operationTimeout bounds a single bucket call.
//
// fake-gcs-server answers unsupported requests with a 500 carrying reason
// "internalError", which the Cloud Storage client treats as retryable and
// retries until its context expires. Without a deadline a single unsupported
// field would hang the provisioner forever instead of failing the run.
const operationTimeout = 30 * time.Second

// Bucket is a transport-neutral Cloud Storage bucket.
type Bucket struct {
	Name              string
	VersioningEnabled bool
	// Location and StorageClass are only ever sent at creation.
	Location     string
	StorageClass string
}

// StorageClient wraps the Cloud Storage bucket admin API for a single project.
type StorageClient struct {
	projectID string
	client    *storage.Client
}

// ErrBucketNotFound reports that a bucket does not exist.
var ErrBucketNotFound = errors.New("bucket not found")

// ConnectStorage dials Cloud Storage and blocks until it answers.
//
// An empty emulatorHost means Google Cloud, reached with Application Default
// Credentials. A host — with or without a scheme — means an emulator such as
// fake-gcs-server, reached unauthenticated.
func ConnectStorage(ctx context.Context, projectID, emulatorHost string) (*StorageClient, error) {
	var opts []option.ClientOption
	if emulatorHost != "" {
		opts = append(opts,
			option.WithEndpoint(storageEndpoint(emulatorHost)),
			option.WithoutAuthentication(),
		)
	}

	sc, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating storage client: %w", err)
	}

	c := &StorageClient{projectID: projectID, client: sc}

	// Listing one bucket is the cheapest call that proves the API is answering.
	probe := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, operationTimeout)
		defer cancel()

		it := sc.Buckets(ctx, projectID)
		if _, err := it.Next(); err != nil && !errors.Is(err, iterator.Done) {
			return err
		}
		return nil
	}

	if err := retryUntilReady(ctx, "Cloud Storage", probe); err != nil {
		_ = sc.Close()
		return nil, err
	}

	slog.Info("Connected to Cloud Storage", "project", projectID, "emulator", emulatorHost != "")
	return c, nil
}

// storageEndpoint normalises an emulator host into the JSON API base URL the
// Cloud Storage client expects. STORAGE_EMULATOR_HOST is conventionally set to
// a bare host:port, while the client wants a full path.
func storageEndpoint(host string) string {
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	host = strings.TrimSuffix(host, "/")
	if strings.HasSuffix(host, "/storage/v1") {
		return host + "/"
	}
	return host + "/storage/v1/"
}

// Close releases the underlying client.
func (c *StorageClient) Close() error {
	return c.client.Close()
}

// GetBucket returns the bucket, or ErrBucketNotFound if it does not exist.
func (c *StorageClient) GetBucket(ctx context.Context, name string) (*Bucket, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()

	attrs, err := c.client.Bucket(name).Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrBucketNotExist) {
			return nil, ErrBucketNotFound
		}
		return nil, fmt.Errorf("getting bucket %q: %w", name, err)
	}

	return &Bucket{
		Name:              attrs.Name,
		VersioningEnabled: attrs.VersioningEnabled,
		Location:          attrs.Location,
		StorageClass:      attrs.StorageClass,
	}, nil
}

// CreateBucket creates a bucket with its create-time attributes. An already
// existing bucket is not an error, so concurrent runs do not fight each other.
func (c *StorageClient) CreateBucket(ctx context.Context, b Bucket) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()

	attrs := &storage.BucketAttrs{
		VersioningEnabled: b.VersioningEnabled,
		Location:          b.Location,
		StorageClass:      b.StorageClass,
	}

	if err := c.client.Bucket(b.Name).Create(ctx, c.projectID, attrs); err != nil {
		if isBucketExistsError(err) {
			return nil
		}
		return fmt.Errorf("creating bucket %q: %w", b.Name, err)
	}
	return nil
}

// SetVersioning enables or suspends object versioning on an existing bucket.
func (c *StorageClient) SetVersioning(ctx context.Context, name string, enabled bool) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()

	_, err := c.client.Bucket(name).Update(ctx, storage.BucketAttrsToUpdate{
		VersioningEnabled: enabled,
	})
	if err != nil {
		return fmt.Errorf("setting versioning on bucket %q: %w", name, err)
	}
	return nil
}

// isBucketExistsError reports whether err is Cloud Storage rejecting a create
// because the bucket is already there.
func isBucketExistsError(err error) bool {
	var apiErr interface{ HTTPCode() int }
	if errors.As(err, &apiErr) && apiErr.HTTPCode() == 409 {
		return true
	}
	return strings.Contains(err.Error(), "already exists") ||
		strings.Contains(err.Error(), "409")
}

// IsVersioningUnsupported reports whether err is fake-gcs-server refusing
// versioning because it was started on its default filesystem backend.
//
// Only the in-memory backend implements versioning; the filesystem backend
// answers with a 500 whose body says so. That is a deployment mistake worth a
// clear message rather than a generic API failure.
func IsVersioningUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "does not support versioning")
}
