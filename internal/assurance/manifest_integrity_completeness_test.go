package assurance

import "testing"

func TestManifestRejectsTamperedHash(t *testing.T) {
	a := EvidenceArtifact{ArtifactID: "a", Name: "subject.json", MediaType: "application/json", StorageRef: "opaque", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ByteCount: 1}
	m := EvidenceManifest{ManifestID: "m", ManifestVersion: 2, SubjectKind: "snapshot", SubjectID: "s", Artifacts: []EvidenceArtifact{a}, PackageHash: digestHex(HashBytes(packageHash([]EvidenceArtifact{a})))}
	a.SHA256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := VerifyManifest(m, []EvidenceArtifact{a}); err == nil {
		t.Fatal("tampered artifact was accepted")
	}
}
