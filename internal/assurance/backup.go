package assurance

// This file deliberately contains only local recovery primitives.  It does
// not know about Azure Blob Storage or the transfer fence protocol.  A caller
// must keep the returned envelope private and immutable until a separately
// implemented durable-object gate has sealed it.

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
)

const (
	backupFormatVersion = 1
	backupDatabaseName  = "database.sqlite"
	backupManifestName  = "manifest.json"
	backupDomain        = "northern-lights/sqlite-backup/v1\x00"
	backupTimeFormat    = time.RFC3339Nano
)

var (
	ErrBackupDraining            = errors.New("assurance: database is draining for backup")
	ErrBackupIncomplete          = errors.New("assurance: backup envelope is incomplete")
	ErrBackupTampered            = errors.New("assurance: backup verification failed")
	ErrBackupWrongTenant         = errors.New("assurance: backup tenant mismatch")
	ErrBackupWrongKey            = errors.New("assurance: backup signature verification failed")
	ErrBackupRollback            = errors.New("assurance: backup rollback rejected")
	ErrBackupDestinationNotEmpty = errors.New("assurance: restore destination is not empty")
)

// DrainGate is the process-local admission gate used by all materializing
// writers around the shared SQLite handle.  A drain closes admission first,
// then waits for writers that already entered to finish.  New writers fail
// with ErrBackupDraining while a backup is in progress.
//
// The gate is intentionally explicit: *sql.DB does not expose a hook that can
// distinguish reads from writes.  Production write paths must use EnterWrite
// (or WithWrite on BackupManager), rather than bypassing the gate with a raw
// Exec call.
type DrainGate struct {
	mu       sync.Mutex
	active   int
	draining bool
	changed  chan struct{}
}

func NewDrainGate() *DrainGate {
	return &DrainGate{changed: make(chan struct{})}
}

func (g *DrainGate) signalLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// EnterWrite admits one write-side operation and returns its release function.
func (g *DrainGate) EnterWrite(ctx context.Context) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining {
		return nil, ErrBackupDraining
	}
	g.active++
	var once sync.Once
	return func() { once.Do(func() { g.leaveWrite() }) }, nil
}

// Draining reports the admission state. It is useful for readiness and for
// tests that coordinate a fault window without touching the database.
func (g *DrainGate) Draining() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.draining
}

func (g *DrainGate) leaveWrite() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active > 0 {
		g.active--
	}
	if g.active == 0 {
		g.signalLocked()
	}
}

// BeginDrain prevents new writes and waits for admitted writes to leave.
// The returned function reopens admission.  A canceled drain is rolled back.
func (g *DrainGate) BeginDrain(ctx context.Context) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	g.mu.Lock()
	if g.draining {
		g.mu.Unlock()
		return nil, ErrBackupDraining
	}
	g.draining = true
	for g.active != 0 {
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.draining = false
			g.signalLocked()
			g.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
			g.mu.Lock()
		}
	}
	g.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { g.EndDrain() }) }, nil
}

func (g *DrainGate) EndDrain() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.draining {
		g.draining = false
		g.signalLocked()
	}
	g.mu.Unlock()
}

// SigningKey selects exactly one manifest signature scheme. HMAC is useful
// for a symmetric deployment secret; Ed25519 is preferred when verification
// happens in a process that should not hold the signing secret.
type SigningKey struct {
	HMAC           []byte
	Ed25519Private ed25519.PrivateKey
	Ed25519Public  ed25519.PublicKey
}

type BackupPhase string

const (
	BackupPhaseDatabaseComplete     BackupPhase = "database_complete"
	BackupPhaseManifestBeforeCommit BackupPhase = "manifest_before_commit"
	BackupPhaseEnvelopeComplete     BackupPhase = "envelope_complete"
)

// BackupOptions controls the local, signed backup. TenantID is mandatory and
// is bound into the signed manifest. ExpectedSchemaMarkers is optional, but
// when supplied every marker must match exactly.
type BackupOptions struct {
	TenantID               string
	SigningKey             SigningKey
	Gate                   *DrainGate
	ExpectedSchemaMarkers  map[string]int
	CheckpointPublicKey    ed25519.PublicKey
	CheckpointID           string
	RequireAuditCheckpoint bool
	// FenceHighWater is an independently captured Azure Blob last-modified
	// watermark. It is signed into the manifest and is never derived from the
	// audit sequence.
	FenceHighWater *BackupFenceHighWater
	Fault          func(BackupPhase) error
}

