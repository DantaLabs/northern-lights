package assurance

import "testing"

func TestManifestPackageHashUsesCanonicalPayloadSet(t *testing.T) {
	a := EvidenceArtifact{ArtifactID: "a", Name: "subject.json", MediaType: "application/json", StorageRef: "opaque", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ByteCount: 1}
	m := EvidenceManifest{ManifestID: "m", ManifestVersion: 2, SubjectKind: "snapshot", SubjectID: "s", Artifacts: []EvidenceArtifact{a}}
	m.PackageHash = digestHex(HashBytes(packageHash(m.Artifacts)))
	if err := VerifyManifest(m, []EvidenceArtifact{a}); err != nil {
		t.Fatal(err)
	}
}
