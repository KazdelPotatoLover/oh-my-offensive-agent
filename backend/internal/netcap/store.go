package netcap

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists credentials, sessions, receipts, and origin quotas.
type Store struct {
	pool   *pgxpool.Pool
	gDepth sync.Map
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// TransactionDepth is the number of open transactions on the calling goroutine.
// A dial may overlap another request's database work. It must not overlap this
// goroutine's pace lock.
func (s *Store) TransactionDepth() int {
	if s == nil {
		return 0
	}
	value, ok := s.gDepth.Load(goroutineID())
	if !ok {
		return 0
	}
	return value.(int)
}

func (s *Store) addDepth(delta int) {
	id := goroutineID()
	current, _ := s.gDepth.Load(id)
	depth, _ := current.(int)
	depth += delta
	if depth <= 0 {
		s.gDepth.Delete(id)
		return
	}
	s.gDepth.Store(id, depth)
}

func (s *Store) begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	s.addDepth(1)
	return tx, nil
}

func (s *Store) done(ctx context.Context, tx pgx.Tx, err error) error {
	s.addDepth(-1)
	if err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return commitErr
	}
	return nil
}

func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	line := string(buf[:n])
	line = strings.TrimPrefix(line, "goroutine ")
	line = line[:strings.IndexByte(line, ' ')]
	id, _ := strconv.ParseUint(line, 10, 64)
	return id
}

func (s *Store) withTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	return s.done(ctx, tx, fn(tx))
}

func (s *Store) CurrentEpoch(ctx context.Context, runID uuid.UUID) (int, error) {
	var epoch int
	err := s.pool.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1`, runID).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, contractError(contract.CodeNotFound, "run not found")
	}
	return epoch, err
}

func (s *Store) LoadPolicy(ctx context.Context, runID uuid.UUID) (Policy, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT execution_input FROM runs WHERE id=$1`, runID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, contractError(contract.CodeNotFound, "run not found")
	}
	if err != nil {
		return Policy{}, err
	}
	return PolicyFromExecutionInput(raw)
}

type Credential struct {
	Alias        string
	Version      int
	TargetOrigin string
	Kind         string
	HeaderName   string
	Secret       string
	Username     string
	LoginPath    string
}

func (s *Store) PutCredential(ctx context.Context, c Credential) error {
	if c.Alias == "" || c.Secret == "" || c.TargetOrigin == "" {
		return contractError(contract.CodeInvalidInput, "credential requires alias, secret, and target origin")
	}
	if c.Version <= 0 {
		c.Version = 1
	}
	if c.Kind == "" {
		c.Kind = "token"
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO http_credentials(alias,version,target_origin,kind,header_name,secret,username,login_path)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (alias, version) DO UPDATE SET secret=EXCLUDED.secret, target_origin=EXCLUDED.target_origin, kind=EXCLUDED.kind, header_name=EXCLUDED.header_name, username=EXCLUDED.username, login_path=EXCLUDED.login_path`,
		c.Alias, c.Version, c.TargetOrigin, c.Kind, c.HeaderName, c.Secret, c.Username, c.LoginPath)
	return err
}

func (s *Store) LoadCredential(ctx context.Context, alias, origin string) (Credential, error) {
	var c Credential
	err := s.pool.QueryRow(ctx, `SELECT alias,version,target_origin,kind,header_name,secret,username,login_path
		FROM http_credentials WHERE alias=$1 ORDER BY version DESC LIMIT 1`, alias).Scan(
		&c.Alias, &c.Version, &c.TargetOrigin, &c.Kind, &c.HeaderName, &c.Secret, &c.Username, &c.LoginPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, contractError(contract.CodeCredentialUnavailable, "credential alias is not configured")
	}
	if err != nil {
		return Credential{}, err
	}
	if origin != "" && c.TargetOrigin != origin {
		return Credential{}, contractError(contract.CodeCredentialUnavailable, "credential is not bound to this origin")
	}
	return c, nil
}

type Session struct {
	Ref               string
	RunID             uuid.UUID
	Epoch             int
	OwnerWorkItem     *uuid.UUID
	OwnerAttempt      *uuid.UUID
	Shared            bool
	TargetOrigin      string
	CredentialAlias   string
	CredentialVersion int
	Cookie            string
	Authorization     string
}

func (s *Store) PutSharedSession(ctx context.Context, sess Session) error {
	if !sess.Shared {
		return contractError(contract.CodeInvalidInput, "shared sessions must be created explicitly")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO http_sessions(ref,run_id,epoch_number,owner_work_item_id,owner_attempt_id,shared,target_origin,credential_alias,credential_version,cookie_header,authorization_header)
		VALUES($1,$2,$3,$4,$5,true,$6,$7,$8,$9,$10)
		ON CONFLICT (run_id, ref) DO UPDATE SET shared=true, cookie_header=EXCLUDED.cookie_header, authorization_header=EXCLUDED.authorization_header, credential_alias=EXCLUDED.credential_alias, credential_version=EXCLUDED.credential_version, target_origin=EXCLUDED.target_origin, updated_at=now()`,
		sess.Ref, sess.RunID, sess.Epoch, sess.OwnerWorkItem, sess.OwnerAttempt, sess.TargetOrigin, sess.CredentialAlias, sess.CredentialVersion, sess.Cookie, sess.Authorization)
	return err
}