type BackupEnvelope struct {
	Directory       string
	DatabasePath    string
	ManifestPath    string
	Manifest        BackupManifest
	ManifestHash    string
	SignatureScheme string
}

// BackupManifest is the signed, deterministic description of the exact
// database bytes in database.sqlite. Maps are marshalled by encoding/json in
// sorted key order, so the bytes signed here are stable.
type BackupManifest struct {
	FormatVersion          int                   `json:"format_version"`
	EnvelopeID             string                `json:"envelope_id"`
	Complete               bool                  `json:"complete"`
	CreatedAt              string                `json:"created_at"`
	TenantID               string                `json:"tenant_id"`
	DatabaseFile           string                `json:"database_file"`
	DatabaseSHA256         string                `json:"database_sha256"`
	DatabaseBytes          int64                 `json:"database_bytes"`
	SchemaMarkers          map[string]int        `json:"schema_markers"`
	TenantRowCounts        map[string]int64      `json:"tenant_row_counts"`
	TableRowCounts         map[string]int64      `json:"table_row_counts"`
	Audit                  BackupAudit           `json:"audit"`
	EvidenceManifestHashes map[string]string     `json:"evidence_manifest_hashes"`
	FenceHighWater         *BackupFenceHighWater `json:"fence_high_water,omitempty"`
}

// BackupFenceHighWater is the signed restore boundary for external fences.
// CapturedAt is an Azure Blob Last-Modified watermark, not an audit sequence
// or a local wall-clock approximation.
type BackupFenceHighWater struct {
	TenantDigest      string `json:"tenant_digest"`
	EnvironmentDigest string `json:"environment_digest"`
	CapturedAt        string `json:"captured_at"`
	Authority         string `json:"authority"`
}

type BackupAudit struct {
	ChainVerified       bool                          `json:"chain_verified"`
	RowCount            int64                         `json:"row_count"`
	FirstSequence       int64                         `json:"first_sequence"`
	LastSequence        int64                         `json:"last_sequence"`
	LastHash            string                        `json:"last_hash"`
	HashVersionCoverage map[string]BackupHashCoverage `json:"hash_version_coverage"`
	Checkpoint          *BackupCheckpoint             `json:"checkpoint,omitempty"`
}

type BackupHashCoverage struct {
	FirstSequence        int64 `json:"first_sequence"`
	LastSequence         int64 `json:"last_sequence"`
	TenantIdentityHashed bool  `json:"tenant_identity_hashed"`
}

type BackupCheckpoint struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	HighWaterMark int64  `json:"high_water_mark"`
	Hash          string `json:"hash"`
	SignatureHex  string `json:"signature_hex"`
}

type signedBackupManifest struct {
	Manifest        BackupManifest `json:"manifest"`
	ManifestHash    string         `json:"manifest_sha256"`
	SignatureScheme string         `json:"signature_scheme"`
	SignatureHex    string         `json:"signature_hex"`
}

// BackupManager binds a shared SQLite handle to a drain gate. It never owns
// the database handle.
type BackupManager struct {
	db   *sql.DB
	gate *DrainGate
}

func NewBackupManager(db *sql.DB, gate *DrainGate) *BackupManager {
	if gate == nil {
		gate = NewDrainGate()
	}
	return &BackupManager{db: db, gate: gate}
}

