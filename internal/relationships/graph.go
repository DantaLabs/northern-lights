// Package relationships stores and traverses tenant-scoped provenance graphs.
package relationships

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

type Scope struct {
	ResourceIDs []string `json:"resource_ids,omitempty"`
	RootID      string   `json:"root_resource_id,omitempty"`
	Locator     string   `json:"locator,omitempty"`
}
type Canonical struct {
	JSON   string
	Digest string
}

func CanonicalScope(s Scope) (Canonical, error) {
	if (s.RootID == "") == (len(s.ResourceIDs) == 0) {
		return Canonical{}, errors.New("scope must specify resource_ids or lineage root")
	}
	if len(s.ResourceIDs) > 1000 {
		return Canonical{}, errors.New("resource_ids cap exceeded")
	}
	if s.RootID != "" && len(s.RootID) > 128 || len(s.Locator) > 512 {
		return Canonical{}, errors.New("scope bounds exceeded")
	}
	ids := append([]string(nil), s.ResourceIDs...)
	for _, id := range ids {
		if id == "" || len(id) > 128 {
			return Canonical{}, errors.New("invalid resource selector")
		}
	}
	sort.Strings(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return Canonical{}, errors.New("duplicate resource selector")
		}
	}
	raw, _ := json.Marshal(Scope{ResourceIDs: ids, RootID: s.RootID, Locator: s.Locator})
	h := sha256.Sum256(raw)
	return Canonical{string(raw), hex.EncodeToString(h[:])}, nil
}

type Node struct {
	ResourceID       string `json:"resource_id"`
	Kind             string `json:"kind"`
	ExternalID       string `json:"external_id"`
	Locator          string `json:"locator,omitempty"`
	Provider         string `json:"provider,omitempty"`
	ProviderRevision string `json:"provider_revision,omitempty"`
	Provenance       string `json:"provenance"`
	Confidence       string `json:"confidence"`
}
type Edge struct {
	ID         string   `json:"edge_id,omitempty"`
	From       string   `json:"from_resource_id"`
	To         string   `json:"to_resource_id"`
	Relation   string   `json:"relation"`
	Provenance string   `json:"provenance"`
	Confidence string   `json:"confidence"`
	Evidence   []string `json:"evidence_reference_ids,omitempty"`
}
type Discovery struct {
	Complete   bool
	EndOfScope bool
	Nodes      []Node
	Edges      []Edge
	Error      string
}
type Result struct {
	GraphVersion string
	Nodes        []Node
	Edges        []Edge
	Cycles       []string
	Omitted      []string
}

func (r Result) Authorizes(string, string) bool { return false }

const CapabilityRelationshipRead = "assurance.relationship.read"

