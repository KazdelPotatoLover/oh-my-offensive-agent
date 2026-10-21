package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const MaxArtifactBytes = 8 * 1024 * 1024

type Artifact struct {
	ID               uuid.UUID  `json:"id"`
	RunID            uuid.UUID  `json:"run_id"`
	WorkItemID       *uuid.UUID `json:"work_item_id,omitempty"`
	SessionID        *uuid.UUID `json:"session_id,omitempty"`
	SourceToolCallID *uuid.UUID `json:"source_tool_call_id,omitempty"`
	Epoch            int        `json:"epoch_number"`
	Name             string     `json:"name"`
	MediaType        string     `json:"media_type"`
	SHA256           string     `json:"sha256"`
	ByteLength       int        `json:"byte_length"`
	CreatedAt        time.Time  `json:"created_at"`
}

func (s *RunService) SetWorkspaceRoot(root string) { s.workspaceRoot = root }

func workspaceRel(runID uuid.UUID, workID *uuid.UUID) string {
	rel := filepath.Join("runs", runID.String())
	if workID != nil {
		rel = filepath.Join(rel, "work", workID.String())
	}
	return rel
}

func (s *RunService) EnsureWorkspace(ctx context.Context, runID uuid.UUID, workID *uuid.UUID) (string, error) {
	var valid bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs r WHERE r.id=$1 AND ($2::uuid IS NULL OR EXISTS(SELECT 1 FROM work_items w WHERE w.id=$2 AND w.run_id=r.id)))`, runID, workID).Scan(&valid)
	if err != nil {
		return "", err
	}
	if !valid {
		return "", ErrCrossRunBoundary
	}
	if err = os.MkdirAll(s.workspaceRoot, 0700); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(s.workspaceRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()
	rel := workspaceRel(runID, workID)
	probe := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		probe = filepath.Join(probe, part)
		info, err := root.Lstat(probe)
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("workspace symlinks are not allowed")
		}
	}
	if err = root.MkdirAll(rel, 0700); err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(s.workspaceRoot, rel))
}

func (s *RunService) SaveArtifact(ctx context.Context, runID uuid.UUID, workID, sessionID, sourceID *uuid.UUID, name, mediaType, content string) (*Artifact, error) {
	if len(content) > MaxArtifactBytes || !utf8.ValidString(content) || name == "" || len(name) > 128 || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		return nil, fmt.Errorf("invalid artifact name/content or 8 MiB limit exceeded")
	}
	if mediaType == "" {
		mediaType = "text/plain"
	}
	if len(mediaType) > 120 {
		return nil, fmt.Errorf("invalid media type")
	}
	if _, err := s.EnsureWorkspace(ctx, runID, workID); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var epoch int
	if err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&epoch); err != nil {
		return nil, err
	}
	if err := validateContextEpoch(ctx, epoch); err != nil {
		return nil, err
	}
	if sessionID != nil {
		var valid bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_sessions WHERE id=$1 AND run_id=$2 AND epoch_number=$3 AND work_item_id IS NOT DISTINCT FROM $4::uuid)`, sessionID, runID, epoch, workID).Scan(&valid); err != nil {
			return nil, err
		}
		if !valid {
			return nil, ErrStaleEpoch
		}
	}
	if sourceID != nil {
		var valid bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_tool_calls WHERE id=$1 AND run_id=$2 AND session_id IS NOT DISTINCT FROM $3::uuid AND epoch_number=$4 AND output=$5)`, sourceID, runID, sessionID, epoch, content).Scan(&valid); err != nil {
			return nil, err
		}
		if !valid {
			return nil, ErrInvalidEvidence
		}
	}
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	var a Artifact
	err = tx.QueryRow(ctx, `INSERT INTO artifacts(run_id,work_item_id,session_id,source_tool_call_id,epoch_number,name,media_type,content,sha256,byte_length)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(source_tool_call_id) DO UPDATE SET source_tool_call_id=EXCLUDED.source_tool_call_id
		RETURNING id,run_id,work_item_id,session_id,source_tool_call_id,epoch_number,name,media_type,sha256,byte_length,created_at`, runID, workID, sessionID, sourceID, epoch, name, mediaType, content, hash, len(content)).Scan(&a.ID, &a.RunID, &a.WorkItemID, &a.SessionID, &a.SourceToolCallID, &a.Epoch, &a.Name, &a.MediaType, &a.SHA256, &a.ByteLength, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	// The immutable database body is authoritative. Disk is a reusable workspace
	// projection and can always be regenerated after restart or volume loss.
	root, err := os.OpenRoot(s.workspaceRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rel := filepath.Join(workspaceRel(runID, workID), a.ID.String()+"-"+a.Name)
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		return &a, nil
	}
	if err != nil {
		return nil, err
	}
	_, writeErr := f.WriteString(content)
	closeErr := f.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return &a, nil
}

func (s *RunService) SaveToolArtifact(ctx context.Context, runID, sessionID, callID uuid.UUID, name, content string) (*Artifact, error) {
	var workID *uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT work_item_id FROM agent_sessions WHERE id=$1 AND run_id=$2`, sessionID, runID).Scan(&workID); err != nil {
		return nil, err
	}
	return s.SaveArtifact(ctx, runID, workID, &sessionID, &callID, name, "text/plain", content)
}

