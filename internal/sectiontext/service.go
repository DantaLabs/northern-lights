package sectiontext

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/recipe"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

type service struct {
	env     demoenv.Env
	recipes recipe.Service
	facts   recipe.FactStore
	initErr error
	now     func() time.Time
}

// New constructs the section text service on the shared database. Initialization
// errors are returned by Stage and Confirm because the scaffold signature cannot
// return an error. Empty recipe configuration is valid at server startup.
func New(env demoenv.Env, recipes recipe.Service, facts recipe.FactStore) Service {
	s := &service{env: env, recipes: recipes, facts: facts, now: time.Now}
	if env.DB != nil {
		s.initErr = sqlitedb.Migrate(context.Background(), env.DB, "section_text", []string{`
CREATE TABLE section_text_previews (
 id TEXT PRIMARY KEY, tenant TEXT NOT NULL, actor TEXT NOT NULL,
 permission TEXT NOT NULL, section_id TEXT NOT NULL, document_id TEXT NOT NULL,
 title TEXT NOT NULL, blocks TEXT NOT NULL, text_hash TEXT NOT NULL,
 token_digest TEXT NOT NULL, expires_at INTEGER NOT NULL, consumed INTEGER NOT NULL DEFAULT 0
);`})
	}
	return s
}

func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func randomID() string       { return rand.Text() }

