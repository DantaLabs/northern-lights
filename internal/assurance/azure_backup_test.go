package assurance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

type backupObject struct {
	body []byte
	etag string
	meta http.Header
}
type backupFake struct {
	mu                                                         sync.Mutex
	objects                                                    map[string]backupObject
	staged                                                     map[string][]byte
	puts                                                       []string
	failManifest, badGet, omitGet, failGet, delay, changedETag bool
	containerPropertiesStatus                                  int
	containerPublicAccess                                      string
	containerPublicAccessSet                                   bool
}

func newBackupFake() *backupFake {
	return &backupFake{objects: map[string]backupObject{}, staged: map[string][]byte{}}
}
func (f *backupFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Query().Get("restype") == "container" {
		f.mu.Lock()
		status, access, accessSet := f.containerPropertiesStatus, f.containerPublicAccess, f.containerPublicAccessSet
		f.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		if accessSet {
			w.Header().Set("x-ms-blob-public-access", access)
		}
		w.Header().Set("ETag", `"container-policy"`)
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(status)
		return
	}
	if f.delay {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
	name := r.URL.Path
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		if f.failGet {
			w.WriteHeader(503)
			return
		}
		obj, ok := f.objects[name]
		if !ok || f.omitGet {
			w.WriteHeader(404)
			return
		}
		body := obj.body
		if f.badGet {
			body = append(append([]byte{}, body...), 1)
		}
		for k, v := range obj.meta {
			w.Header()[k] = v
		}
		etag := obj.etag
		if f.changedETag {
			etag = `"changed"`
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(200)
		_, _ = w.Write(body)
		return
	}
	if r.Method != http.MethodPut {
		w.WriteHeader(405)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	if r.URL.Query().Get("comp") == "block" {
		f.staged[name] = append([]byte{}, body...)
		w.Header().Set("ETag", `"staged"`)
		w.WriteHeader(201)
		return
	}
	f.puts = append(f.puts, r.Header.Get("If-None-Match"))
	if f.failManifest && strings.HasSuffix(name, "manifest.json") {
		w.WriteHeader(503)
		return
	}
	if _, exists := f.objects[name]; exists {
		w.WriteHeader(412)
		return
	}
	if r.Header.Get("If-None-Match") != "*" {
		w.WriteHeader(400)
		return
	}
	if staged, ok := f.staged[name]; ok {
		body = staged
	}
	meta := http.Header{}
	for k, v := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-ms-meta-") {
			meta[k] = append([]string{}, v...)
		}
	}
	etag := `"` + strconv.Itoa(len(f.objects)+1) + `"`
	f.objects[name] = backupObject{body: append([]byte{}, body...), etag: etag, meta: meta}
	w.Header().Set("ETag", etag)
	w.WriteHeader(201)
}

