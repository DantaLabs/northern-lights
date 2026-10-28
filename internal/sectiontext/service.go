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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/chart"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/recipe"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workiva"
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
			if b.Note != "" {
				return Preview{}, refusal("Notes belong on narrative paragraphs, not verbatim blocks.")
			}
			if !found || b.VerbatimID == "" || b.Text != normalizeLines(approved) {
				return Preview{}, &VerbatimError{ID: b.VerbatimID, Position: firstDifference(b.Text, normalizeLines(approved))}
			}
		case "narrative":
			if err := guardNumbers(b.Text, facts, section); err != nil {
				return Preview{}, err
			}
			if err := guardNote(b, facts, section); err != nil {
				return Preview{}, err
			}
			b.Text = normalizeLines(b.Text)
		case "chart":
			if i != len(blocks)-1 {
				return Preview{}, refusal("A chart must be the last block; it is inserted after the text.")
			}
			if _, err := chartRows(b.Text, facts); err != nil {
				return Preview{}, err
			}
		default:
			return Preview{}, errors.New("block kind must be narrative, verbatim or chart")
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

// NotePrefix marks a bookmark as the agent's reasoning, since the write itself
// is attributed to the signed-in analyst.
const NotePrefix = "NL AGENT: "

// maxNoteRunes keeps prefix plus note inside Workiva's 250-character bookmark name.
const maxNoteRunes = 240

func guardNote(b *Block, facts []recipe.Fact, section recipe.SectionInfo) error {
	b.Note = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(b.Note), strings.TrimSpace(NotePrefix)))
	if b.Note == "" {
		return nil
	}
	if utf8.RuneCountInString(b.Note) > maxNoteRunes || strings.ContainsAny(b.Note, "\r\n") {
		return refusal("A note must be one line of at most 240 characters.")
	}
	return guardNumbers(b.Note, facts, section)
}

type bookmarker interface {
	AddSectionBookmarks(ctx context.Context, documentID, sectionID string, marks []workiva.Bookmark) error
}

// addNotes bookmarks each noted narrative paragraph after the text is verified.
// The text write already completed, so a failure here is reported, not undone.
func (s *service) addNotes(ctx context.Context, writer any, documentID, sectionID string, blocks []Block) ([]string, string) {
	notes, marks, index := []string{}, []workiva.Bookmark{}, 0
	for _, b := range blocks {
		lines := strings.Split(b.Text, "\n")
		if b.Note != "" {
			last := lines[len(lines)-1]
			marks = append(marks, workiva.Bookmark{ParagraphIndex: index + len(lines) - 1, Length: utf8.RuneCountInString(last), Name: NotePrefix + b.Note})
			notes = append(notes, NotePrefix+b.Note)
		}
		index += len(lines)
	}
	if len(marks) == 0 {
		return notes, ""
	}
	bm, ok := writer.(bookmarker)
	if !ok || bm.AddSectionBookmarks(ctx, documentID, sectionID, marks) != nil {
		return notes, "failed"
	}
	return notes, "completed"
}

// textBlocks drops the chart block, which is drawn, not written as text.
func textBlocks(blocks []Block) []Block {
	out := make([]Block, 0, len(blocks))
	for _, b := range blocks {
		if b.Kind != "chart" {
			out = append(out, b)
		}
	}
	return out
}

type imageAppender interface {
	AppendSectionImage(ctx context.Context, documentID, sectionID, fileName string, png []byte, width, height int) error
}

// addChart draws the chart block from the facts prepared for this section and
// inserts it after the verified text. A failure is reported, never undone.
func (s *service) addChart(ctx context.Context, writer any, actor, sectionID, documentID, workivaID string, blocks []Block) string {
	last := blocks[len(blocks)-1]
	if last.Kind != "chart" {
		return ""
	}
	facts, _, err := s.facts.LatestFacts(ctx, actor, sectionID)
	if err != nil {
		return "failed"
	}
	rows, err := chartRows(last.Text, facts)
	if err != nil {
		return "failed"
	}
	section := recipe.SectionInfo{Title: sectionID}
	if s.recipes != nil {
		for _, info := range s.recipes.Sections() {
			if info.ID == sectionID {
				section = info
			}
		}
	}
	data, w, h, err := chart.Bars(section.Title, section.Period+" compared with "+section.PriorPeriod+"; bars scaled per indicator", section.Period, section.PriorPeriod, rows)
	appender, ok := writer.(imageAppender)
	if err != nil || !ok || appender.AppendSectionImage(ctx, documentID, workivaID, sectionID+"-chart.png", data, w, h) != nil {
		return "failed"
	}
	return "completed"
}

// maxChartRows keeps the chart readable on a report page.
const maxChartRows = 8

// chartRows resolves comma-separated field keys (or label words) to prepared
// facts. Every printed figure comes from the fact, as the number guard requires.
func chartRows(spec string, facts []recipe.Fact) ([]chart.Row, error) {
	var rows []chart.Row
	for _, want := range strings.Split(spec, ",") {
		want = strings.ToLower(strings.TrimSpace(want))
		if want == "" {
			continue
		}
		var match *recipe.Fact
		for i := range facts {
			if strings.ToLower(facts[i].Key) == want || strings.Contains(strings.ToLower(facts[i].Label), want) {
				match = &facts[i]
				break
			}
		}
		if match == nil {
			return nil, refusal(fmt.Sprintf("No prepared figure matches %q for the chart.", want))
		}
		rows = append(rows, chartRow(*match))
	}
	if len(rows) == 0 || len(rows) > maxChartRows {
		return nil, refusal(fmt.Sprintf("A chart needs 1 to %d fields.", maxChartRows))
	}
	return rows, nil
}

func chartRow(f recipe.Fact) chart.Row {
	value := func(v string) (float64, string) {
		n, err := strconv.ParseFloat(v, 64)
		if v == "" || err != nil {
			return 0, "missing"
		}
		return n, groupThousands(v) + " " + f.Unit
	}
	r := chart.Row{Label: f.Label}
	r.Current, r.CurrentText = value(f.Current)
	r.Prior, r.PriorText = value(f.Prior)
	if pct, err := strconv.ParseFloat(f.ChangePct, 64); err == nil && f.Material {
		r.Tag = strconv.FormatFloat(pct, 'f', 1, 64) + "%, material"
		if pct > 0 {
			r.Tag = "+" + r.Tag
		}
	}
	return r
}

// groupThousands adds comma separators to a canonical decimal string.
func groupThousands(v string) string {
	sign, intPart, frac := "", v, ""
	if strings.HasPrefix(intPart, "-") {
		sign, intPart = "-", intPart[1:]
	}
	if i := strings.IndexByte(intPart, '.'); i >= 0 {
		intPart, frac = intPart[:i], intPart[i:]
	}
	for i := len(intPart) - 3; i > 0; i -= 3 {
		intPart = intPart[:i] + "," + intPart[i:]
	}
	return sign + intPart + frac
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
	paragraphs := strings.Split(fullText(textBlocks(blocks)), "\n")
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
	notes := []string{}
	if result.Status == "completed" {
		notes, result.Notes = s.addNotes(ctx, writer, documentID, workivaID, textBlocks(blocks))
		result.Chart = s.addChart(ctx, writer, actor, sectionID, documentID, workivaID, blocks)
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
		Notes            []string `json:"agent_notes,omitempty"`
		NotesStatus      string   `json:"agent_notes_status,omitempty"`
		ChartStatus      string   `json:"chart_status,omitempty"`
	}{sectionID, documentID, workivaID, hash, ids, result.Status, notes, result.Notes, result.Chart})
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