func (s *RunService) ListArtifacts(ctx context.Context, runID uuid.UUID, workID *uuid.UUID, offset, limit int) ([]Artifact, error) {
	if offset < 0 {
		return nil, fmt.Errorf("invalid offset")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT id,run_id,work_item_id,session_id,source_tool_call_id,epoch_number,name,media_type,sha256,byte_length,created_at FROM artifacts WHERE run_id=$1 AND ($2::uuid IS NULL OR work_item_id=$2) ORDER BY created_at,id OFFSET $3 LIMIT $4`, runID, workID, offset, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := []Artifact{}
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.RunID, &a.WorkItemID, &a.SessionID, &a.SourceToolCallID, &a.Epoch, &a.Name, &a.MediaType, &a.SHA256, &a.ByteLength, &a.CreatedAt); err != nil {
			return nil, err
		}
		res = append(res, a)
	}
	return res, rows.Err()
}

func (s *RunService) ReadArtifact(ctx context.Context, runID, artifactID uuid.UUID, offset, limit int) (map[string]any, error) {
	if offset < 0 {
		return nil, fmt.Errorf("invalid byte offset")
	}
	if limit <= 0 || limit > 32768 {
		limit = 8192
	}
	var content, hash, name string
	var owner uuid.UUID
	var sourceID *uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT run_id,content,sha256,name,source_tool_call_id FROM artifacts WHERE id=$1`, artifactID).Scan(&owner, &content, &hash, &name, &sourceID); err != nil {
		return nil, err
	}
	if owner != runID {
		return nil, ErrCrossRunBoundary
	}
	if offset > len(content) || (offset < len(content) && !utf8.RuneStart(content[offset])) {
		return nil, fmt.Errorf("invalid byte offset")
	}
	end := min(len(content), offset+limit)
	for end > offset && end < len(content) && !utf8.RuneStart(content[end]) {
		end--
	}
	if end == offset && offset < len(content) {
		return nil, fmt.Errorf("limit cannot hold a UTF-8 character")
	}
	return map[string]any{"artifact_id": artifactID, "name": name, "sha256": hash, "source_tool_call_id": sourceID, "offset": offset, "next_offset": end, "total_bytes": len(content), "has_more": end < len(content), "content": content[offset:end]}, nil
}

func (s *RunService) GetWorkerOutput(ctx context.Context, runID, workID, stepID uuid.UUID, offset, limit int) (map[string]any, error) {
	var owner uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT run_id FROM work_items WHERE id=$1`, workID).Scan(&owner); err != nil {
		return nil, err
	}
	if owner != runID {
		return nil, ErrCrossRunBoundary
	}
	var artifactID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT a.id FROM artifacts a WHERE a.run_id=$1 AND a.work_item_id=$2 AND a.source_tool_call_id=$3`, runID, workID, stepID).Scan(&artifactID)
	if err == pgx.ErrNoRows {
		var content, status string
		if err := s.pool.QueryRow(ctx, `SELECT COALESCE(tc.output,''),tc.status FROM agent_tool_calls tc JOIN agent_sessions sess ON sess.id=tc.session_id WHERE tc.id=$1 AND tc.run_id=$2 AND sess.work_item_id=$3`, stepID, runID, workID).Scan(&content, &status); err != nil {
			return nil, err
		}
		if offset < 0 || offset > len(content) || (offset < len(content) && !utf8.RuneStart(content[offset])) {
			return nil, fmt.Errorf("invalid byte offset")
		}
		if limit <= 0 || limit > 32768 {
			limit = 8192
		}
		end := min(len(content), offset+limit)
		for end > offset && end < len(content) && !utf8.RuneStart(content[end]) {
			end--
		}
		hash := sha256.Sum256([]byte(content))
		return map[string]any{"step_id": stepID, "work_item_id": workID, "source_tool_call_id": stepID, "status": status, "content": content[offset:end], "next_offset": end, "total_bytes": len(content), "has_more": end < len(content), "sha256": hex.EncodeToString(hash[:]), "read_tool": "get_worker_output"}, nil
	}
	if err != nil {
		return nil, err
	}
	return s.ReadArtifact(ctx, runID, artifactID, offset, limit)
}