func TestAzureBackupTransportRequiresObservablePrivateContainer(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		access        string
		accessPresent bool
		wantErr       bool
	}{
		{name: "private absent access header", status: http.StatusOK},
		{name: "public blob access", status: http.StatusOK, access: "blob", accessPresent: true, wantErr: true},
		{name: "public container access", status: http.StatusOK, access: "container", accessPresent: true, wantErr: true},
		{name: "unknown access policy", status: http.StatusOK, access: "unexpected", accessPresent: true, wantErr: true},
		{name: "unobservable properties", status: http.StatusServiceUnavailable, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/private" || r.URL.Query().Get("restype") != "container" {
					t.Errorf("unexpected container-policy request: %s %s", r.Method, r.URL.String())
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("ETag", `"container-policy"`)
				w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
				if tc.accessPresent {
					w.Header().Set("x-ms-blob-public-access", tc.access)
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
			if err != nil {
				t.Fatal(err)
			}
			transport, err := newAzureBackupTransport(client, AzureBackupConfig{ContainerName: "private", TenantID: testTenant, EnvironmentDigest: "sandbox-a", Timeout: time.Second, MaxDatabaseBytes: 32 << 20})
			if err != nil {
				t.Fatal(err)
			}
			err = transport.ValidatePrivateContainer(context.Background())
			if tc.wantErr && err == nil {
				t.Fatal("unsafe or unobservable container policy was accepted")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("private container rejected: %v", err)
			}
		})
	}
}
func blobFixture(t *testing.T) (*AzureBackupTransport, *backupFake, string, BackupEnvelope, RestoreOptions, func()) {
	t.Helper()
	db, pub, _ := recoveryDB(t)
	key := SigningKey{HMAC: []byte("fake-http-private-signing-key")}
	dir := filepath.Join(t.TempDir(), "envelope")
	env, err := CreateBackup(context.Background(), db, dir, BackupOptions{TenantID: testTenant, SigningKey: key, CheckpointPublicKey: pub})
	if err != nil {
		t.Fatal(err)
	}
	fake := newBackupFake()
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	// RestoreEnvelope's bound includes verification plus SQLite migration and
	// readiness checks. Match the production operation budget here; keeping
	// this at one second makes the race-instrumented migration flaky under
	// parallel package load. Deadline behavior is exercised explicitly below.
	transport, err := newAzureBackupTransport(client, AzureBackupConfig{ContainerName: "private", TenantID: testTenant, EnvironmentDigest: "sandbox-a", Timeout: 90 * time.Second, MaxDatabaseBytes: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	opts := RestoreOptions{ExpectedTenantID: testTenant, SigningKey: key, CheckpointPublicKey: pub, MinimumCreatedAt: time.Now().Add(-time.Minute), ExpectedSchemaMarkers: env.Manifest.SchemaMarkers}
	return transport, fake, dir, env, opts, func() { server.Close(); _ = db.Close() }
}
func TestBlobBackupCreateOnlyReadbackRestoreAndConcurrentRace(t *testing.T) {
	a, f, dir, env, opts, done := blobFixture(t)
	defer done()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := a.UploadEnvelope(context.Background(), dir, opts); errs <- err }()
	}
	wg.Wait()
	close(errs)
	success, failed := 0, 0
	var results []error
	for err := range errs {
		results = append(results, err)
		if err == nil {
			success++
		} else {
			failed++
		}
	}
	if success != 1 || failed != 1 {
		t.Fatalf("race: success=%d failure=%d errors=%v", success, failed, results)
	}
	f.mu.Lock()
	for _, v := range f.puts {
		if v != "*" {
			t.Fatalf("unsafe create header %q", v)
		}
	}
	if len(f.objects) != 2 {
		t.Fatalf("objects=%d", len(f.objects))
	}
	f.mu.Unlock()
	dest := filepath.Join(t.TempDir(), "restored.db")
	result, err := a.RestoreEnvelope(context.Background(), env.Manifest.EnvelopeID, dest, opts)
	if err != nil || !result.Ready {
		t.Fatalf("restore: %v %+v", err, result)
	}
	if result.DatabaseSHA256 != env.Manifest.DatabaseSHA256 {
		t.Fatal("wrong database")
	}
	if _, err := a.UploadEnvelope(context.Background(), dir, opts); err == nil {
		t.Fatal("adopted existing object")
	}
	if _, err := a.RestoreEnvelope(context.Background(), env.Manifest.EnvelopeID, dest, opts); !errors.Is(err, ErrBackupDestinationNotEmpty) {
		t.Fatalf("nonempty: %v", err)
	}
}
func TestBlobBackupPartialMissingMismatchedAlteredAndBinding(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		a, f, dir, env, opts, done := blobFixture(t)
		defer done()
		f.failManifest = true
		if _, err := a.UploadEnvelope(context.Background(), dir, opts); err == nil {
			t.Fatal("partial upload accepted")
		}
		f.mu.Lock()
		n := len(f.objects)
		f.mu.Unlock()
		if n != 1 {
			t.Fatalf("partial objects=%d", n)
		}
		if _, err := a.RestoreEnvelope(context.Background(), env.Manifest.EnvelopeID, filepath.Join(t.TempDir(), "new.db"), opts); err == nil {
			t.Fatal("partial restore")
		}
		f.failManifest = false
		if _, err := a.UploadEnvelope(context.Background(), dir, opts); err == nil {
			t.Fatal("unsafe adoption")
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		a, f, dir, env, opts, done := blobFixture(t)
		defer done()
		if _, err := a.UploadEnvelope(context.Background(), dir, opts); err != nil {
			t.Fatal(err)
		}
		for _, fault := range []string{"missing-db", "corrupt-db", "alter-manifest", "wrong-env", "missing-manifest"} {
			f.mu.Lock()
			saved := make(map[string]backupObject, len(f.objects))
			for k, v := range f.objects {
				saved[k] = v
			}
			for k, v := range f.objects {
				switch fault {
				case "missing-db":
					if strings.HasSuffix(k, "database.sqlite") {
						delete(f.objects, k)
					}
				case "corrupt-db":
					if strings.HasSuffix(k, "database.sqlite") {
						v.body = append([]byte{}, v.body...)
						v.body[0] ^= 1
						f.objects[k] = v
					}
				case "alter-manifest":
					if strings.HasSuffix(k, "manifest.json") {
						v.body = append([]byte{}, v.body...)
						v.body[0] ^= 1
						f.objects[k] = v
					}
				case "wrong-env":
					v.meta.Set("X-Ms-Meta-Nlenvironment", "other")
					f.objects[k] = v
				case "missing-manifest":
					if strings.HasSuffix(k, "manifest.json") {
						delete(f.objects, k)
					}
				}
			}
			f.mu.Unlock()
			dst := filepath.Join(t.TempDir(), fault+".db")
			if _, err := a.RestoreEnvelope(context.Background(), env.Manifest.EnvelopeID, dst, opts); err == nil {
				t.Fatalf("%s restored", fault)
			}
			if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s installed database: %v", fault, err)
			}
			f.mu.Lock()
			f.objects = saved
			f.mu.Unlock()
		}
	})
	t.Run("tenant", func(t *testing.T) {
		a, f, dir, env, opts, done := blobFixture(t)
		defer done()
		opts.ExpectedTenantID = "other"
		if _, err := a.UploadEnvelope(context.Background(), dir, opts); !errors.Is(err, ErrBackupWrongTenant) {
			t.Fatalf("upload %v", err)
		}
		if _, err := a.RestoreEnvelope(context.Background(), env.Manifest.EnvelopeID, filepath.Join(t.TempDir(), "new.db"), opts); !errors.Is(err, ErrBackupWrongTenant) {
			t.Fatalf("restore %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.puts) != 0 {
			t.Fatal("tenant mismatch hit network")
		}
	})
}
func TestBlobBackupUploadFailsClosedOnReadbackFaults(t *testing.T) {
	for _, fault := range []string{"bytes", "etag", "network"} {
		t.Run(fault, func(t *testing.T) {
			a, f, dir, _, opts, done := blobFixture(t)
			defer done()
			switch fault {
			case "bytes":
				f.badGet = true
			case "etag":
				f.changedETag = true
			case "network":
				f.failGet = true
			}
			if _, err := a.UploadEnvelope(context.Background(), dir, opts); err == nil {
				t.Fatal("unverified upload sealed")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.objects) != 1 {
				t.Fatalf("unverified upload objects=%d", len(f.objects))
			}
		})
	}
}

