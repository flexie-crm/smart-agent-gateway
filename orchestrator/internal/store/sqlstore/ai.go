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

type vendorStore struct{ db *sqldb.DB }

// A vendor is read WITH the machine it points at, when it points at one.
//
// This is the whole reason nothing above the store had to change for machines.
// A vendor row that carries a node id has no address and no key of its own; both
// live on the machine, once. Resolving that here means the gateway, the model
// registry and every screen go on seeing an ordinary vendor with an address and
// a sealed key, and none of them has to know what a machine is.
const vendorColumns = `v.id, v.workspace_id, v.vendor_key, v.name,
	COALESCE(n.base_url, v.base_url), v.node_id,
	COALESCE(n.key_enc, v.credentials_enc),
	v.status, v.settings, v.created_at, v.updated_at`

const vendorFrom = ` FROM ai_vendors v LEFT JOIN inference_nodes n ON n.id = v.node_id `

// The store persists credentials exactly as handed to it: sealing happens in
// the app layer, so no plaintext secret ever reaches SQL.
func (s *vendorStore) Create(ctx context.Context, v *model.AIVendor) error {
	now := time.Now().UTC()
	v.CreatedAt, v.UpdatedAt = now, now
	if v.Status == "" {
		v.Status = model.StatusActive
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ai_vendors
		 (workspace_id, vendor_key, name, base_url, node_id, credentials_enc, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.WorkspaceID, v.VendorKey, v.Name, nullString(v.BaseURL), nullID(v.NodeID),
		nullBytes(v.Credentials), v.Status, v.CreatedAt, v.UpdatedAt)
	if err != nil {
		return wrapWriteErr("insert vendor", err)
	}
	v.ID, err = res.LastInsertId()
	return err
}

func (s *vendorStore) GetByID(ctx context.Context, workspaceID, id int64) (*model.AIVendor, error) {
	return scanVendor(s.db.QueryRowContext(ctx,
		`SELECT `+vendorColumns+vendorFrom+`WHERE v.id = ? AND v.workspace_id = ?`, id, workspaceID))
}

func (s *vendorStore) ByNodeID(ctx context.Context, workspaceID, nodeID int64) (*model.AIVendor, error) {
	if nodeID == 0 {
		// Every hosted vendor has no machine, so a zero would match the first of
		// them and hand a machine somebody else's row.
		return nil, store.ErrNotFound
	}
	return scanVendor(s.db.QueryRowContext(ctx,
		`SELECT `+vendorColumns+vendorFrom+`WHERE v.workspace_id = ? AND v.node_id = ?`,
		workspaceID, nodeID))
}

func (s *vendorStore) List(ctx context.Context, workspaceID int64) ([]*model.AIVendor, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+vendorColumns+vendorFrom+`WHERE v.workspace_id = ? ORDER BY v.id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list vendors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	vendors := []*model.AIVendor{}
	for rows.Next() {
		v, err := scanVendorRow(rows)
		if err != nil {
			return nil, err
		}
		vendors = append(vendors, v)
	}
	return vendors, rows.Err()
}

// Update writes credentials only when a new sealed value is supplied: a
// caller editing a vendor's name must not have to resend the secret, and an
// absent secret must never blank the stored one.
func (s *vendorStore) Update(ctx context.Context, v *model.AIVendor) error {
	if err := requireExists(ctx, s.db, "update vendor",
		`SELECT 1 FROM ai_vendors WHERE id = ? AND workspace_id = ?`, v.ID, v.WorkspaceID); err != nil {
		return err
	}
	v.UpdatedAt = time.Now().UTC()

	// Two statements rather than one assembled from pieces. Whether the secret
	// is being replaced is a decision, and a decision belongs in Go, not in a
	// string that grows a clause.
	if v.Credentials == nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE ai_vendors SET name = ?, base_url = ?, status = ?, updated_at = ?
			 WHERE id = ? AND workspace_id = ?`,
			v.Name, nullString(v.BaseURL), v.Status, v.UpdatedAt, v.ID, v.WorkspaceID); err != nil {
			return wrapWriteErr("update vendor", err)
		}
		return nil
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE ai_vendors SET name = ?, base_url = ?, status = ?, updated_at = ?, credentials_enc = ?
		 WHERE id = ? AND workspace_id = ?`,
		v.Name, nullString(v.BaseURL), v.Status, v.UpdatedAt, v.Credentials,
		v.ID, v.WorkspaceID); err != nil {
		return wrapWriteErr("update vendor", err)
	}
	return nil
}

// ClearCredentials removes the stored secret without deleting the vendor.
func (s *vendorStore) ClearCredentials(ctx context.Context, workspaceID, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ai_vendors SET credentials_enc = NULL, updated_at = ?
		 WHERE id = ? AND workspace_id = ? AND credentials_enc IS NOT NULL`,
		time.Now().UTC(), id, workspaceID)
	if err != nil {
		return fmt.Errorf("clear vendor credentials: %w", err)
	}
	// Nothing to clear is not an error, but a missing vendor is.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return requireExists(ctx, s.db, "clear vendor credentials",
			`SELECT 1 FROM ai_vendors WHERE id = ? AND workspace_id = ?`, id, workspaceID)
	}
	return nil
}

// Delete refuses while models still point at the vendor: an orphaned model
// would reference credentials that no longer exist.
func (s *vendorStore) Delete(ctx context.Context, workspaceID, id int64) error {
	var models int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ai_models WHERE vendor_id = ? AND workspace_id = ?`, id, workspaceID).
		Scan(&models)
	if err != nil {
		return fmt.Errorf("count vendor models: %w", err)
	}
	if models > 0 {
		return store.ErrInUse
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ai_vendors WHERE id = ? AND workspace_id = ?`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete vendor: %w", err)
	}
	return requireAffected(res, "delete vendor")
}

