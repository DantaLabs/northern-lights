package audit

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/dantalabs/northern-lights/internal/identity"
)

type AuditRange struct {
	FirstSeq  int64   `json:"first_seq"`
	LastSeq   int64   `json:"last_seq"`
	FirstHash string  `json:"first_hash"`
	LastHash  string  `json:"last_hash"`
	Entries   []Entry `json:"entries"`
}
type HashVersionCoverage struct {
	HashVersion          int   `json:"hash_version"`
	FirstSeq             int64 `json:"first_seq"`
	LastSeq              int64 `json:"last_seq"`
	TenantIdentityHashed bool  `json:"tenant_identity_hashed"`
}
type AuditCompleteness struct {
	Status                     string `json:"status"`
	ExpectedCount              int    `json:"expected_count"`
	IncludedCount              int    `json:"included_count"`
	OmittedCount               int    `json:"omitted_count"`
	ExpectedEventCount         int    `json:"expected_event_count"`
	IncludedEventCount         int    `json:"included_event_count"`
	OmissionCount              int    `json:"omission_count"`
	OmissionsRecorded          bool   `json:"omissions_recorded"`
	TerminalAnchor             string `json:"terminal_anchor"`
	TerminalAnchorVerified     bool   `json:"terminal_anchor_verified"`
	FinalRowDeletionDetectable bool   `json:"final_row_deletion_detectable"`
}
type AuditVerification struct {
	ChainVerified       bool                  `json:"chain_verified"`
	HashVersionCoverage []HashVersionCoverage `json:"hash_version_coverage"`
	Completeness        AuditCompleteness     `json:"completeness"`
	Caveats             []string              `json:"caveats"`
}

// RangeBounds returns the tenant-scoped current audit extent for evidence
// selection. It exposes no rows or filesystem details.
func (l *Log) RangeBounds(ctx context.Context) (int64, int64, error) {
	tenant := identity.StorageTenant(ctx)
	var first, last sql.NullInt64
	if err := l.db.QueryRowContext(ctx, `SELECT min(seq), max(seq) FROM audit_log WHERE tenant_id=?`, tenant).Scan(&first, &last); err != nil {
		return 0, 0, err
	}
	if !first.Valid || !last.Valid {
		return 0, 0, fmt.Errorf("audit: tenant has no rows")
	}
	return first.Int64, last.Int64, nil
}

func (l *Log) ExportRange(ctx context.Context, firstSeq, lastSeq int64) (AuditRange, error) {
	if firstSeq <= 0 || lastSeq < firstSeq || lastSeq-firstSeq > 10000 {
		return AuditRange{}, fmt.Errorf("audit: invalid bounded range")
	}
	tenant := identity.StorageTenant(ctx)
	rows, err := l.db.QueryContext(ctx, `SELECT `+auditColumns+` FROM audit_log WHERE tenant_id=? AND seq>=? AND seq<=? ORDER BY seq`, tenant, firstSeq, lastSeq)
	if err != nil {
		return AuditRange{}, err
	}
	defer func() { _ = rows.Close() }()
	result := AuditRange{FirstSeq: firstSeq, LastSeq: lastSeq, Entries: []Entry{}}
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return AuditRange{}, err
		}
		result.Entries = append(result.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return AuditRange{}, err
	}
	if len(result.Entries) == 0 {
		return AuditRange{}, fmt.Errorf("audit: range is empty")
	}
	result.FirstSeq, result.LastSeq = result.Entries[0].Seq, result.Entries[len(result.Entries)-1].Seq
	result.FirstHash, result.LastHash = result.Entries[0].Hash, result.Entries[len(result.Entries)-1].Hash
	return result, nil
}

// ExportLinked returns only audit rows linked to one subject. Tenant scope is
// applied before the linked-ID predicate, and no unrelated rows are exposed to
// fill sequence gaps.
func (l *Log) ExportLinked(ctx context.Context, auditIDs []string) (AuditRange, error) {
	if len(auditIDs) == 0 || len(auditIDs) > 1000 {
		return AuditRange{}, fmt.Errorf("audit: invalid linked audit set")
	}
	placeholders := make([]string, len(auditIDs))
	args := make([]any, 0, len(auditIDs)+1)
	args = append(args, identity.StorageTenant(ctx))
	for i, id := range auditIDs {
		if id == "" || len(id) > 128 {
			return AuditRange{}, fmt.Errorf("audit: invalid linked audit ID")
		}
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := l.db.QueryContext(ctx, `SELECT `+auditColumns+` FROM audit_log WHERE tenant_id=? AND audit_id IN (`+strings.Join(placeholders, ",")+") ORDER BY seq", args...)
	if err != nil {
		return AuditRange{}, err
	}
	defer func() { _ = rows.Close() }()
	result := AuditRange{Entries: []Entry{}}
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return AuditRange{}, err
		}
		result.Entries = append(result.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return AuditRange{}, err
	}
	if len(result.Entries) == 0 {
		return AuditRange{}, fmt.Errorf("audit: linked subject has no rows")
	}
	result.FirstSeq, result.LastSeq = result.Entries[0].Seq, result.Entries[len(result.Entries)-1].Seq
	result.FirstHash, result.LastHash = result.Entries[0].Hash, result.Entries[len(result.Entries)-1].Hash
	return result, nil
}

