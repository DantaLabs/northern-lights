package assurance

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestStageBundleAcceptsOnlyExplicitTrustedHistoricalSigners(t *testing.T) {
	for _, tc := range []struct {
		name      string
		trust     string
		tamper    bool
		empty     bool
		wantStage bool
	}{
		{name: "missing legacy keys reject rotation"},
		{name: "unknown legacy key rejects rotation", trust: "unknown"},
		{name: "pinned v1 and v2 signers allow v3", trust: "known", wantStage: true},
		{name: "tampered history rejects despite pinned keys", trust: "known", tamper: true},
		{name: "empty candidate rejects history with missing legacy keys", empty: true},
		{name: "empty candidate rejects history with unknown legacy keys", trust: "unknown", empty: true},
		{name: "empty candidate rejects tampered history despite pinned keys", trust: "known", tamper: true, empty: true},
		{name: "empty candidate accepts explicitly pinned history without changing it", trust: "known", empty: true, wantStage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, db, keys := seedThreeKeyHistory(t)
			stageStore, err := NewWithDB(db)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.trust {
			case "known":
				input := []ed25519.PublicKey{keys.v1, keys.v2}
				if err := stageStore.SetContentPolicyHistoryKeys(input); err != nil {
					t.Fatalf("configure known signer history: %v", err)
				}
				input[0][0] ^= 0xff
				input[1][0] ^= 0xff
			case "unknown":
				if err := stageStore.SetContentPolicyHistoryKeys([]ed25519.PublicKey{keys.unknown}); err != nil {
					t.Fatalf("configure unknown signer: %v", err)
				}
			}
			if tc.tamper {
				if _, err := db.Exec(`UPDATE assurance_bundle_candidates SET signature_hex=? WHERE tenant_id=? AND bundle_version=1`, strings.Repeat("0", ed25519.SignatureSize*2), testTenant); err != nil {
					t.Fatal(err)
				}
			}
			before := bundleHistorySnapshot(t, db)
			candidate := historyBundle(t, keys.priv3, 3, !tc.empty)
			err = stageStore.StageBundle(assuranceContext(), candidate)
			if tc.wantStage {
				if err != nil {
					t.Fatalf("StageBundle with explicit historical keys: %v", err)
				}
			} else if err == nil {
				t.Fatal("StageBundle accepted untrusted or tampered signed history")
			}
			after := bundleHistorySnapshot(t, db)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("staging changed existing signed history\nbefore=%#v\nafter=%#v", before, after)
			}
			var staged int
			if err := db.QueryRow(`SELECT count(*) FROM assurance_bundle_candidates WHERE tenant_id=? AND bundle_version=3`, testTenant).Scan(&staged); err != nil {
				t.Fatal(err)
			}
			if (staged == 1) != tc.wantStage {
				t.Fatalf("candidate rows=%d want staged=%t", staged, tc.wantStage)
			}
		})
	}
}

func TestContentPolicyHistoryKeyringIsBoundedCopiedAndImmutable(t *testing.T) {
	store, _ := openTestStore(t)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetContentPolicyHistoryKeys([]ed25519.PublicKey{public}); err != nil {
		t.Fatalf("set history keyring: %v", err)
	}
	if err := store.SetContentPolicyHistoryKeys([]ed25519.PublicKey{public}); err == nil {
		t.Fatal("history keyring was mutable after initial configuration")
	}
	tooManyStore, _ := openTestStore(t)
	tooMany := make([]ed25519.PublicKey, maxContentPolicyHistoryKeys+1)
	for i := range tooMany {
		key, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tooMany[i] = key
	}
	if err := tooManyStore.SetContentPolicyHistoryKeys(tooMany); err == nil {
		t.Fatal("oversized history keyring was accepted")
	}
	malformedStore, _ := openTestStore(t)
	if err := malformedStore.SetContentPolicyHistoryKeys([]ed25519.PublicKey{{1}}); err == nil {
		t.Fatal("malformed historical public key was accepted")
	}
	duplicateStore, _ := openTestStore(t)
	if err := duplicateStore.SetContentPolicyHistoryKeys([]ed25519.PublicKey{public, public}); err == nil {
		t.Fatal("duplicate historical public keys were accepted")
	}
}