func (s *Store) OpenSession(ctx context.Context, scope contract.ExecutionScope, ref, origin, credAlias string, credVersion int) (Session, error) {
	var sess Session
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT ref,run_id,epoch_number,owner_work_item_id,owner_attempt_id,shared,target_origin,credential_alias,credential_version,cookie_header,authorization_header
			FROM http_sessions WHERE run_id=$1 AND ref=$2 FOR UPDATE`, scope.RunID, ref).Scan(
			&sess.Ref, &sess.RunID, &sess.Epoch, &sess.OwnerWorkItem, &sess.OwnerAttempt, &sess.Shared, &sess.TargetOrigin, &sess.CredentialAlias, &sess.CredentialVersion, &sess.Cookie, &sess.Authorization)
		if errors.Is(err, pgx.ErrNoRows) {
			sess = Session{
				Ref: ref, RunID: scope.RunID, Epoch: scope.EpochNumber,
				OwnerWorkItem: scope.WorkItemID, OwnerAttempt: scope.AttemptID,
				Shared: false, TargetOrigin: origin, CredentialAlias: credAlias, CredentialVersion: credVersion,
			}
			_, err = tx.Exec(ctx, `INSERT INTO http_sessions(ref,run_id,epoch_number,owner_work_item_id,owner_attempt_id,shared,target_origin,credential_alias,credential_version)
				VALUES($1,$2,$3,$4,$5,false,$6,$7,$8)`,
				ref, scope.RunID, scope.EpochNumber, scope.WorkItemID, scope.AttemptID, origin, credAlias, credVersion)
			return err
		}
		if err != nil {
			return err
		}
		if sess.Epoch != scope.EpochNumber {
			return contractError(contract.CodeStaleEpoch, "session belongs to another epoch")
		}
		if sess.TargetOrigin != origin {
			return contractError(contract.CodeScopeDenied, "session is bound to a different origin")
		}
		if !sameUUID(sess.OwnerWorkItem, scope.WorkItemID) && !sess.Shared {
			return contractError(contract.CodeScopeDenied, "session is isolated to another direction")
		}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

func (s *Store) SaveSessionAuth(ctx context.Context, runID uuid.UUID, ref, cookie, authorization string, credVersion int) error {
	tag, err := s.pool.Exec(ctx, `UPDATE http_sessions SET cookie_header=$3, authorization_header=$4, credential_version=$5, updated_at=now()
		WHERE run_id=$1 AND ref=$2`, runID, ref, cookie, authorization, credVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return contractError(contract.CodeNotFound, "session not found")
	}
	return nil
}

type reservation struct {
	ID          uuid.UUID
	Wait        time.Duration
	PageKey     string
	ClaimedPage bool
}

func (s *Store) Reserve(ctx context.Context, scope contract.ExecutionScope, origin string, policy Policy, pageKey string, countPage bool) (reservation, error) {
	var reserved reservation
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO http_origin_pace(run_id,epoch_number,origin,next_at)
			VALUES($1,$2,$3,now()) ON CONFLICT DO NOTHING`, scope.RunID, scope.EpochNumber, origin); err != nil {
			return err
		}
		var nextAt time.Time
		if err := tx.QueryRow(ctx, `SELECT next_at FROM http_origin_pace WHERE run_id=$1 AND epoch_number=$2 AND origin=$3 FOR UPDATE`,
			scope.RunID, scope.EpochNumber, origin).Scan(&nextAt); err != nil {
			return err
		}
		if policy.EnforceLimits {
			var pages, requests int
			if err := tx.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM http_pages WHERE run_id=$1 AND epoch_number=$2 AND origin=$3),
				(SELECT count(*) FROM http_request_ledger WHERE run_id=$1 AND epoch_number=$2 AND origin=$3 AND state IN ('reserved','completed','outcome_unknown','cancelled','timeout'))`,
				scope.RunID, scope.EpochNumber, origin).Scan(&pages, &requests); err != nil {
				return err
			}
			if requests >= policy.MaxRequests {
				return contractError(contract.CodeScopeDenied, "origin request limit reached")
			}
			if countPage && pageKey != "" {
				var seen bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM http_pages WHERE run_id=$1 AND epoch_number=$2 AND page_key=$3)`,
					scope.RunID, scope.EpochNumber, pageKey).Scan(&seen); err != nil {
					return err
				}
				if !seen && pages >= policy.MaxPages {
					return contractError(contract.CodeScopeDenied, "origin page limit reached")
				}
				if !seen {
					if _, err := tx.Exec(ctx, `INSERT INTO http_pages(run_id,epoch_number,origin,page_key) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
						scope.RunID, scope.EpochNumber, origin, pageKey); err != nil {
						return err
					}
					reserved.ClaimedPage = true
					reserved.PageKey = pageKey
				}
			}
		}
		wait := time.Until(nextAt)
		if wait < 0 {
			wait = 0
		}
		next := time.Now().Add(wait).Add(policy.Interval)
		if _, err := tx.Exec(ctx, `UPDATE http_origin_pace SET next_at=$4 WHERE run_id=$1 AND epoch_number=$2 AND origin=$3`,
			scope.RunID, scope.EpochNumber, origin, next); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO http_request_ledger(run_id,epoch_number,origin,state) VALUES($1,$2,$3,'reserved') RETURNING id`,
			scope.RunID, scope.EpochNumber, origin).Scan(&reserved.ID); err != nil {
			return err
		}
		reserved.Wait = wait
		return nil
	})
	if err != nil {
		return reservation{}, err
	}
	return reserved, nil
}

