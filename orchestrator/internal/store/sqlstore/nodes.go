package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/store/sqldb"
)

// The machines that run models we own, and the token they present to join
// (KB/35). Both belong to the PLATFORM: a machine is racked once and every
// workspace may have models on it.

type nodeStore struct{ db *sqldb.DB }

const nodeColumns = `id, node_id, name, base_url, key_enc, version, cert_expires_at, pinned_cert, created_at, updated_at`

func (s *nodeStore) List(ctx context.Context) ([]*model.InferenceNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeColumns+` FROM inference_nodes ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	defer func() { _ = rows.Close() }()

	nodes := []*model.InferenceNode{}
	for rows.Next() {
		n := &model.InferenceNode{}
		if err := rows.Scan(&n.ID, &n.NodeID, &n.Name, &n.BaseURL, &n.Key, &n.Version,
			&n.CertExpiresAt, &n.PinnedCert, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan machine: %w", err)
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func (s *nodeStore) Get(ctx context.Context, id int64) (*model.InferenceNode, error) {
	return scanNode(s.db.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM inference_nodes WHERE id = ?`, id))
}

// ByNodeID finds the machine that minted this id, which is how one that comes
// back is recognised as the same machine.
func (s *nodeStore) ByNodeID(ctx context.Context, nodeID string) (*model.InferenceNode, error) {
	if nodeID == "" {
		return nil, store.ErrNotFound
	}
	return scanNode(s.db.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM inference_nodes WHERE node_id = ?`, nodeID))
}

func (s *nodeStore) Create(ctx context.Context, n *model.InferenceNode) error {
	now := time.Now().UTC()
	n.CreatedAt, n.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO inference_nodes (node_id, name, base_url, key_enc, version, cert_expires_at, pinned_cert, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.NodeID, n.Name, n.BaseURL, n.Key, n.Version, n.CertExpiresAt, n.PinnedCert, n.CreatedAt, n.UpdatedAt)
	if err != nil {
		return wrapWriteErr("insert machine", err)
	}
	n.ID, err = res.LastInsertId()
	return err
}

// Update writes what a machine says about itself when it comes back: where it is
// now, what it calls itself, and its current key. Its id and the rows pointing
// at it are untouched, which is the point of matching on the node id at all.
func (s *nodeStore) Update(ctx context.Context, n *model.InferenceNode) error {
	n.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_nodes SET name = ?, base_url = ?, key_enc = ?, version = ?,
		 cert_expires_at = ?, pinned_cert = ?, updated_at = ?
		 WHERE id = ?`,
		n.Name, n.BaseURL, n.Key, n.Version, n.CertExpiresAt, n.PinnedCert, n.UpdatedAt, n.ID)
	if err != nil {
		return wrapWriteErr("update machine", err)
	}
	return requireAffected(res, "update machine")
}

// Delete removes a machine. Every vendor row pointing at it goes with it, and
// their models with those, which is the truth: the weights are on that machine.
func (s *nodeStore) Delete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM inference_nodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete machine: %w", err)
	}
	return requireAffected(res, "delete machine")
}

func scanNode(row *sql.Row) (*model.InferenceNode, error) {
	n := &model.InferenceNode{}
	err := row.Scan(&n.ID, &n.NodeID, &n.Name, &n.BaseURL, &n.Key, &n.Version,
		&n.CertExpiresAt, &n.PinnedCert, &n.CreatedAt, &n.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan machine: %w", err)
	}
	return n, nil
}

// nodeJoinStore holds the invitations machines present to join.
//
// One row per person, enforced by the unique key rather than by remembering to
// look first: asking again UPDATES, so nobody can accumulate credentials they
// have forgotten they hold.
type nodeJoinStore struct{ db *sqldb.DB }