func TestBlobBackupRollbackAndNetworkFailures(t *testing.T) {
	a, f, dir, env, opts, done := blobFixture(t)
	defer done()
	if _, err := a.UploadEnvelope(context.Background(), dir, opts); err != nil {
		t.Fatal(err)
	}
	for _, modify := range []func(*RestoreOptions){func(o *RestoreOptions) { o.MinimumCreatedAt = time.Now().Add(time.Hour) }, func(o *RestoreOptions) { o.MinimumAuditSequence = 100 }, func(o *RestoreOptions) { o.ExpectedDatabaseSHA256 = strings.Repeat("0", 64) }} {
		changed := opts
		modify(&changed)
		dest := filepath.Join(t.TempDir(), "stale.db")
		if _, err := a.RestoreEnvelope(context.Background(), env.Manifest.EnvelopeID, dest, changed); !errors.Is(err, ErrBackupRollback) {
			t.Fatalf("stale: %v", err)
		}
	}
	f.failGet = true
	if _, err := a.RestoreEnvelope(context.Background(), env.Manifest.EnvelopeID, filepath.Join(t.TempDir(), "network.db"), opts); err == nil {
		t.Fatal("network fault accepted")
	}
	f.failGet = false
	f.delay = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := a.RestoreEnvelope(ctx, env.Manifest.EnvelopeID, filepath.Join(t.TempDir(), "timeout.db"), opts); err == nil {
		t.Fatal("timeout accepted")
	}
}
