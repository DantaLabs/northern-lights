package assurance

import (
	"context"
	"database/sql"
	"testing"

	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	_ "modernc.org/sqlite"
)

func TestV16UpgradePreservesV15RowsAndTracksUncertaintyEpoch(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := sqlitedb.Migrate(ctx, db, "assurance", migrations[:15]); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO transfer_intents(tenant_id,transfer_id,actor_id,permission,intent_json,state,token_digest,idempotency_digest,request_digest,expires_at) VALUES('tenant','transfer','actor','confirm','{}','reconciliation_required','digest','idem','request','later')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var epoch int
	var state string
	if err := db.QueryRow(`SELECT state,reconciliation_epoch FROM transfer_intents WHERE transfer_id='transfer'`).Scan(&state, &epoch); err != nil || state != "reconciliation_required" || epoch != 0 {
		t.Fatalf("upgrade lost row state=%q epoch=%d err=%v", state, epoch, err)
	}
	if _, err := db.Exec(`UPDATE transfer_intents SET state='machine_verified_visual_ack_pending' WHERE transfer_id='transfer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE transfer_intents SET state='reconciliation_required' WHERE transfer_id='transfer'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT reconciliation_epoch FROM transfer_intents WHERE transfer_id='transfer'`).Scan(&epoch); err != nil || epoch != 1 {
		t.Fatalf("new episode epoch=%d err=%v", epoch, err)
	}
}
