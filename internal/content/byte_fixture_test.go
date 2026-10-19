package content

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Exercise the exact handoff fixtures rather than substituting a model answer
// or filename for the document bytes that reach private persistence.
func TestByteIntakeFixturesPersistExactEncryptedSourcesAndReplay(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "wave4", "fixtures")
	manifestRaw, err := os.ReadFile(filepath.Join(dir, "byte-intake-expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Sources map[string]struct {
			SHA256 string `json:"sha256"`
			Length int    `json:"byte_length"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"paste", "pdf"} {
		t.Run(kind, func(t *testing.T) {
			store, db, resolver, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
			if err := store.SetContentStorageKey("fixture-source-key", bytes.Repeat([]byte{0x37}, 32)); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "byte-intake-"+kind+".json"))
			if err != nil {
				t.Fatal(err)
			}
			projection, err := DecodeContentIngest(raw)
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewIntakeService(store, resolver)
			if err != nil {
				t.Fatal(err)
			}
			ctx := intakeContext()
			now := time.Now().UTC()
			created, err := service.IngestPublic(ctx, raw, "fixture-byte-intake-audit", now)
			if err != nil {
				t.Fatal(err)
			}
			want := manifest.Sources[kind]
			if created.PublicProjection == nil || created.SourceSHA256 != want.SHA256 || created.PublicProjection.Source.ByteLength != want.Length {
				t.Fatal("saved source hash/length differs from the original fixture bytes")
			}
			source, err := store.GetContentSource(ctx, created.SourceArtifactID)
			if err != nil || !bytes.Equal(source.SourceBytes, projection.Input.SourceBytes) {
				t.Fatalf("owned decrypted source differs from supplied bytes: %v", err)
			}
			var saved []byte
			if err := db.QueryRow(`SELECT source_bytes FROM assurance_content_sources WHERE source_artifact_id=?`, created.SourceArtifactID).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(saved, projection.Input.SourceBytes) || bytes.Contains(saved, []byte("12.4")) {
				t.Fatal("source bytes were stored without encryption")
			}
			service.resolver = nil
			store.SetAuditLog(nil)
			replayed, err := service.IngestPublic(ctx, raw, "", now.Add(time.Minute))
			if err != nil || !reflect.DeepEqual(created, replayed) {
				t.Fatalf("sealed replay changed source bindings or required execution dependencies: %v", err)
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM assurance_content_sources`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("replay created another source: count=%d err=%v", count, err)
			}
		})
	}
}