func (s *service) Stage(ctx context.Context, actor, sectionID string, blocks []Block) (Preview, error) {
	if s.facts == nil {
		return Preview{}, refusal("Prepare the section first.")
	}
	facts, verbatim, err := s.facts.LatestFacts(ctx, actor, sectionID)
	if err != nil || len(facts) == 0 {
		return Preview{}, refusal("Prepare the section first.")
	}
	if actor == "" || actor == "unknown" {
		return Preview{}, errors.New("section text requires an authenticated actor")
	}
	var section recipe.SectionInfo
	if s.recipes != nil {
		for _, info := range s.recipes.Sections() {
			if info.ID == sectionID {
				section = info
				break
			}
		}
	}
	if section.ID == "" || section.DocumentID == "" {
		return Preview{}, errors.New("unknown section or missing report document")
	}
	if len(blocks) < 1 || len(blocks) > 20 {
		return Preview{}, errors.New("section text requires 1 to 20 blocks")
	}
	blocks = append([]Block(nil), blocks...)
	for i := range blocks {
		b := &blocks[i]
		if utf8.RuneCountInString(b.Text) > 4000 {
			return Preview{}, errors.New("section text blocks must have at most 4,000 characters")
		}
		switch b.Kind {
		case "verbatim":
			approved, found := "", false
			for _, v := range verbatim {
				if v.ID == b.VerbatimID {
					approved = v.Text
					found = true
					break
				}
			}
			b.Text = normalizeLines(b.Text)
			if !found || b.VerbatimID == "" || b.Text != normalizeLines(approved) {
				return Preview{}, &VerbatimError{ID: b.VerbatimID, Position: firstDifference(b.Text, normalizeLines(approved))}
			}
		case "narrative":
			if err := guardNumbers(b.Text, facts, section); err != nil {
				return Preview{}, err
			}
			b.Text = normalizeLines(b.Text)
		default:
			return Preview{}, errors.New("block kind must be narrative or verbatim")
		}
	}
	if s.initErr != nil {
		return Preview{}, s.initErr
	}
	if s.env.DB == nil {
		return Preview{}, errors.New("section text database is unavailable")
	}
	preview := Preview{ID: randomID(), Token: randomID(), SectionID: sectionID, SectionTitle: section.Title, Blocks: blocks, ExpiresAt: s.now().Add(10 * time.Minute)}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return Preview{}, err
	}
	_, err = s.env.DB.ExecContext(ctx, `INSERT INTO section_text_previews (id,tenant,actor,permission,section_id,document_id,title,blocks,text_hash,token_digest,expires_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, preview.ID, identity.StorageTenant(ctx), actor, string(identity.PermissionWorkivaWriteConfirm), sectionID, section.DocumentID, section.Title, string(encoded), digest(fullText(blocks)), digest(preview.Token), preview.ExpiresAt.UnixNano())
	return preview, err
}

func normalizeLines(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
func fullText(blocks []Block) string {
	parts := make([]string, len(blocks))
	for i, b := range blocks {
		parts[i] = b.Text
	}
	return strings.Join(parts, "\n")
}

// Confirm consumes a preview before the first provider mutation. Any uncertain
// outcome requires reconciliation; the consumed token can never resubmit it.
func (s *service) Confirm(ctx context.Context, actor, id, token string) (Result, error) {
	const permission = identity.PermissionWorkivaWriteConfirm
	if p, ok := identity.PrincipalFromContext(ctx); ok {
		if !p.HasPermission(permission) {
			return Result{}, errors.New("section text confirmation requires workiva.write.confirm")
		}
	} else if s.env.Cfg == nil || s.env.Cfg.AuthMode != config.AuthModeAPIKey {
		// API-key mode is the repository's explicit legacy authorization boundary.
		// Every other mode must supply a verified principal with this permission.
		return Result{}, errors.New("section text confirmation requires workiva.write.confirm")
	}
	if s.initErr != nil {
		return Result{}, s.initErr
	}
	if s.env.DB == nil {
		return Result{}, errors.New("section text database is unavailable")
	}
	writer, ok := workivaprovider.DocumentWriterFor(s.env.Client)
	if !ok || s.env.Audit == nil {
		return Result{}, errors.New("section text document writer or audit log is unavailable")
	}
	tx, err := s.env.DB.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var tenant, owner, required, sectionID, documentID, title, encoded, hash, tokenHash string
	var expires int64
	var consumed int
	err = tx.QueryRowContext(ctx, `SELECT tenant,actor,permission,section_id,document_id,title,blocks,text_hash,token_digest,expires_at,consumed FROM section_text_previews WHERE id=?`, id).Scan(&tenant, &owner, &required, &sectionID, &documentID, &title, &encoded, &hash, &tokenHash, &expires, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, errors.New("unknown section text confirmation token")
	}
	if err != nil {
		return Result{}, err
	}
	if tenant != identity.StorageTenant(ctx) || owner != actor || actor == "" {
		return Result{}, errors.New("section text confirmation belongs to another actor")
	}
	if subtle.ConstantTimeCompare([]byte(digest(token)), []byte(tokenHash)) != 1 {
		return Result{}, errors.New("unknown section text confirmation token")
	}
	if consumed != 0 {
		return Result{}, errors.New("section text confirmation token already used")
	}
	if s.now().UnixNano() >= expires {
		return Result{}, errors.New("section text confirmation token expired")
	}
	if required != string(permission) {
		return Result{}, errors.New("section text confirmation requires workiva.write.confirm")
	}
	var blocks []Block
	if err = json.Unmarshal([]byte(encoded), &blocks); err != nil {
		return Result{}, err
	}
	if digest(fullText(blocks)) != hash {
		return Result{}, errors.New("staged section text hash does not match")
	}
	updated, err := tx.ExecContext(ctx, `UPDATE section_text_previews SET consumed=1 WHERE id=? AND consumed=0`, id)
	if err != nil {
		return Result{}, err
	}
	n, err := updated.RowsAffected()
	if err != nil {
		return Result{}, err
	}
	if n != 1 {
		return Result{}, errors.New("section text confirmation token already used")
	}
	if err = tx.Commit(); err != nil {
		return Result{}, err
	}
	result := Result{SectionID: sectionID, DocumentID: documentID, Status: "reconciliation_required"}
	workivaID, writeErr := writer.EnsureSection(ctx, documentID, title)
	result.WorkivaSectionID = workivaID
	var op string
	paragraphs := strings.Split(fullText(blocks), "\n")
	if writeErr == nil {
		op, writeErr = writer.WriteSectionText(ctx, documentID, workivaID, paragraphs)
	}
	if writeErr == nil {
		var actual []string
		actual, writeErr = writer.ReadSectionText(ctx, documentID, workivaID)
		if writeErr == nil {
			result.Status = "mismatch"
			if equalParagraphs(actual, paragraphs) {
				result.Status = "completed"
			}
		}
	}
	ids := []string{}
	for _, b := range blocks {
		if b.Kind == "verbatim" {
			ids = append(ids, b.VerbatimID)
		}
	}
	payload, err := json.Marshal(struct {
		SectionID        string   `json:"section_id"`
		DocumentID       string   `json:"document_id"`
		WorkivaSectionID string   `json:"workiva_section_id"`
		TextHash         string   `json:"text_sha256"`
		VerbatimIDs      []string `json:"verbatim_ids"`
		Status           string   `json:"status"`
	}{sectionID, documentID, workivaID, hash, ids, result.Status})
	if err != nil {
		return result, err
	}
	auditCtx, cancelAudit := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelAudit()
	entry, err := s.env.Audit.Append(auditCtx, audit.Entry{Actor: actor, Tool: "workiva_confirm_section_text", Action: result.Status, Target: sectionID, AfterJSON: string(payload), WorkivaOpURL: op, AuditID: randomID()})
	if err != nil {
		result.Status = "reconciliation_required"
		return result, fmt.Errorf("reconciliation_required: record section text audit: %w", err)
	}
	result.AuditID = entry.AuditID
	// Provider errors are represented by the durable outcome, without resubmission.
	return result, nil
}

func equalParagraphs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
