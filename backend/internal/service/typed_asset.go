package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// DeriveTypedAsset records canonical identities of an already authorized target.
// It does not probe a host, infer an IP from DNS, or authorize another origin.
func (s *RunService) DeriveTypedAsset(ctx context.Context, runID, parentID uuid.UUID, kind, identity string) (map[string]any, error) {
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
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&status); err != nil {
		return nil, err
	}
	if status != "running" {
		return nil, ErrInvalidState
	}
	var raw []byte
	var key, parentKind string
	if err = tx.QueryRow(ctx, `SELECT ra.snapshot,a.external_key,a.kind FROM run_assets ra JOIN assets a ON a.id=ra.asset_id WHERE ra.run_id=$1 AND ra.asset_id=$2`, runID, parentID).Scan(&raw, &key, &parentKind); err != nil {
		return nil, ErrAssetOutOfScope
	}
	if parentKind != "http_endpoint" {
		return nil, fmt.Errorf("typed derivation requires an authorized HTTP endpoint")
	}
	var snapshot struct {
		URL      string `json:"url"`
		Metadata struct {
			URL string `json:"url"`
		} `json:"metadata"`
	}
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	target := snapshot.Metadata.URL
	if target == "" {
		target = snapshot.URL
	}
	if target == "" {
		target = strings.TrimPrefix(key, "GET ")
	}
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("parent has no canonical target URL")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	canonical, relation := "", "contains"
	switch kind {
	case "host":
		canonical, relation = host, "hosts"
		identity = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(identity)), ".")
	case "ip":
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return nil, fmt.Errorf("a domain does not authorize an inferred IP; supply an explicit IP-scoped target")
		}
		canonical, relation = ip.Unmap().String(), "hosts"
		if identity != "" {
			parsed, err := netip.ParseAddr(identity)
			if err != nil {
				return nil, err
			}
			identity = parsed.Unmap().String()
		}
	case "service":
		canonical, relation = net.JoinHostPort(host, port), "serves"
		if identity != "" {
			h, p, err := net.SplitHostPort(identity)
			if err != nil {
				return nil, err
			}
			identity = net.JoinHostPort(strings.TrimSuffix(strings.ToLower(h), "."), p)
		}
	case "app":
		originHost := host
		if strings.Contains(host, ":") {
			originHost = "[" + host + "]"
		}
		if (u.Scheme == "http" && port != "80") || (u.Scheme == "https" && port != "443") {
			originHost = net.JoinHostPort(host, port)
		}
		canonical = (&url.URL{Scheme: strings.ToLower(u.Scheme), Host: originHost}).String()
		identity = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(identity)), "/")
	default:
		return nil, fmt.Errorf("supported derived kinds: app,host,ip,service")
	}
	if identity != "" && identity != canonical {
		return nil, ErrAssetOutOfScope
	}
	meta, _ := json.Marshal(map[string]any{"canonical_identity": canonical, "kind": kind})
	var assetID uuid.UUID
	if err = tx.QueryRow(ctx, `INSERT INTO assets(namespace,kind,external_key,name,description,metadata) VALUES('typed',$1,$2,$2,'Canonical identity derived from authorized scope',$3) ON CONFLICT(namespace,kind,external_key) DO UPDATE SET updated_at=now() RETURNING id`, kind, canonical, meta).Scan(&assetID); err != nil {
		return nil, err
	}
	snap, _ := json.Marshal(map[string]any{"kind": kind, "external_key": canonical, "metadata": json.RawMessage(meta), "origin": "scope_identity_derivation", "parent_asset_id": parentID, "epoch_number": epoch})
	if _, err = tx.Exec(ctx, `INSERT INTO run_assets(run_id,asset_id,snapshot) VALUES($1,$2,$3) ON CONFLICT(run_id,asset_id) DO NOTHING`, runID, assetID, snap); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO asset_relations(source_asset_id,target_asset_id,relation_type,metadata) VALUES($1,$2,$3,'{"origin":"scope_identity_derivation"}') ON CONFLICT(source_asset_id,target_asset_id,relation_type) DO NOTHING`, assetID, parentID, relation); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"asset_id": assetID, "parent_asset_id": parentID, "kind": kind, "identity": canonical, "relation": relation, "origin": "scope_identity_derivation"}, nil
}