// BindPendingToolCall attaches a gateway receipt to the pending tool call the
// runner inserted before Execute. The tool does not receive that call id.
func (s *Store) BindPendingToolCall(ctx context.Context, scope contract.ExecutionScope, receiptID uuid.UUID, toolName string, args []byte) error {
	if s == nil || scope.SessionID == nil || *scope.SessionID == uuid.Nil || receiptID == uuid.Nil || toolName == "" || len(args) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var callID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM agent_tool_calls
			WHERE run_id=$1 AND session_id=$2 AND tool_name=$3 AND status='pending' AND epoch_number=$4 AND arguments=$5::jsonb
			  AND NOT EXISTS (SELECT 1 FROM http_receipts WHERE source_tool_call_id=agent_tool_calls.id)
			ORDER BY created_at DESC
			LIMIT 1
			FOR UPDATE SKIP LOCKED`,
			scope.RunID, scope.SessionID, toolName, scope.EpochNumber, string(args)).Scan(&callID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE http_receipts SET source_tool_call_id=$2 WHERE id=$1 AND run_id=$3 AND epoch_number=$4 AND source_tool_call_id IS NULL`,
			receiptID, callID, scope.RunID, scope.EpochNumber)
		return err
	})
}

func (s *Store) FinishReservation(ctx context.Context, id uuid.UUID, state string, receiptID *uuid.UUID) {
	_, _ = s.pool.Exec(ctx, `UPDATE http_request_ledger SET state=$2, receipt_id=$3 WHERE id=$1`, id, state, receiptID)
}

// ReleaseUnsentPage drops a page slot claimed before the request was sent.
// A cancellation during pacing must not consume max_pages.
func (s *Store) ReleaseUnsentPage(ctx context.Context, scope contract.ExecutionScope, pageKey string) {
	if pageKey == "" {
		return
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM http_pages WHERE run_id=$1 AND epoch_number=$2 AND page_key=$3`,
		scope.RunID, scope.EpochNumber, pageKey)
}

func (s *Store) NotePage(ctx context.Context, scope contract.ExecutionScope, origin, key string) {
	_, _ = s.pool.Exec(ctx, `INSERT INTO http_pages(run_id,epoch_number,origin,page_key) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		scope.RunID, scope.EpochNumber, origin, key)
}