// WithWrite is the write-side companion to Create. Existing write paths can
// use it to make the drain gate an actual admission barrier.
func (m *BackupManager) WithWrite(ctx context.Context, fn func(*sql.DB) error) error {
	if m == nil || m.db == nil || fn == nil {
		return errors.New("assurance: database and write function are required")
	}
	release, err := m.gate.EnterWrite(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn(m.db)
}

func (m *BackupManager) Create(ctx context.Context, destination string, opts BackupOptions) (BackupEnvelope, error) {
	if m == nil || m.db == nil {
		return BackupEnvelope{}, errors.New("assurance: backup manager and database are required")
	}
	if opts.Gate != nil && opts.Gate != m.gate {
		return BackupEnvelope{}, errors.New("assurance: backup gate does not match shared admission gate")
	}
	opts.Gate = m.gate
	return createBackup(ctx, m.db, destination, opts)
}

// CreateBackup creates an immutable local envelope. The final directory is
// never overwritten; a crash before manifest commit leaves an unusable
// incomplete directory rather than a misleading backup.
func CreateBackup(ctx context.Context, db *sql.DB, destination string, opts BackupOptions) (BackupEnvelope, error) {
	return createBackup(ctx, db, destination, opts)
}

func createBackup(ctx context.Context, db *sql.DB, destination string, opts BackupOptions) (envelope BackupEnvelope, resultErr error) {
	if db == nil || opts.TenantID == "" {
		return BackupEnvelope{}, errors.New("assurance: database and tenant are required")
	}
	if _, _, err := signingScheme(opts.SigningKey, false); err != nil {
		return BackupEnvelope{}, err
	}
	gate := opts.Gate
	if gate == nil {
		gate = NewDrainGate()
	}
	release, err := gate.BeginDrain(ctx)
	if err != nil {
		return BackupEnvelope{}, fmt.Errorf("assurance: begin backup drain: %w", err)
	}
	// Keep admission closed if SQLite cannot be restored to writable mode.
	// An independent bounded context is needed when the backup caller cancels.
	if db.Stats().MaxOpenConnections != 1 {
		release()
		return BackupEnvelope{}, errors.New("assurance: backup requires the shared single-connection SQLite opener")
	}
	if _, err := db.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		// The PRAGMA may have completed even if the caller's context was canceled.
		resetCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, resetErr := db.ExecContext(resetCtx, `PRAGMA query_only=OFF`)
		cancel()
		if resetErr == nil {
			release()
		}
		return BackupEnvelope{}, errors.Join(fmt.Errorf("assurance: enable SQLite write drain: %w", err), resetErr)
	}
	defer func() {
		resetCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, resetErr := db.ExecContext(resetCtx, `PRAGMA query_only=OFF`)
		if resetErr != nil {
			envelope = BackupEnvelope{}
			resultErr = errors.Join(resultErr, fmt.Errorf("assurance: restore SQLite write readiness: %w", resetErr))
			return // fail closed: do not reopen admission
		}
		release()
	}()

	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return BackupEnvelope{}, fmt.Errorf("assurance: create backup parent: %w", err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return BackupEnvelope{}, fmt.Errorf("%w: %s", ErrBackupTampered, destination)
		}
		return BackupEnvelope{}, fmt.Errorf("assurance: create backup envelope: %w", err)
	}
	databasePath := filepath.Join(destination, backupDatabaseName)
	partialPath := databasePath + ".partial"
	manifestPath := filepath.Join(destination, backupManifestName)
	if err := onlineBackup(ctx, db, partialPath); err != nil {
		return BackupEnvelope{}, fmt.Errorf("assurance: online backup: %w", err)
	}
	if err := os.Rename(partialPath, databasePath); err != nil {
		return BackupEnvelope{}, fmt.Errorf("assurance: seal database file: %w", err)
	}
	if opts.Fault != nil {
		if err := opts.Fault(BackupPhaseDatabaseComplete); err != nil {
			return BackupEnvelope{}, err
		}
	}

	manifest, err := collectBackupManifest(ctx, db, databasePath, opts)
	if err != nil {
		return BackupEnvelope{}, err
	}
	manifest.EnvelopeID = sha256Hex([]byte(manifest.DatabaseSHA256 + "\x00" + manifest.CreatedAt + "\x00" + manifest.TenantID))[:32]
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return BackupEnvelope{}, fmt.Errorf("assurance: marshal backup manifest: %w", err)
	}
	manifestHash := sha256Hex(manifestJSON)
	signed, err := signManifest(manifest, opts.SigningKey)
	if err != nil {
		return BackupEnvelope{}, err
	}
	if opts.Fault != nil {
		if err := opts.Fault(BackupPhaseManifestBeforeCommit); err != nil {
			return BackupEnvelope{}, err
		}
	}
	manifestBytes, err := json.Marshal(signed)
	if err != nil {
		return BackupEnvelope{}, fmt.Errorf("assurance: marshal signed manifest: %w", err)
	}
	manifestTemp := manifestPath + ".partial"
	if err := writeSyncFile(manifestTemp, manifestBytes, 0o600); err != nil {
		return BackupEnvelope{}, err
	}
	if err := os.Rename(manifestTemp, manifestPath); err != nil {
		return BackupEnvelope{}, fmt.Errorf("assurance: seal manifest: %w", err)
	}
	if err := syncDirectory(destination); err != nil {
		return BackupEnvelope{}, err
	}
	// The contents are now sealed. Directory permissions prevent accidental
	// mutation by ordinary processes; restore still verifies every byte.
	_ = os.Chmod(databasePath, 0o444)
	_ = os.Chmod(manifestPath, 0o444)
	_ = os.Chmod(destination, 0o700)
	if opts.Fault != nil {
		if err := opts.Fault(BackupPhaseEnvelopeComplete); err != nil {
			return BackupEnvelope{}, err
		}
	}
	return BackupEnvelope{Directory: destination, DatabasePath: databasePath, ManifestPath: manifestPath, Manifest: manifest, ManifestHash: manifestHash, SignatureScheme: signed.SignatureScheme}, nil
}