func (s *Store) allowed(ctx context.Context, tenant, actor, resourceID string) (bool, error) {
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM assurance_relationship_allowlist_revisions r
 JOIN assurance_active_bundle_objects a
   ON a.tenant_id=r.tenant_id
  AND a.object_kind='relationship_allowlist'
  AND a.object_id=r.entry_id
  AND a.object_revision=r.revision
 WHERE r.tenant_id=? AND r.actor_id=? AND r.capability=? AND r.resource_id=?)`,
		tenant, actor, CapabilityRelationshipRead, resourceID).Scan(&allowed)
	return allowed, err
}

type Store struct {
	db    *sql.DB
	ready func() error
}

func NewStore(db *sql.DB, readiness ...func() error) (*Store, error) {
	if db == nil {
		return nil, errors.New("database required")
	}
	if len(readiness) > 1 {
		return nil, errors.New("at most one readiness check is supported")
	}
	store := &Store{db: db}
	if len(readiness) == 1 {
		store.ready = readiness[0]
	}
	return store, nil
}
func (s *Store) Refresh(ctx context.Context, tenant string, scope Scope, d Discovery, actor, audit string) (Result, error) {
	c, err := CanonicalScope(scope)
	if err != nil {
		return Result{}, err
	}
	if tenant == "" || actor == "" {
		return Result{}, errors.New("trusted tenant and actor required")
	}
	if err := validateDiscovery(d); err != nil {
		return Result{}, err
	}
	complete := d.Complete && d.EndOfScope && d.Error == ""
	if complete {
		selectors := map[string]bool{}
		if scope.RootID != "" {
			selectors[scope.RootID] = true
		}
		for _, id := range scope.ResourceIDs {
			selectors[id] = true
		}
		for _, edge := range d.Edges {
			if !validRelation(edge.Relation) || edge.Provenance == "inferred" && edge.Confidence == "high" || !selectors[edge.From] {
				return Result{}, errors.New("invalid or out-of-scope relationship")
			}
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Ensure a tenant version row exists, then lock it through the transaction's write.
	if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_relationship_graph_state(tenant_id,graph_version) VALUES(?,0) ON CONFLICT(tenant_id) DO NOTHING`, tenant); err != nil {
		return Result{}, err
	}
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT graph_version FROM assurance_relationship_graph_state WHERE tenant_id=?`, tenant).Scan(&version); err != nil {
		return Result{}, err
	}
	next := version
	if complete {
		next++
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	status, completeness := "partial", "incomplete"
	if complete {
		status, completeness = "completed", "complete"
	} else if d.Error != "" {
		status = "failed"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_relationship_discovery_runs(tenant_id,run_id,scope_digest,scope_json,status,completeness,provider_end_of_scope,page_count,cursor_count,error_json,prior_graph_version,resulting_graph_version,actor_id,audit_id,started_at,terminal_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, tenant, runID, c.Digest, c.JSON, status, completeness, boolInt(d.EndOfScope), 0, 0, d.Error, version, version, actor, audit, now, now); err != nil {
		return Result{}, err
	}
	if !complete {
		if err = tx.Commit(); err != nil {
			return Result{}, err
		}
		return Result{GraphVersion: fmt.Sprint(version)}, nil
	}
	// Compare-and-set the tenant graph version; a conflict rolls back the entire refresh.
	res, err := tx.ExecContext(ctx, `UPDATE assurance_relationship_graph_state SET graph_version=? WHERE tenant_id=? AND graph_version=?`, next, tenant, version)
	if err != nil {
		return Result{}, err
	}
	if count, _ := res.RowsAffected(); count != 1 {
		return Result{}, errors.New("optimistic graph version conflict")
	}
	_, err = tx.ExecContext(ctx, `UPDATE assurance_relationship_edges SET active=0 WHERE tenant_id=? AND scope_digest=? AND active=1`, tenant, c.Digest)
	if err != nil {
		return Result{}, err
	}
	for _, n := range d.Nodes {
		_, err = tx.ExecContext(ctx, `INSERT INTO assurance_resources(tenant_id,resource_id,provider,kind,external_id,parent_resource_id,locator,provider_revision,status) VALUES(?,?,?,?,?,?,?,?, 'active') ON CONFLICT(tenant_id,resource_id) DO UPDATE SET provider=excluded.provider,kind=excluded.kind,external_id=excluded.external_id,locator=excluded.locator,provider_revision=excluded.provider_revision,status='active'`, tenant, n.ResourceID, n.Provider, n.Kind, n.ExternalID, "", n.Locator, n.ProviderRevision)
		if err != nil {
			return Result{}, err
		}
	}
	for _, ed := range d.Edges {
		if ed.ID == "" {
			h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s", c.Digest, ed.From, ed.To, ed.Relation)))
			ed.ID = hex.EncodeToString(h[:16])
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO assurance_relationship_edges(tenant_id,edge_id,source_resource_id,target_resource_id,relation,provenance,graph_version,active,policy_version,created_at,scope_digest) VALUES(?,?,?,?,?,?,?,1,'',?,?) ON CONFLICT(tenant_id,edge_id) DO UPDATE SET source_resource_id=excluded.source_resource_id,target_resource_id=excluded.target_resource_id,relation=excluded.relation,provenance=excluded.provenance,graph_version=excluded.graph_version,active=1,created_at=excluded.created_at,scope_digest=excluded.scope_digest`, tenant, ed.ID, ed.From, ed.To, ed.Relation, ed.Provenance, next, now, c.Digest)
		if err != nil {
			return Result{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE assurance_relationship_discovery_runs SET resulting_graph_version=? WHERE tenant_id=? AND run_id=?`, next, tenant, runID); err != nil {
		return Result{}, err
	}
	if err = tx.Commit(); err != nil {
		return Result{}, err
	}
	return Result{GraphVersion: fmt.Sprint(next)}, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func validateDiscovery(d Discovery) error {
	for _, n := range d.Nodes {
		if n.ResourceID == "" || len(n.ResourceID) > 128 || n.ExternalID == "" || len(n.ExternalID) > 512 {
			return errors.New("invalid discovery node identity bounds")
		}
		if len(n.Kind) == 0 || len(n.Kind) > 64 || !validResourceKind(n.Kind) {
			return errors.New("invalid discovery node kind")
		}
		if len(n.Locator) > 512 || len(n.Provider) > 128 || len(n.ProviderRevision) > 256 {
			return errors.New("invalid discovery node bounds")
		}
		if n.Provenance != "" && !validProvenance(n.Provenance) {
			return errors.New("invalid discovery node provenance")
		}
		if n.Confidence != "" && !validConfidence(n.Confidence) {
			return errors.New("invalid discovery node confidence")
		}
	}
	return nil
}

func validResourceKind(kind string) bool {
	switch kind {
	case "spreadsheet", "sheet", "document", "document_table", "range", "mapping", "report":
		return true
	default:
		return false
	}
}

func validProvenance(value string) bool {
	switch value {
	case "observed", "provider_declared", "operator", "inferred":
		return true
	default:
		return false
	}
}

func validConfidence(value string) bool {
	switch value {
	case "high", "medium", "low", "unavailable":
		return true
	default:
		return false
	}
}

func validRelation(r string) bool {
	switch r {
	case "contains", "maps_to", "derived_from", "copied_to", "validated_by", "reported_in", "references":
		return true
	}
	return false
}
func (s *Store) Traverse(ctx context.Context, tenant, actor, root, locator, direction string, maxDepth, maxNodes, maxEdges int) (Result, error) {
	if s.ready != nil {
		if err := s.ready(); err != nil {
			return Result{}, err
		}
	}
	if tenant == "" || actor == "" || root == "" {
		return Result{}, errors.New("trusted tenant, actor, and root required")
	}
	rootAllowed, err := s.allowed(ctx, tenant, actor, root)
	if err != nil {
		return Result{}, err
	}
	if !rootAllowed {
		return Result{}, errors.New("relationship root denied by resource allowlist")
	}
	if direction == "" {
		direction = "both"
	}
	if direction != "both" && direction != "upstream" && direction != "downstream" {
		return Result{}, errors.New("invalid direction")
	}
	if maxDepth < 0 || maxDepth > 8 || maxNodes < 1 || maxNodes > 1000 || maxEdges < 1 || maxEdges > 5000 {
		return Result{}, errors.New("invalid traversal bounds")
	}
	out := Result{Nodes: []Node{}, Edges: []Edge{}, Cycles: []string{}, Omitted: []string{}}
	type step struct {
		id    string
		depth int
	}
	q := []step{{root, 0}}
	visited := map[string]bool{}
	seenEdges := map[string]bool{}
	active := map[string]bool{}
	nodes := map[string]Node{}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		if active[cur.id] {
			out.Cycles = append(out.Cycles, cur.id)
			continue
		}
		if visited[cur.id] {
			continue
		}
		if len(nodes) >= maxNodes {
			out.Omitted = append(out.Omitted, cur.id)
			continue
		}
		allowed, allowErr := s.allowed(ctx, tenant, actor, cur.id)
		if allowErr != nil {
			return Result{}, allowErr
		}
		if !allowed {
			out.Omitted = append(out.Omitted, cur.id+":denied")
			continue
		}
		visited[cur.id] = true
		active[cur.id] = true
		var n Node
		err := s.db.QueryRowContext(ctx, `SELECT resource_id,kind,external_id,locator,provider,provider_revision FROM assurance_resources WHERE tenant_id=? AND resource_id=? AND status='active'`, tenant, cur.id).Scan(&n.ResourceID, &n.Kind, &n.ExternalID, &n.Locator, &n.Provider, &n.ProviderRevision)
		if err == nil {
			n.Provenance = "observed"
			n.Confidence = "high"
			if cur.id == root && locator != "" && locator != n.Locator {
				return Result{}, errors.New("root locator outside authorized scope")
			}
			nodes[n.ResourceID] = n
		} else if !errors.Is(err, sql.ErrNoRows) {
			return Result{}, err
		}
		if cur.depth < maxDepth {
			rows, e := s.db.QueryContext(ctx, `SELECT edge_id,source_resource_id,target_resource_id,relation,provenance FROM assurance_relationship_edges WHERE tenant_id=? AND active=1 AND ((? IN ('both','downstream') AND source_resource_id=?) OR (? IN ('both','upstream') AND target_resource_id=?))`, tenant, direction, cur.id, direction, cur.id)
			if e != nil {
				return Result{}, e
			}
			var candidates []Edge
			for rows.Next() {
				var ed Edge
				if e = rows.Scan(&ed.ID, &ed.From, &ed.To, &ed.Relation, &ed.Provenance); e != nil {
					_ = rows.Close()
					return Result{}, e
				}
				candidates = append(candidates, ed)
			}
			if e = rows.Err(); e != nil {
				_ = rows.Close()
				return Result{}, e
			}
			if e = rows.Close(); e != nil {
				return Result{}, e
			}
			for _, ed := range candidates {
				fromAllowed, fromErr := s.allowed(ctx, tenant, actor, ed.From)
				toAllowed, toErr := s.allowed(ctx, tenant, actor, ed.To)
				if fromErr != nil {
					return Result{}, fromErr
				}
				if toErr != nil {
					return Result{}, toErr
				}
				if !fromAllowed || !toAllowed {
					deniedID := ed.From
					if !toAllowed {
						deniedID = ed.To
					}
					out.Omitted = append(out.Omitted, deniedID+":denied")
					continue
				}
				if seenEdges[ed.ID] {
					continue
				}
				seenEdges[ed.ID] = true
				if len(out.Edges) >= maxEdges {
					out.Omitted = append(out.Omitted, cur.id)
					break
				}
				out.Edges = append(out.Edges, ed)
				next := ed.To
				if next == cur.id {
					next = ed.From
				}
				if active[next] || visited[next] {
					out.Cycles = append(out.Cycles, next)
				} else {
					q = append(q, step{next, cur.depth + 1})
				}
			}
		}
		delete(active, cur.id)
	}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, n)
	}
	// Detect directed cycles on the bounded returned edge set.
	adj := map[string][]string{}
	for _, edge := range out.Edges {
		adj[edge.From] = append(adj[edge.From], edge.To)
	}
	state := map[string]uint8{}
	var walk func(string)
	walk = func(id string) {
		state[id] = 1
		for _, to := range adj[id] {
			switch state[to] {
			case 1:
				out.Cycles = append(out.Cycles, to)
			case 0:
				walk(to)
			}
		}
		state[id] = 2
	}
	walk(root)
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ResourceID < out.Nodes[j].ResourceID })
	return out, nil
}