type savedExchange struct {
	Scope        contract.ExecutionScope
	AssetID      *uuid.UUID
	Method       string
	URL          string
	FinalURL     string
	Status       int
	Mode         string
	RequestHash  string
	ResponseHash string
	CredRef      string
	CredVersion  int
	SessionRef   string
	Truncated    bool
	Outcome      string
	Preview      string
	ReplaySafe   bool
	RequestHead  map[string][]string
	RequestBody  []byte
	RequestType  string
	ResponseHead map[string][]string
	ResponseBody []byte
	ResponseType string
	Binary       bool
	Sensitive    bool
	ToolName     string
	ToolArgs     []byte
}

func (s *Store) SaveExchange(ctx context.Context, ex savedExchange) (*contract.HTTPReceipt, error) {
	var receipt *contract.HTTPReceipt
	stale := false
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var current int
		if err := tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1 FOR UPDATE`, ex.Scope.RunID).Scan(&current); err != nil {
			return err
		}
		// Persist under the epoch that started the exchange. A later epoch must
		// not treat this row as current-epoch evidence.
		if current != ex.Scope.EpochNumber {
			stale = true
		}
		saved, err := insertExchange(ctx, tx, ex)
		if err != nil {
			return err
		}
		receipt = saved
		return nil
	})
	if err != nil {
		return nil, err
	}
	if stale {
		return nil, contractError(contract.CodeStaleEpoch, "run epoch changed before the exchange could be recorded")
	}
	return receipt, nil
}

func insertExchange(ctx context.Context, tx pgx.Tx, ex savedExchange) (*contract.HTTPReceipt, error) {
	callID := ex.Scope.SourceToolCallID
	if callID == nil && ex.Scope.SessionID != nil && ex.ToolName != "" && len(ex.ToolArgs) > 0 {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM agent_tool_calls
			WHERE run_id=$1 AND session_id=$2 AND tool_name=$3 AND status='pending' AND epoch_number=$4 AND arguments=$5::jsonb
			ORDER BY created_at DESC LIMIT 1`,
			ex.Scope.RunID, ex.Scope.SessionID, ex.ToolName, ex.Scope.EpochNumber, string(ex.ToolArgs)).Scan(&id)
		if err == nil {
			callID = &id
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	reqArt, err := insertArtifact(ctx, tx, ex, "request", ex.RequestType, ex.RequestHead, ex.RequestBody, false)
	if err != nil {
		return nil, err
	}
	var respArt *uuid.UUID
	if ex.Mode != contract.CaptureConnectMeta {
		id, err := insertArtifact(ctx, tx, ex, "response", ex.ResponseType, ex.ResponseHead, ex.ResponseBody, ex.Binary)
		if err != nil {
			return nil, err
		}
		respArt = &id
	}
	receiptID := uuid.New()
	requestID := uuid.New()
	_, err = tx.Exec(ctx, `INSERT INTO http_receipts(
		id,request_id,run_id,epoch_number,role,work_item_id,attempt_id,session_id,source_tool_call_id,job_id,asset_id,
		method,url,final_url,status_code,capture_mode,request_hash,response_hash,credential_ref,credential_version,session_ref,
		truncated,outcome,preview,request_artifact_id,response_artifact_id,replay_safe,minted_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,'gateway')`,
		receiptID, requestID, ex.Scope.RunID, ex.Scope.EpochNumber, string(ex.Scope.Role), ex.Scope.WorkItemID, ex.Scope.AttemptID, ex.Scope.SessionID, callID, ex.Scope.JobID, ex.AssetID,
		ex.Method, ex.URL, ex.FinalURL, ex.Status, ex.Mode, ex.RequestHash, ex.ResponseHash, ex.CredRef, ex.CredVersion, ex.SessionRef,
		ex.Truncated, ex.Outcome, ex.Preview, reqArt, respArt, ex.ReplaySafe)
	if err != nil {
		return nil, err
	}
	receipt := &contract.HTTPReceipt{
		RequestID: requestID, ReceiptID: receiptID, AssetID: ex.AssetID, Method: ex.Method, URL: ex.URL,
		StatusCode: ex.Status, ResponsePreview: ex.Preview, RequestArtifactID: &reqArt, ResponseArtifactID: respArt,
		Truncated: ex.Truncated, RequestHash: ex.RequestHash, ResponseHash: ex.ResponseHash,
		CredentialRef: ex.CredRef, CaptureMode: ex.Mode, Scope: ex.Scope,
	}
	if callID != nil {
		receipt.Scope.SourceToolCallID = callID
	}
	return receipt, nil
}