func onlineBackup(ctx context.Context, db *sql.DB, destination string) error {
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return conn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("assurance: SQLite driver does not expose online backup")
		}
		backup, err := backuper.NewBackup(destination)
		if err != nil {
			return err
		}
		more, stepErr := backup.Step(-1)
		if stepErr == nil && more {
			stepErr = errors.New("assurance: SQLite online backup did not finish")
		}
		finishErr := backup.Finish()
		return errors.Join(stepErr, finishErr)
	})
}

func collectBackupManifest(ctx context.Context, db *sql.DB, databasePath string, opts BackupOptions) (BackupManifest, error) {
	markers, err := schemaMarkers(ctx, db)
	if err != nil {
		return BackupManifest{}, err
	}
	if err := validateMarkers(markers, opts.ExpectedSchemaMarkers); err != nil {
		return BackupManifest{}, err
	}
	if err := validateFenceHighWater(opts.FenceHighWater, opts.TenantID); err != nil {
		return BackupManifest{}, err
	}
	tenantCounts, tableCounts, err := countTables(ctx, db, opts.TenantID)
	if err != nil {
		return BackupManifest{}, err
	}
	auditSnapshot, err := collectAuditSnapshot(ctx, db, opts)
	if err != nil {
		return BackupManifest{}, err
	}
	evidence, err := evidenceManifestHashes(ctx, db, opts.TenantID)
	if err != nil {
		return BackupManifest{}, err
	}
	stat, err := os.Stat(databasePath)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("assurance: stat backup database: %w", err)
	}
	hash, err := fileSHA256(databasePath)
	if err != nil {
		return BackupManifest{}, err
	}
	return BackupManifest{
		FormatVersion:          backupFormatVersion,
		Complete:               true,
		CreatedAt:              time.Now().UTC().Format(backupTimeFormat),
		TenantID:               opts.TenantID,
		DatabaseFile:           backupDatabaseName,
		DatabaseSHA256:         hash,
		DatabaseBytes:          stat.Size(),
		SchemaMarkers:          markers,
		TenantRowCounts:        tenantCounts,
		TableRowCounts:         tableCounts,
		Audit:                  auditSnapshot,
		EvidenceManifestHashes: evidence,
		FenceHighWater:         opts.FenceHighWater,
	}, nil
}

func validateFenceHighWater(highWater *BackupFenceHighWater, tenant string) error {
	if highWater == nil {
		return nil
	}
	if highWater.TenantDigest == "" || highWater.EnvironmentDigest == "" || highWater.CapturedAt == "" || highWater.Authority != "azure_blob_last_modified" {
		return fmt.Errorf("%w: incomplete external fence high-water", ErrBackupTampered)
	}
	if _, err := time.Parse(time.RFC3339Nano, highWater.CapturedAt); err != nil {
		return fmt.Errorf("%w: invalid external fence high-water time", ErrBackupTampered)
	}
	if tenant == "" || highWater.TenantDigest != sha256Hex([]byte(tenant)) {
		return fmt.Errorf("%w: external fence high-water tenant binding mismatch", ErrBackupTampered)
	}
	return nil
}