type historyKeys struct {
	v1, v2, v3, unknown ed25519.PublicKey
	priv1, priv2, priv3 ed25519.PrivateKey
}

func seedThreeKeyHistory(t *testing.T) (*Store, *sql.DB, historyKeys) {
	t.Helper()
	store, db := openTestStore(t)
	keys := historyKeys{}
	for _, pair := range []struct {
		public  *ed25519.PublicKey
		private *ed25519.PrivateKey
	}{{&keys.v1, &keys.priv1}, {&keys.v2, &keys.priv2}, {&keys.v3, &keys.priv3}, {&keys.unknown, new(ed25519.PrivateKey)}} {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		*pair.public, *pair.private = public, private
	}
	first := historyBundle(t, keys.priv1, 1, false)
	if err := store.StageBundle(assuranceContext(), first); err != nil {
		t.Fatalf("stage v1: %v", err)
	}
	if err := store.RequestActivation(assuranceContext(), "history-bundle", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(assuranceContext(), testTenant, keys.v1); err != nil {
		t.Fatalf("activate v1: %v", err)
	}
	if err := store.SetContentPolicyHistoryKeys([]ed25519.PublicKey{keys.v1}); err != nil {
		t.Fatal(err)
	}
	second := historyBundle(t, keys.priv2, 2, false)
	if err := store.StageBundle(assuranceContext(), second); err != nil {
		t.Fatalf("stage v2 with explicitly pinned v1 signer: %v", err)
	}
	if err := store.RequestActivation(assuranceContext(), "history-bundle", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(assuranceContext(), testTenant, keys.v2); err != nil {
		t.Fatalf("activate v2: %v", err)
	}
	return store, db, keys
}

func historyBundle(t *testing.T, private ed25519.PrivateKey, version int, addIntake bool) *ValidatedBundle {
	t.Helper()
	raw, _ := signedBundle(t, private, func(b *Bundle) {
		b.BundleID = "history-bundle"
		b.BundleVersion = version
		if addIntake {
			b.ContentAccessPolicies = []ContentAccessPolicy{{PolicyID: "rotation-intake", Revision: 1,
				State: "active", TenantID: testTenant, ActorObjectID: contentTestActor,
				Capabilities: []string{"content.ingest"}, ResourceIDs: []string{"resource-1"},
				ValidUntil: "2027-01-01T00:00:00Z"}}
			b.ContentRetentionPolicies = []ContentRetentionPolicy{{PolicyID: "rotation-retention", Revision: 1,
				State: "active", SourceLifetimeSeconds: 86400, DraftLifetimeSeconds: 43200,
				IntentLifetimeSeconds: 3600, IdempotencyLifetimeSeconds: 7200, RecoveryLifetimeSeconds: 7200}}
		}
	})
	var parsed Bundle
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalJSON(parsed)
	if err != nil {
		t.Fatal(err)
	}
	public := private.Public().(ed25519.PublicKey)
	validated, err := ValidateBundle(canonical, ed25519.Sign(private, canonical), testTenant, public)
	if err != nil {
		t.Fatalf("validate v%d bundle: %v", version, err)
	}
	return validated
}

func bundleHistorySnapshot(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT 'candidate',bundle_id,bundle_version,bundle_json,signature_hex,content_hash FROM assurance_bundle_candidates WHERE tenant_id=? AND bundle_version<3
		UNION ALL SELECT 'active',bundle_id,bundle_version,bundle_json,signature_hex,content_hash FROM assurance_active_bundles WHERE tenant_id=? AND singleton=1
		ORDER BY 1,2,3`, testTenant, testTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close bundle history rows: %v", err)
		}
	}()
	var out []string
	for rows.Next() {
		var kind, id, raw, signature, hash string
		var version int
		if err := rows.Scan(&kind, &id, &version, &raw, &signature, &hash); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%d|%s|%s|%s", kind, id, version, raw, signature, hash))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