func insertArtifact(ctx context.Context, tx pgx.Tx, ex savedExchange, kind, media string, headers map[string][]string, body []byte, binary bool) (uuid.UUID, error) {
	if media == "" {
		media = "application/octet-stream"
	}
	if body == nil {
		body = []byte{}
	}
	if headers == nil {
		headers = map[string][]string{}
	}
	rawHeaders, _ := json.Marshal(headers)
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO http_capture_artifacts(run_id,epoch_number,kind,media_type,headers,content,sha256,byte_length,truncated,is_binary,sensitive)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		ex.Scope.RunID, ex.Scope.EpochNumber, kind, media, rawHeaders, body, sha256Hex(body), len(body), ex.Truncated && kind == "response", binary, ex.Sensitive).Scan(&id)
	return id, err
}

func (s *Store) Lookup(ctx context.Context, receiptID uuid.UUID) (*contract.HTTPReceipt, error) {
	var r contract.HTTPReceipt
	var role string
	var work, attempt, session, call, job *uuid.UUID
	var cred string
	var runID uuid.UUID
	var epoch int
	err := s.pool.QueryRow(ctx, `SELECT request_id,id,asset_id,method,url,status_code,preview,request_artifact_id,response_artifact_id,truncated,request_hash,response_hash,credential_ref,capture_mode,
		run_id,epoch_number,role,work_item_id,attempt_id,session_id,source_tool_call_id,job_id
		FROM http_receipts WHERE id=$1 AND minted_by='gateway'`, receiptID).Scan(
		&r.RequestID, &r.ReceiptID, &r.AssetID, &r.Method, &r.URL, &r.StatusCode, &r.ResponsePreview, &r.RequestArtifactID, &r.ResponseArtifactID, &r.Truncated, &r.RequestHash, &r.ResponseHash, &cred, &r.CaptureMode,
		&runID, &epoch, &role, &work, &attempt, &session, &call, &job)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, contractError(contract.CodeNotFound, "receipt not found")
	}
	if err != nil {
		return nil, err
	}
	r.CredentialRef = cred
	r.Scope = contract.ExecutionScope{
		RunID: runID, EpochNumber: epoch, Role: contract.Role(role),
		WorkItemID: work, AttemptID: attempt, SessionID: session, SourceToolCallID: call, JobID: job,
	}
	return &r, nil
}

// LookupScoped rejects a receipt that belongs to another run or epoch.
func (s *Store) LookupScoped(ctx context.Context, scope contract.ExecutionScope, receiptID uuid.UUID) (*contract.HTTPReceipt, error) {
	r, err := s.Lookup(ctx, receiptID)
	if err != nil {
		return nil, err
	}
	if r.Scope.RunID != scope.RunID || r.Scope.EpochNumber != scope.EpochNumber {
		return nil, contractError(contract.CodeScopeDenied, "receipt belongs to another run or epoch")
	}
	return r, nil
}

type artifactView struct {
	MediaType string
	Headers   map[string][]string
	Body      []byte
	Binary    bool
	Sensitive bool
	SHA256    string
}