func signManifest(manifest BackupManifest, key SigningKey) (signedBackupManifest, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return signedBackupManifest{}, err
	}
	scheme, _, err := signingScheme(key, false)
	if err != nil {
		return signedBackupManifest{}, err
	}
	message := append([]byte(backupDomain), raw...)
	var sig []byte
	switch scheme {
	case "hmac-sha256":
		mac := hmac.New(sha256.New, key.HMAC)
		_, _ = mac.Write(message)
		sig = mac.Sum(nil)
	case "ed25519":
		sig = ed25519.Sign(key.Ed25519Private, message)
	default:
		return signedBackupManifest{}, errors.New("assurance: unsupported backup signature scheme")
	}
	return signedBackupManifest{Manifest: manifest, ManifestHash: sha256Hex(raw), SignatureScheme: scheme, SignatureHex: hex.EncodeToString(sig)}, nil
}

func signingScheme(key SigningKey, forVerify bool) (string, ed25519.PublicKey, error) {
	hasHMAC := len(key.HMAC) != 0
	hasEd := len(key.Ed25519Private) != 0 || len(key.Ed25519Public) != 0
	if hasHMAC == hasEd {
		return "", nil, errors.New("assurance: configure exactly one HMAC or Ed25519 backup key")
	}
	if hasHMAC {
		return "hmac-sha256", nil, nil
	}
	if len(key.Ed25519Private) != 0 && len(key.Ed25519Private) != ed25519.PrivateKeySize {
		return "", nil, errors.New("assurance: invalid Ed25519 private key")
	}
	pub := key.Ed25519Public
	if len(pub) == 0 && len(key.Ed25519Private) != 0 {
		pub = key.Ed25519Private.Public().(ed25519.PublicKey)
	}
	if len(pub) != ed25519.PublicKeySize {
		return "", nil, errors.New("assurance: invalid Ed25519 public key")
	}
	if forVerify && len(key.Ed25519Private) != 0 && !hmac.Equal(pub, key.Ed25519Private.Public().(ed25519.PublicKey)) {
		return "", nil, errors.New("assurance: Ed25519 public/private key mismatch")
	}
	return "ed25519", append(ed25519.PublicKey(nil), pub...), nil
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("assurance: open %s for hashing: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("assurance: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeSyncFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("assurance: create %s: %w", path, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("assurance: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("assurance: sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("assurance: close %s: %w", path, err)
	}
	ok = true
	return nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("assurance: open envelope directory: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("assurance: sync envelope directory: %w", err)
	}
	return nil
}

func schemaMarkers(ctx context.Context, db *sql.DB) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT app, max(version), count(*) FROM schema_migrations GROUP BY app ORDER BY app`)
	if err != nil {
		return nil, fmt.Errorf("assurance: read schema markers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	markers := map[string]int{}
	for rows.Next() {
		var app string
		var maxVersion, count int
		if err := rows.Scan(&app, &maxVersion, &count); err != nil {
			return nil, err
		}
		if app == "" || maxVersion <= 0 || count != maxVersion {
			return nil, fmt.Errorf("%w: incomplete schema marker for %q", ErrBackupTampered, app)
		}
		markers[app] = maxVersion
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if markers["assurance"] != len(migrations) {
		return nil, fmt.Errorf("%w: assurance schema marker is %d, want %d", ErrBackupTampered, markers["assurance"], len(migrations))
	}
	return markers, nil
}

func validateMarkers(actual, expected map[string]int) error {
	for app, want := range expected {
		if actual[app] != want {
			return fmt.Errorf("%w: schema marker %s=%d, want %d", ErrBackupTampered, app, actual[app], want)
		}
	}
	return nil
}

func countTables(ctx context.Context, db *sql.DB, tenant string) (map[string]int64, map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var tableNames []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, nil, err
		}
		tableNames = append(tableNames, table)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	tenantCounts := map[string]int64{}
	tableCounts := map[string]int64{}
	for _, table := range tableNames {
		quoted := quoteIdentifier(table)
		var total int64
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+quoted).Scan(&total); err != nil {
			return nil, nil, fmt.Errorf("assurance: count table %s: %w", table, err)
		}
		tableCounts[table] = total
		columns, err := backupTableColumns(ctx, db, table)
		if err != nil {
			return nil, nil, err
		}
		if columns["tenant_id"] {
			var count int64
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+quoted+` WHERE "tenant_id"=?`, tenant).Scan(&count); err != nil {
				return nil, nil, fmt.Errorf("assurance: count tenant rows in %s: %w", table, err)
			}
			tenantCounts[table] = count
		}
	}
	return tenantCounts, tableCounts, nil
}

func backupTableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+quoteIdentifier(table)+`)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func evidenceManifestHashes(ctx context.Context, db *sql.DB, tenant string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT manifest_id, manifest_hash, manifest_json FROM assurance_evidence_manifests WHERE tenant_id=? ORDER BY manifest_id`, tenant)
	if err != nil {
		return nil, fmt.Errorf("assurance: read evidence manifests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := map[string]string{}
	for rows.Next() {
		var id, storedHash, raw string
		if err := rows.Scan(&id, &storedHash, &raw); err != nil {
			return nil, err
		}
		if !validSHA256(storedHash) || sha256Hex([]byte(raw)) != strings.ToLower(storedHash) {
			return nil, fmt.Errorf("%w: evidence manifest %s hash mismatch", ErrBackupTampered, id)
		}
		if _, exists := result[id]; exists {
			return nil, fmt.Errorf("%w: duplicate evidence manifest %s", ErrBackupTampered, id)
		}
		result[id] = strings.ToLower(storedHash)
	}
	return result, rows.Err()
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// collectAuditSnapshot validates the complete tenant chain before the backup
// becomes sealable. It intentionally reads the same shared handle that is
// drained, so its checkpoint and counts describe the bytes being backed up.
func collectAuditSnapshot(ctx context.Context, db *sql.DB, opts BackupOptions) (BackupAudit, error) {
	rows, err := db.QueryContext(ctx, `SELECT seq, ts, tenant_id, hash_version, actor, tool, action, target, before_json, after_json, workiva_op_url, audit_id, prev_hash, hash FROM audit_log ORDER BY seq`)
	if err != nil {
		return BackupAudit{}, fmt.Errorf("assurance: read audit chain: %w", err)
	}
	defer func() { _ = rows.Close() }()
	previous := map[string]string{}
	coverage := map[string]BackupHashCoverage{}
	var selected []auditRow
	for rows.Next() {
		var r auditRow
		var ts any
		var actor, tool, action, target, before, after, opURL, auditID, prev, hash sql.NullString
		if err := rows.Scan(&r.Seq, &ts, &r.Tenant, &r.HashVersion, &actor, &tool, &action, &target, &before, &after, &opURL, &auditID, &prev, &hash); err != nil {
			return BackupAudit{}, err
		}
		r.TS, err = auditTimestamp(ts)
		if err != nil {
			return BackupAudit{}, err
		}
		r.Actor, r.Tool, r.Action, r.Target = actor.String, tool.String, action.String, target.String
		r.Before, r.After, r.OpURL, r.AuditID = before.String, after.String, opURL.String, auditID.String
		r.Prev, r.Hash = prev.String, hash.String
		wantPrev := previous[r.Tenant]
		if wantPrev == "" {
			wantPrev = "GENESIS"
		}
		if r.Prev != wantPrev {
			return BackupAudit{}, fmt.Errorf("%w: audit chain previous hash at seq %d", ErrBackupTampered, r.Seq)
		}
		var want string
		switch r.HashVersion {
		case 1:
			want = auditHashV1(r.Prev, r.TS, r.Actor, r.Tool, r.Action, r.Target, r.Before, r.After, r.OpURL, r.AuditID)
		case 2:
			want = auditHashV2(r.Prev, r.Tenant, r.TS, r.Actor, r.Tool, r.Action, r.Target, r.Before, r.After, r.OpURL, r.AuditID)
		default:
			return BackupAudit{}, fmt.Errorf("%w: unsupported audit hash version %d", ErrBackupTampered, r.HashVersion)
		}
		if r.Hash != want {
			return BackupAudit{}, fmt.Errorf("%w: audit hash at seq %d", ErrBackupTampered, r.Seq)
		}
		previous[r.Tenant] = r.Hash
		if r.Tenant == opts.TenantID {
			selected = append(selected, r)
		}
		key := strconv.Itoa(r.HashVersion)
		item := coverage[key]
		if item.FirstSequence == 0 {
			item.FirstSequence = r.Seq
		}
		item.LastSequence = r.Seq
		item.TenantIdentityHashed = r.HashVersion >= 2
		coverage[key] = item
	}
	if err := rows.Err(); err != nil {
		return BackupAudit{}, err
	}
	audit := BackupAudit{ChainVerified: true, HashVersionCoverage: coverage, RowCount: int64(len(selected))}
	if len(selected) > 0 {
		audit.FirstSequence = selected[0].Seq
		audit.LastSequence = selected[len(selected)-1].Seq
		audit.LastHash = selected[len(selected)-1].Hash
		checkpoint, err := readCheckpoint(ctx, db, opts.TenantID, audit.LastSequence, audit.LastHash, opts)
		if err != nil {
			return BackupAudit{}, err
		}
		audit.Checkpoint = &checkpoint
	}
	return audit, nil
}

type auditRow struct {
	Seq                                                                                int64
	TS, Tenant, Actor, Tool, Action, Target, Before, After, OpURL, AuditID, Prev, Hash string
	HashVersion                                                                        int
}

func auditTimestamp(value any) (string, error) {
	switch v := value.(type) {
	case time.Time:
		return v.UTC().Format("2006-01-02 15:04:05"), nil
	case string:
		for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, v); err == nil {
				return parsed.UTC().Format("2006-01-02 15:04:05"), nil
			}
		}
		return "", fmt.Errorf("%w: invalid audit timestamp %q", ErrBackupTampered, v)
	case []byte:
		return auditTimestamp(string(v))
	default:
		return "", fmt.Errorf("%w: invalid audit timestamp type %T", ErrBackupTampered, value)
	}
}

func auditHashV1(prev, ts, actor, tool, action, target, before, after, opURL, auditID string) string {
	return sha256Hex([]byte(prev + ts + actor + tool + action + target + before + after + opURL + auditID))
}

func auditHashV2(prev, tenant, ts, actor, tool, action, target, before, after, opURL, auditID string) string {
	return sha256Hex([]byte(strings.Join([]string{prev, tenant, ts, actor, tool, action, target, before, after, opURL, auditID}, "\x00")))
}

func readCheckpoint(ctx context.Context, db *sql.DB, tenant string, seq int64, hash string, opts BackupOptions) (BackupCheckpoint, error) {
	id := opts.CheckpointID
	if id == "" {
		id = "audit-terminal"
	}
	var checkpoint BackupCheckpoint
	var highWater, storedHash, signature, kind string
	err := db.QueryRowContext(ctx, `SELECT checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex FROM assurance_checkpoints WHERE tenant_id=? AND checkpoint_id=?`, tenant, id).Scan(&checkpoint.ID, &kind, &highWater, &storedHash, &signature)
	if err == sql.ErrNoRows {
		return BackupCheckpoint{}, fmt.Errorf("%w: audit checkpoint %s is missing", ErrBackupTampered, id)
	}
	if err != nil {
		return BackupCheckpoint{}, fmt.Errorf("assurance: read audit checkpoint: %w", err)
	}
	checkpoint.Kind, checkpoint.SignatureHex = kind, signature
	parsed, err := strconv.ParseInt(highWater, 10, 64)
	if err != nil || parsed != seq || storedHash != hash || signature == "" {
		return BackupCheckpoint{}, fmt.Errorf("%w: audit checkpoint does not match terminal audit row", ErrBackupTampered)
	}
	checkpoint.HighWaterMark, checkpoint.Hash = parsed, storedHash
	if len(opts.CheckpointPublicKey) != ed25519.PublicKeySize {
		return BackupCheckpoint{}, fmt.Errorf("%w: audit checkpoint trust key is not configured", ErrBackupTampered)
	}
	sig, err := hex.DecodeString(signature)
	if err != nil || !ed25519.Verify(opts.CheckpointPublicKey, []byte(strings.Join([]string{tenant, strconv.FormatInt(seq, 10), hash, id}, "\x00")), sig) {
		return BackupCheckpoint{}, fmt.Errorf("%w: audit checkpoint signature mismatch", ErrBackupTampered)
	}
	return checkpoint, nil
}