func (s *nodeJoinStore) Mint(ctx context.Context, userID int64, sealed []byte, expiresAt, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO node_join_tokens (user_id, token_enc, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		   token_enc = VALUES(token_enc),
		   expires_at = VALUES(expires_at),
		   updated_at = VALUES(updated_at)`,
		userID, sealed, expiresAt.UTC(), now.UTC(), now.UTC())
	if err != nil {
		return wrapWriteErr("mint a join token", err)
	}
	return nil
}

func (s *nodeJoinStore) Live(ctx context.Context, now time.Time) ([]store.NodeJoinToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, token_enc, expires_at
		 FROM node_join_tokens
		 WHERE expires_at > ?
		 ORDER BY id`, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("read the join tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var live []store.NodeJoinToken
	for rows.Next() {
		var t store.NodeJoinToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Sealed, &t.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan a join token: %w", err)
		}
		live = append(live, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the join tokens: %w", err)
	}
	return live, nil
}

// Spend deletes the token and says whether this call is the one that did it.
//
// The expiry is repeated in the DELETE rather than trusted from the read that
// found it: a token can go stale between being matched and being spent, and the
// database is the only place that can decide that without a race.
func (s *nodeJoinStore) Spend(ctx context.Context, id int64, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM node_join_tokens WHERE id = ? AND expires_at > ?`, id, now.UTC())
	if err != nil {
		return false, wrapWriteErr("spend a join token", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("spend a join token: %w", err)
	}
	return affected == 1, nil
}

func (s *nodeJoinStore) Sweep(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM node_join_tokens WHERE expires_at <= ?`, now.UTC()); err != nil {
		return wrapWriteErr("sweep the join tokens", err)
	}
	return nil
}

// nodeAuthorityStore holds the ONE authority machines chain to.
//
// One row, like the join token, and for the same reason: it belongs to the
// deployment. What differs is that this one is never replaced in place, because
// replacing it orphans every machine holding a certificate from the old one.
type nodeAuthorityStore struct{ db *sqldb.DB }

func (s *nodeAuthorityStore) Get(ctx context.Context) ([]byte, []byte, error) {
	var sealed, certPEM []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT key_enc, cert_pem FROM node_authority WHERE id = 1`).Scan(&sealed, &certPEM)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, store.ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read the machine authority: %w", err)
	}
	return sealed, certPEM, nil
}

// Create writes the authority, once.
//
// A plain insert on a constant key, so two boots racing to make the first one
// end with one authority and a duplicate-key error, rather than with two and a
// fleet split between them. The caller reads the row back on that error.
func (s *nodeAuthorityStore) Create(ctx context.Context, sealedKey, certPEM []byte) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO node_authority (id, key_enc, cert_pem, created_at, updated_at)
		 VALUES (1, ?, ?, ?, ?)`,
		sealedKey, certPEM, now, now)
	if err != nil {
		return wrapWriteErr("create the machine authority", err)
	}
	return nil
}

// modelLibraryStore holds the ONE credential for fetching gated weights.
//
// One row, like the authority above, and for a related reason: it belongs to
// the deployment rather than to a machine. What differs is that this one IS
// replaced in place. It is somebody's credential with an outside service, it
// expires, and rotating it is the only thing anybody ever does with it, so an
// insert that refused to overwrite would refuse the whole point.
type modelLibraryStore struct{ db *sqldb.DB }

func (s *modelLibraryStore) Token(ctx context.Context) ([]byte, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT token_enc FROM model_library_credential WHERE id = 1`).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read the model library credential: %w", err)
	}
	return sealed, nil
}

// Describe answers what a screen needs without reading the secret.
//
// Deliberately a separate call from Token rather than a flag on it: a route
// that only wants to say "one is set" must not be holding the credential to
// find that out, because a value that is never in hand is a value that cannot
// be logged, returned or leaked by a later mistake.
func (s *modelLibraryStore) Describe(ctx context.Context) (*int64, time.Time, error) {
	var by sql.NullInt64
	var at time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT updated_by, updated_at FROM model_library_credential WHERE id = 1`).Scan(&by, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, store.ErrNotFound
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read the model library credential: %w", err)
	}
	if !by.Valid {
		return nil, at, nil
	}
	return &by.Int64, at, nil
}

func (s *modelLibraryStore) Set(ctx context.Context, sealed []byte, by int64) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_library_credential (id, token_enc, updated_by, created_at, updated_at)
		 VALUES (1, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE token_enc = VALUES(token_enc),
		   updated_by = VALUES(updated_by), updated_at = VALUES(updated_at)`,
		sealed, by, now, now)
	if err != nil {
		return wrapWriteErr("save the model library credential", err)
	}
	return nil
}

func (s *modelLibraryStore) Clear(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM model_library_credential WHERE id = 1`)
	if err != nil {
		return fmt.Errorf("clear the model library credential: %w", err)
	}
	return nil
}
