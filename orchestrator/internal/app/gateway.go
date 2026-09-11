package app

import (
	"context"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// storeLookup adapts the store to the narrow view the gateway needs, so the
// gateway depends on two methods rather than on the whole persistence layer.
type storeLookup struct {
	store store.Store
}

func (s *storeLookup) AIModel(ctx context.Context, workspaceID, modelID int64) (*model.AIModel, error) {
	return s.store.AIModels().GetByID(ctx, workspaceID, modelID)
}

func (s *storeLookup) Vendor(ctx context.Context, workspaceID, vendorID int64) (*model.AIVendor, error) {
	return s.store.Vendors().GetByID(ctx, workspaceID, vendorID)
}