func (s *Store) ReadArtifact(ctx context.Context, scope contract.ExecutionScope, artifactID uuid.UUID, authorized bool) (artifactView, error) {
	var view artifactView
	var rawHeaders []byte
	var runID uuid.UUID
	var epoch int
	err := s.pool.QueryRow(ctx, `SELECT run_id,epoch_number,media_type,headers,content,is_binary,sensitive,sha256 FROM http_capture_artifacts WHERE id=$1`, artifactID).Scan(
		&runID, &epoch, &view.MediaType, &rawHeaders, &view.Body, &view.Binary, &view.Sensitive, &view.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return artifactView{}, contractError(contract.CodeNotFound, "capture artifact not found")
	}
	if err != nil {
		return artifactView{}, err
	}
	if runID != scope.RunID || epoch != scope.EpochNumber {
		return artifactView{}, contractError(contract.CodeScopeDenied, "artifact belongs to another run or epoch")
	}
	_ = json.Unmarshal(rawHeaders, &view.Headers)
	if view.Sensitive && !authorized {
		view.Body = []byte(redactBodyPreview(view.Body, view.Binary))
		view.Headers = redactHeaderMap(view.Headers)
	}
	if len(view.Body) > hardMaxBody {
		view.Body = view.Body[:hardMaxBody]
	}
	return view, nil
}

func redactHeaderMap(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for key, values := range in {
		if isSensitiveHeader(key) {
			out[key] = []string{"[redacted]"}
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	return out
}

func (s *Store) AssetInRun(ctx context.Context, runID, assetID uuid.UUID) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_assets WHERE run_id=$1 AND asset_id=$2)`, runID, assetID).Scan(&ok)
	return ok, err
}

func (s *Store) AssetInWorkItem(ctx context.Context, workItemID, assetID uuid.UUID) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_item_assets WHERE work_item_id=$1 AND asset_id=$2)`, workItemID, assetID).Scan(&ok)
	return ok, err
}

func (s *Store) AssetURL(ctx context.Context, runID, assetID uuid.UUID) (string, string, error) {
	var kind, externalKey string
	var snapshot []byte
	err := s.pool.QueryRow(ctx, `SELECT a.kind, a.external_key, ra.snapshot FROM run_assets ra JOIN assets a ON a.id=ra.asset_id WHERE ra.run_id=$1 AND ra.asset_id=$2`,
		runID, assetID).Scan(&kind, &externalKey, &snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", contractError(contract.CodeScopeDenied, "asset is outside run scope")
	}
	if err != nil {
		return "", "", err
	}
	var snap map[string]any
	if json.Unmarshal(snapshot, &snap) == nil {
		if meta, ok := snap["metadata"].(map[string]any); ok {
			if u, ok := meta["url"].(string); ok && u != "" {
				return u, kind, nil
			}
		}
	}
	parts := strings.SplitN(externalKey, " ", 2)
	if len(parts) == 2 {
		return parts[1], kind, nil
	}
	return externalKey, kind, nil
}

func (s *Store) SaveCapability(ctx context.Context, hash string, scope contract.ExecutionScope, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO http_job_capabilities(token_hash,run_id,epoch_number,job_id,work_item_id,attempt_id,session_id,role,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		hash, scope.RunID, scope.EpochNumber, scope.JobID, scope.WorkItemID, scope.AttemptID, scope.SessionID, string(scope.Role), expires)
	return err
}

func (s *Store) LoadCapability(ctx context.Context, hash string) (contract.ExecutionScope, time.Time, *time.Time, error) {
	var scope contract.ExecutionScope
	var role string
	var expires time.Time
	var revoked *time.Time
	err := s.pool.QueryRow(ctx, `SELECT run_id,epoch_number,role,work_item_id,attempt_id,session_id,job_id,expires_at,revoked_at
		FROM http_job_capabilities WHERE token_hash=$1`, hash).Scan(
		&scope.RunID, &scope.EpochNumber, &role, &scope.WorkItemID, &scope.AttemptID, &scope.SessionID, &scope.JobID, &expires, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.ExecutionScope{}, time.Time{}, nil, contractError(contract.CodeUnauthorized, "proxy capability is not recognized")
	}
	scope.Role = contract.Role(role)
	return scope, expires, revoked, err
}

func (s *Store) RevokeCapabilities(ctx context.Context, scope contract.ExecutionScope, jobID *uuid.UUID) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE http_job_capabilities SET revoked_at=now()
		WHERE run_id=$1 AND epoch_number=$2 AND revoked_at IS NULL AND ($3::uuid IS NULL OR job_id=$3)`,
		scope.RunID, scope.EpochNumber, jobID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func sameUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func contractError(code contract.ErrorCode, message string) *contract.Error {
	return &contract.Error{Code: code, Scope: contract.ScopeRequest, Message: message, SuggestedAction: contract.ActionNone}
}
