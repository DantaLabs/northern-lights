package assurance

import "testing"

func TestExportProfileRejectsInvalidVocabulariesAndDuplicateSubjects(t *testing.T) {
	base := ExportProfile{ProfileID: "p", Revision: 1, Status: "active", PermittedSubjects: []string{"snapshot", "comparison"}, RedactionProfile: "standard", RetentionClass: "long_term", MaxRows: 10, MaxBytes: 100, DeliveryPolicy: "opaque_reference"}
	for name, mutate := range map[string]func(*ExportProfile){
		"duplicate subject":   func(p *ExportProfile) { p.PermittedSubjects = []string{"snapshot", "snapshot"} },
		"unsupported subject": func(p *ExportProfile) { p.PermittedSubjects = []string{"transfer"} },
		"invalid retention":   func(p *ExportProfile) { p.RetentionClass = "" },
		"invalid delivery":    func(p *ExportProfile) { p.DeliveryPolicy = "url" },
		"zero rows":           func(p *ExportProfile) { p.MaxRows = 0 },
		"zero bytes":          func(p *ExportProfile) { p.MaxBytes = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			profile := base
			mutate(&profile)
			if err := ValidateExportProfile(profile); err == nil {
				t.Fatal("invalid export profile was accepted")
			}
		})
	}
}
