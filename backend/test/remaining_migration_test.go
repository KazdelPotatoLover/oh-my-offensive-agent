package test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// This test needs a second, empty database. It never downgrades or truncates
// TEST_DATABASE_URL, and refuses an already migrated database.
func TestRemaining_MigrationUpgradeAndAuditRollbackGuards(t *testing.T) {
	database := os.Getenv("MIGRATION_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("set MIGRATION_TEST_DATABASE_URL to a new, empty migration acceptance database")
	}
	u, err := url.Parse(database)
	require.NoError(t, err)
	require.Contains(t, u.Path, "migration", "use a dedicated migration database")
	require.NotEqual(t, "/agent_workbench", u.Path)
	m, err := migrate.New("file://"+migrationsPath, database)
	require.NoError(t, err)
	defer m.Close()
	_, _, err = m.Version()
	require.ErrorIs(t, err, migrate.ErrNilVersion, "acceptance must begin with a fresh database")
	require.NoError(t, m.Migrate(15))
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, database)
	require.NoError(t, err)
	defer pool.Close()
	taskID, runID, assetID, checkID, findingID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO tasks(id,name,objective,scope,config) VALUES($1,'Legacy audited task','Original objective','{"endpoints":["sample_home"]}','{}')`, []any{taskID}},
		{`INSERT INTO runs(id,task_id,status,finish_reason,execution_input,final_output) VALUES($1,$2,'completed','final_answer','{"objective":"Original objective","scope":{"endpoints":["sample_home"]}}','Original closeout')`, []any{runID, taskID}},
		{`INSERT INTO assets(id,namespace,kind,external_key,name) VALUES($1,'sample','http_response_sample','sample_home','Legacy asset')`, []any{assetID}},
		{`INSERT INTO run_assets(run_id,asset_id,snapshot) VALUES($1,$2,'{"original":true}')`, []any{runID, assetID}},
		{`INSERT INTO check_results(id,run_id,asset_id,check_id,check_name,check_version,status,explanation,evidence_snapshot) VALUES($1,$2,$3,'legacy_rule','Legacy rule','1','needs_attention','Original evidence','{"marker":"unchanged"}')`, []any{checkID, runID, assetID}},
		{`INSERT INTO findings(id,run_id,check_result_id,asset_id,title,category,severity,status,description,review_version) VALUES($1,$2,$3,$4,'Reviewed legacy finding','configuration','low','confirmed','Original description',1)`, []any{findingID, runID, checkID, assetID}},
		{`INSERT INTO finding_reviews(finding_id,run_id,from_status,to_status,from_version,to_version,reviewer,comment,request_id) VALUES($1,$2,'pending_review','confirmed',0,1,'Legacy auditor','Original review',$3)`, []any{findingID, runID, uuid.New()}},
	}
	for _, statement := range statements {
		_, err := pool.Exec(ctx, statement.sql, statement.args...)
		require.NoError(t, err)
	}
	const snapshot = `SELECT jsonb_build_object('run',jsonb_build_object('status',r.status,'input',r.execution_input,'output',r.final_output),
		'finding',jsonb_build_object('title',f.title,'status',f.status,'version',f.review_version),
		'review',jsonb_build_object('reviewer',v.reviewer,'comment',v.comment,'request_id',v.request_id),
		'evidence',c.evidence_snapshot,'scope',ra.snapshot)
		FROM runs r JOIN findings f ON f.run_id=r.id JOIN finding_reviews v ON v.finding_id=f.id
		JOIN check_results c ON c.id=f.check_result_id JOIN run_assets ra ON ra.run_id=r.id WHERE r.id=$1`
	var before, after []byte
	require.NoError(t, pool.QueryRow(ctx, snapshot, runID).Scan(&before))
	require.NoError(t, m.Up())
	require.NoError(t, pool.QueryRow(ctx, snapshot, runID).Scan(&after))
	require.JSONEq(t, string(before), string(after))
	var fabricated int
	require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM exploration_nodes)+(SELECT count(*) FROM execution_epochs)`).Scan(&fabricated))
	require.Zero(t, fabricated, "migration must not invent goals, proofs or execution authorization")
	var source, verification string
	require.NoError(t, pool.QueryRow(ctx, `SELECT source_kind,verification_state FROM findings WHERE id=$1`, findingID).Scan(&source, &verification))
	require.Equal(t, "check_rule", source)
	require.Equal(t, "unverified", verification)
	// With no new exploration records, the reversible suffix can move to 21
	// and back without modifying the original review/evidence snapshot.
	require.NoError(t, m.Migrate(21))
	require.NoError(t, m.Up())
	require.NoError(t, pool.QueryRow(ctx, snapshot, runID).Scan(&after))
	require.JSONEq(t, string(before), string(after))
	_, err = pool.Exec(ctx, `INSERT INTO artifacts(run_id,epoch_number,name,content,sha256,byte_length) VALUES($1,1,'immutable.txt','saved','audit-check',5)`, runID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO control_commands(run_id,kind,idempotency_key,request_hash,result) VALUES($1,'epoch_start','original-command','original-hash','{"epoch_number":2}')`, runID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO execution_epochs(run_id,epoch_number,status) VALUES($1,1,'stopped')`, runID)
	require.NoError(t, err)
	for _, name := range []string{"016_artex_alignment_core", "022_capabilities_artifacts_checkpoints", "024_control_commands", "015_audit_retention_restrict_fk", "014_finding_reviews"} {
		t.Run(name, func(t *testing.T) {
			sql, err := os.ReadFile("../migrations/" + name + ".down.sql")
			require.NoError(t, err)
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer tx.Rollback(ctx)
			_, err = tx.Exec(ctx, string(sql))
			var pgerr *pgconn.PgError
			require.ErrorAs(t, err, &pgerr)
			require.Equal(t, "P0001", pgerr.Code, "must refuse before destructive DDL")
			require.True(t, strings.Contains(pgerr.Message, "cannot rollback"))
		})
	}
	require.NoError(t, pool.QueryRow(ctx, snapshot, runID).Scan(&after))
	require.JSONEq(t, string(before), string(after))
	t.Log("v15 -> v25 preserves original evidence and reviews; empty suffix v25 -> v21 -> v25 passes; data-bearing rollback is rejected")
}