func (l *Log) VerifyRange(ctx context.Context, firstSeq, lastSeq int64) (AuditVerification, error) {
	rangeResult, err := l.ExportRange(ctx, firstSeq, lastSeq)
	if err != nil {
		return AuditVerification{}, err
	}
	coverage := map[int]*HashVersionCoverage{}
	verified := true
	previous := ""
	for index, entry := range rangeResult.Entries {
		item := coverage[entry.HashVersion]
		if item == nil {
			item = &HashVersionCoverage{HashVersion: entry.HashVersion, FirstSeq: entry.Seq, LastSeq: entry.Seq, TenantIdentityHashed: entry.HashVersion >= 2}
			coverage[entry.HashVersion] = item
		}
		if entry.Seq < item.FirstSeq {
			item.FirstSeq = entry.Seq
		}
		if entry.Seq > item.LastSeq {
			item.LastSeq = entry.Seq
		}
		if index > 0 && entry.PrevHash != previous {
			verified = false
		}
		var expected string
		switch entry.HashVersion {
		case 1:
			expected = entryHash(entry.PrevHash, entry.Ts.UTC().Format(timeFormat), entry.Actor, entry.Tool, entry.Action, entry.Target, entry.BeforeJSON, entry.AfterJSON, entry.WorkivaOpURL, entry.AuditID)
		case 2:
			expected = tenantEntryHash(entry.PrevHash, entry.TenantID, entry.Ts.UTC().Format(timeFormat), entry.Actor, entry.Tool, entry.Action, entry.Target, entry.BeforeJSON, entry.AfterJSON, entry.WorkivaOpURL, entry.AuditID)
		default:
			verified = false
		}
		if entry.Hash != expected {
			verified = false
		}
		previous = entry.Hash
	}
	versions := make([]HashVersionCoverage, 0, len(coverage))
	for _, item := range coverage {
		versions = append(versions, *item)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].HashVersion < versions[j].HashVersion })
	count := len(rangeResult.Entries)
	return AuditVerification{ChainVerified: verified, HashVersionCoverage: versions, Completeness: AuditCompleteness{Status: "unknown", ExpectedCount: count, IncludedCount: count, OmittedCount: 0, ExpectedEventCount: count, IncludedEventCount: count, OmissionCount: 0, OmissionsRecorded: true, TerminalAnchor: "unknown", TerminalAnchorVerified: false, FinalRowDeletionDetectable: false}, Caveats: []string{"tenant-scoped sequence gaps are not omissions", "final-row deletion requires an external terminal anchor"}}, nil
}

// VerifyLinked validates only the selected rows. Hashes are checked against
// their stored previous-hash pointer; sequence gaps are reported as an
// omission caveat rather than filled with unrelated tenant events.
func (l *Log) VerifyLinked(_ context.Context, entries []Entry) AuditVerification {
	coverage := map[int]*HashVersionCoverage{}
	verified := len(entries) > 0
	contiguous := true
	for i, entry := range entries {
		item := coverage[entry.HashVersion]
		if item == nil {
			item = &HashVersionCoverage{HashVersion: entry.HashVersion, FirstSeq: entry.Seq, LastSeq: entry.Seq, TenantIdentityHashed: entry.HashVersion >= 2}
			coverage[entry.HashVersion] = item
		}
		if entry.Seq < item.FirstSeq {
			item.FirstSeq = entry.Seq
		}
		if entry.Seq > item.LastSeq {
			item.LastSeq = entry.Seq
		}
		if i > 0 && entry.Seq != entries[i-1].Seq+1 {
			contiguous = false
		}
		var expected string
		switch entry.HashVersion {
		case 1:
			expected = entryHash(entry.PrevHash, entry.Ts.UTC().Format(timeFormat), entry.Actor, entry.Tool, entry.Action, entry.Target, entry.BeforeJSON, entry.AfterJSON, entry.WorkivaOpURL, entry.AuditID)
		case 2:
			expected = tenantEntryHash(entry.PrevHash, entry.TenantID, entry.Ts.UTC().Format(timeFormat), entry.Actor, entry.Tool, entry.Action, entry.Target, entry.BeforeJSON, entry.AfterJSON, entry.WorkivaOpURL, entry.AuditID)
		default:
			verified = false
		}
		if entry.Hash != expected {
			verified = false
		}
	}
	versions := make([]HashVersionCoverage, 0, len(coverage))
	for _, item := range coverage {
		versions = append(versions, *item)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].HashVersion < versions[j].HashVersion })
	caveats := []string{"subject selection excludes unlinked tenant events"}
	if !contiguous {
		caveats = append(caveats, "unlinked sequence rows are omitted; included-row hashes were checked individually")
	}
	return AuditVerification{ChainVerified: verified, HashVersionCoverage: versions, Completeness: AuditCompleteness{Status: "unknown", ExpectedCount: len(entries), IncludedCount: len(entries), ExpectedEventCount: len(entries), IncludedEventCount: len(entries), OmissionsRecorded: true, TerminalAnchor: "unknown"}, Caveats: caveats}
}