func scanVendor(row *sql.Row) (*model.AIVendor, error) {
	v := &model.AIVendor{}
	var baseURL sql.NullString
	var nodeID sql.NullInt64
	var settings []byte
	err := row.Scan(&v.ID, &v.WorkspaceID, &v.VendorKey, &v.Name, &baseURL, &nodeID, &v.Credentials,
		&v.Status, &settings, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan vendor: %w", err)
	}
	v.BaseURL, v.NodeID = baseURL.String, nodeID.Int64
	return v, nil
}

func scanVendorRow(rows *sql.Rows) (*model.AIVendor, error) {
	v := &model.AIVendor{}
	var baseURL sql.NullString
	var nodeID sql.NullInt64
	var settings []byte
	if err := rows.Scan(&v.ID, &v.WorkspaceID, &v.VendorKey, &v.Name, &baseURL, &nodeID, &v.Credentials,
		&v.Status, &settings, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return nil, fmt.Errorf("scan vendor: %w", err)
	}
	v.BaseURL, v.NodeID = baseURL.String, nodeID.Int64
	v.Settings = model.ParseSettings(settings)
	return v, nil
}

type aiModelStore struct{ db *sqldb.DB }

const aiModelColumns = `id, workspace_id, vendor_id, model_key, type, context_window,
	description, input_price_per_1m, output_price_per_1m, status, settings, created_at, updated_at`

// Create writes a model, filling in the defaults its enums declare.
//
// An empty string is not a value in an enum, and passing one explicitly does not
// fall back to the column's DEFAULT: it overrides it. On a strict server that is
// an error; on a lenient one it silently stores ”, and nothing ever notices
// until something reads it back and finds a model with no type. The defaults
// belong here, next to the write, because that is the last place they can still
// be applied.
func (s *aiModelStore) Create(ctx context.Context, m *model.AIModel) error {
	now := time.Now().UTC()
	m.CreatedAt, m.UpdatedAt = now, now
	if m.Status == "" {
		m.Status = model.StatusActive
	}
	if m.Type == "" {
		m.Type = model.ModelTypeChat
	}
	settings, err := m.Settings.Marshal()
	if err != nil {
		return fmt.Errorf("insert model: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ai_models
		 (workspace_id, vendor_id, model_key, type, context_window, description,
		  input_price_per_1m, output_price_per_1m, status, settings, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.WorkspaceID, m.VendorID, m.ModelKey, m.Type, m.ContextWindow, m.Description,
		m.InputPricePer1M, m.OutputPricePer1M, m.Status, nullBytes(settings),
		m.CreatedAt, m.UpdatedAt)
	if err != nil {
		return wrapWriteErr("insert model", err)
	}
	m.ID, err = res.LastInsertId()
	return err
}

func (s *aiModelStore) GetByID(ctx context.Context, workspaceID, id int64) (*model.AIModel, error) {
	return scanAIModel(s.db.QueryRowContext(ctx,
		`SELECT `+aiModelColumns+` FROM ai_models WHERE id = ? AND workspace_id = ?`, id, workspaceID))
}

func (s *aiModelStore) List(ctx context.Context, workspaceID int64) ([]*model.AIModel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+aiModelColumns+` FROM ai_models WHERE workspace_id = ? ORDER BY id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer func() { _ = rows.Close() }()

	models := []*model.AIModel{}
	for rows.Next() {
		m := &model.AIModel{}
		var description sql.NullString
		var settings []byte
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.VendorID, &m.ModelKey, &m.Type,
			&m.ContextWindow, &description, &m.InputPricePer1M, &m.OutputPricePer1M, &m.Status,
			&settings, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		m.Description = description.String
		m.Settings = model.ParseSettings(settings)
		models = append(models, m)
	}
	return models, rows.Err()
}

func (s *aiModelStore) Update(ctx context.Context, m *model.AIModel) error {
	if err := requireExists(ctx, s.db, "update model",
		`SELECT 1 FROM ai_models WHERE id = ? AND workspace_id = ?`, m.ID, m.WorkspaceID); err != nil {
		return err
	}
	settings, err := m.Settings.Marshal()
	if err != nil {
		return fmt.Errorf("update model: %w", err)
	}
	m.UpdatedAt = time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		`UPDATE ai_models SET vendor_id = ?, model_key = ?, type = ?, context_window = ?,
		 description = ?, input_price_per_1m = ?, output_price_per_1m = ?, status = ?,
		 settings = ?, updated_at = ?
		 WHERE id = ? AND workspace_id = ?`,
		m.VendorID, m.ModelKey, m.Type, m.ContextWindow, m.Description, m.InputPricePer1M,
		m.OutputPricePer1M, m.Status, nullBytes(settings),
		m.UpdatedAt, m.ID, m.WorkspaceID)
	if err != nil {
		return wrapWriteErr("update model", err)
	}
	return nil
}

func (s *aiModelStore) Delete(ctx context.Context, workspaceID, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ai_models WHERE id = ? AND workspace_id = ?`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	return requireAffected(res, "delete model")
}

func scanAIModel(row *sql.Row) (*model.AIModel, error) {
	m := &model.AIModel{}
	var description sql.NullString
	var settings []byte
	err := row.Scan(&m.ID, &m.WorkspaceID, &m.VendorID, &m.ModelKey, &m.Type,
		&m.ContextWindow, &description, &m.InputPricePer1M, &m.OutputPricePer1M, &m.Status,
		&settings, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan model: %w", err)
	}
	m.Description = description.String
	m.Settings = model.ParseSettings(settings)
	return m, nil
}

func nullBytes(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return v
}
