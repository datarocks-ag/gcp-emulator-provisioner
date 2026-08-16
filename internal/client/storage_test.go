package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGCS is a minimal stand-in for the Cloud Storage JSON API, enough to drive
// the bucket admin calls end to end.
//
// The real emulator is exercised by the integration tests; this covers the
// request/response plumbing — endpoint normalisation, attribute mapping and
// error classification — without Docker, so the paths stay measurable.
type fakeGCS struct {
	mu      sync.Mutex
	buckets map[string]map[string]any

	// forceStatus and forceBody, when set, replace the next response.
	forceStatus int
	forceBody   string

	listCalls int
}

func newFakeGCS(t *testing.T) (*fakeGCS, string) {
	t.Helper()

	f := &fakeGCS{buckets: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	// ConnectStorage takes a host, with or without a scheme, and normalises it.
	return f, strings.TrimPrefix(srv.URL, "http://")
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.forceStatus != 0 {
		http.Error(w, f.forceBody, f.forceStatus)
		f.forceStatus = 0
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/storage/v1/b")
	name = strings.Trim(name, "/")

	switch {
	case r.Method == http.MethodGet && name == "":
		f.listCalls++
		writeJSON(w, map[string]any{"kind": "storage#buckets", "items": []any{}})

	case r.Method == http.MethodGet:
		bucket, ok := f.buckets[name]
		if !ok {
			http.Error(w, `{"error":{"code":404,"message":"Not Found"}}`, http.StatusNotFound)
			return
		}
		writeJSON(w, bucket)

	case r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id, _ := body["name"].(string)
		if _, exists := f.buckets[id]; exists {
			http.Error(w, `{"error":{"code":409,"message":"already exists"}}`, http.StatusConflict)
			return
		}
		// Mirror the emulator: location and storage class are fabricated.
		body["location"] = "US-CENTRAL1"
		body["storageClass"] = "STANDARD"
		f.buckets[id] = body
		writeJSON(w, body)

	case r.Method == http.MethodPatch:
		bucket, ok := f.buckets[name]
		if !ok {
			http.Error(w, `{"error":{"code":404,"message":"Not Found"}}`, http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		for k, v := range body {
			bucket[k] = v
		}
		writeJSON(w, bucket)

	default:
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGCS) failNext(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceStatus = status
	f.forceBody = body
}

func (f *fakeGCS) bucket(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buckets[name]
}

func connectFake(t *testing.T, host string) *StorageClient {
	t.Helper()

	c, err := ConnectStorage(context.Background(), "local-dev", host)
	if err != nil {
		t.Fatalf("ConnectStorage: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestConnectStorageProbesTheEndpoint(t *testing.T) {
	f, host := newFakeGCS(t)
	connectFake(t, host)

	if f.listCalls == 0 {
		t.Error("ConnectStorage must probe the API before returning")
	}
}

func TestStorageBucketLifecycle(t *testing.T) {
	_, host := newFakeGCS(t)
	c := connectFake(t, host)
	ctx := context.Background()

	if _, err := c.GetBucket(ctx, "missing"); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("GetBucket on a missing bucket = %v, want ErrBucketNotFound", err)
	}

	if err := c.CreateBucket(ctx, Bucket{
		Name: "documents", VersioningEnabled: true, Location: "EU", StorageClass: "NEARLINE",
	}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	got, err := c.GetBucket(ctx, "documents")
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	if got.Name != "documents" || !got.VersioningEnabled {
		t.Errorf("bucket = %+v, want documents with versioning on", got)
	}
	// The fake fabricates these exactly as fake-gcs-server does, which is what
	// the create-only drift comparison has to tolerate.
	if got.Location != "US-CENTRAL1" || got.StorageClass != "STANDARD" {
		t.Errorf("location/storage class = %q/%q, want the fabricated values", got.Location, got.StorageClass)
	}

	if err := c.SetVersioning(ctx, "documents", false); err != nil {
		t.Fatalf("SetVersioning: %v", err)
	}
	after, err := c.GetBucket(ctx, "documents")
	if err != nil {
		t.Fatalf("re-reading bucket: %v", err)
	}
	if after.VersioningEnabled {
		t.Error("versioning was not suspended")
	}
}

func TestCreateBucketSwallowsAlreadyExists(t *testing.T) {
	_, host := newFakeGCS(t)
	c := connectFake(t, host)
	ctx := context.Background()

	if err := c.CreateBucket(ctx, Bucket{Name: "documents"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Concurrent provisioner runs must not fight each other.
	if err := c.CreateBucket(ctx, Bucket{Name: "documents"}); err != nil {
		t.Errorf("creating an existing bucket must not error, got %v", err)
	}
}

func TestCreateBucketPassesVersioningThrough(t *testing.T) {
	f, host := newFakeGCS(t)
	c := connectFake(t, host)

	if err := c.CreateBucket(context.Background(), Bucket{Name: "docs", VersioningEnabled: true}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	versioning, _ := f.bucket("docs")["versioning"].(map[string]any)
	if versioning == nil || versioning["enabled"] != true {
		t.Errorf("versioning not sent at creation: %v", f.bucket("docs"))
	}
}

func TestSetVersioningSurfacesTheFilesystemBackendError(t *testing.T) {
	f, host := newFakeGCS(t)
	c := connectFake(t, host)
	ctx := context.Background()

	if err := c.CreateBucket(ctx, Bucket{Name: "docs"}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// What fake-gcs-server answers on its default filesystem backend.
	f.failNext(http.StatusInternalServerError, "not implemented: fs storage type does not support versioning yet")

	err := c.SetVersioning(ctx, "docs", true)
	if err == nil {
		t.Fatal("expected the unsupported-versioning error to surface")
	}
	if !IsVersioningUnsupported(err) {
		t.Errorf("error should be classified as a versioning gap, got %v", err)
	}
}

func TestGetBucketPropagatesUnexpectedErrors(t *testing.T) {
	f, host := newFakeGCS(t)
	c := connectFake(t, host)

	f.failNext(http.StatusForbidden, `{"error":{"code":403,"message":"forbidden"}}`)

	_, err := c.GetBucket(context.Background(), "docs")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrBucketNotFound) {
		t.Error("a 403 must not be reported as a missing bucket")
	}
}
