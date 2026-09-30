package storetest

import (
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// Who made it, across every table that holds something somebody decided.
//
// One test over many tables rather than one per table, because the question is
// the same everywhere and the failure would be too: a column missed in a write
// (the row records nobody) or missed in a read (it reads as nobody whatever was
// written). Both look identical on a screen, and both are invisible until
// somebody asks who did something.
//
// The tables are covered here rather than in each subsystem's own test because
// this is not a fact about vendors or roles; it is one rule applied nineteen
// times, and a rule tested in one place per table drifts.
func testEverythingRecordsWhoDecidedIt(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "authored")
	maker := model.Actor{UserID: mustUser(t, st, ws.ID, "maker@acme.test").ID, Name: "The Maker"}
	editor := model.Actor{UserID: mustUser(t, st, ws.ID, "editor@acme.test").ID, Name: "The Editor"}

	// Each case makes a row as one person and changes it as another, then reads
	// it back: created must be the first and updated the second, because those
	// are two different facts and the commonest bug is one overwriting the other.
	cases := []struct {
		what   string
		make   func() (model.Authored, model.Edited)
		change func() (model.Authored, model.Edited)
	}{
		{
			what: "a group",
			make: func() (model.Authored, model.Edited) {
				g := &model.Group{WorkspaceID: ws.ID, Name: "Support"}
				if err := st.Groups().Create(ctx(), g, maker); err != nil {
					t.Fatalf("create group: %v", err)
				}
				read, err := st.Groups().GetByID(ctx(), ws.ID, g.ID)
				if err != nil {
					t.Fatalf("read group: %v", err)
				}
				return read.Authored, read.Edited
			},
			change: func() (model.Authored, model.Edited) {
				groups, err := st.Groups().List(ctx(), ws.ID)
				if err != nil || len(groups) == 0 {
					t.Fatalf("list groups: %v", err)
				}
				g := groups[0]
				g.Name = "Support, renamed"
				if err := st.Groups().Update(ctx(), g, editor); err != nil {
					t.Fatalf("update group: %v", err)
				}
				read, err := st.Groups().GetByID(ctx(), ws.ID, g.ID)
				if err != nil {
					t.Fatalf("read group: %v", err)
				}
				return read.Authored, read.Edited
			},
		},
		{
			what: "a role",
			make: func() (model.Authored, model.Edited) {
				r := &model.Role{WorkspaceID: ws.ID, Name: "Curator", Permissions: []string{model.PermBrainsView}}
				if err := st.Roles().Create(ctx(), r, maker); err != nil {
					t.Fatalf("create role: %v", err)
				}
				read, err := st.Roles().GetByID(ctx(), ws.ID, r.ID)
				if err != nil {
					t.Fatalf("read role: %v", err)
				}
				return read.Authored, read.Edited
			},
			change: func() (model.Authored, model.Edited) {
				roles, err := st.Roles().List(ctx(), ws.ID)
				if err != nil || len(roles) == 0 {
					t.Fatalf("list roles: %v", err)
				}
				r := roles[0]
				r.Name = "Curator, renamed"
				if err := st.Roles().Update(ctx(), r, editor); err != nil {
					t.Fatalf("update role: %v", err)
				}
				read, err := st.Roles().GetByID(ctx(), ws.ID, r.ID)
				if err != nil {
					t.Fatalf("read role: %v", err)
				}
				return read.Authored, read.Edited
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			made, changed := tc.make()
			if made.CreatedBy != maker.UserID || made.CreatedByName != maker.Name {
				t.Errorf("%s was created by %d/%q, want %d/%q",
					tc.what, made.CreatedBy, made.CreatedByName, maker.UserID, maker.Name)
			}
			// On a create both halves are the maker: the last thing that happened
			// to the row IS its creation.
			if changed.UpdatedByName != maker.Name {
				t.Errorf("%s was last changed by %q on creation", tc.what, changed.UpdatedByName)
			}

			made, changed = tc.change()
			if made.CreatedBy != maker.UserID || made.CreatedByName != maker.Name {
				t.Errorf("%s: an edit rewrote who made it, to %d/%q",
					tc.what, made.CreatedBy, made.CreatedByName)
			}
			if changed.UpdatedBy != editor.UserID || changed.UpdatedByName != editor.Name {
				t.Errorf("%s was last changed by %d/%q, want %d/%q",
					tc.what, changed.UpdatedBy, changed.UpdatedByName, editor.UserID, editor.Name)
			}
		})
	}
}

// A vendor, a model and a custom tool, which are the rows an administrator
// actually spends their time on.
func testTheEngineRecordsWhoConfiguredIt(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "engine-authored")
	maker := model.Actor{UserID: mustUser(t, st, ws.ID, "maker@acme.test").ID, Name: "The Maker"}
	editor := model.Actor{UserID: mustUser(t, st, ws.ID, "editor@acme.test").ID, Name: "The Editor"}

	vendor := &model.AIVendor{
		WorkspaceID: ws.ID, VendorKey: model.VendorAnthropic, Name: "Anthropic",
		Credentials: []byte("sealed"),
	}
	if err := st.Vendors().Create(ctx(), vendor, maker); err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	vendor.Name = "Anthropic (renamed)"
	vendor.Credentials = nil
	if err := st.Vendors().Update(ctx(), vendor, editor); err != nil {
		t.Fatalf("update vendor: %v", err)
	}
	readVendor, err := st.Vendors().GetByID(ctx(), ws.ID, vendor.ID)
	if err != nil {
		t.Fatalf("read vendor: %v", err)
	}
	if readVendor.CreatedByName != "The Maker" || readVendor.UpdatedByName != "The Editor" {
		t.Errorf("the vendor says %q / %q", readVendor.CreatedByName, readVendor.UpdatedByName)
	}

	aModel := &model.AIModel{
		WorkspaceID: ws.ID, VendorID: vendor.ID, ModelKey: "claude-opus", ContextWindow: 200000,
	}
	if err := st.AIModels().Create(ctx(), aModel, maker); err != nil {
		t.Fatalf("create model: %v", err)
	}
	aModel.ContextWindow = 300000
	if err := st.AIModels().Update(ctx(), aModel, editor); err != nil {
		t.Fatalf("update model: %v", err)
	}
	readModel, err := st.AIModels().GetByID(ctx(), ws.ID, aModel.ID)
	if err != nil {
		t.Fatalf("read model: %v", err)
	}
	if readModel.CreatedByName != "The Maker" || readModel.UpdatedByName != "The Editor" {
		t.Errorf("the model says %q / %q", readModel.CreatedByName, readModel.UpdatedByName)
	}

	// A custom tool has an author; a built-in does not, and that difference is
	// the point. Sync writes the built-ins from the code registry, so their
	// author columns stay empty: nobody made them, they came with the product.
	tool := &model.Tool{
		WorkspaceID: ws.ID, Name: "query_local", Kind: "custom",
		Template: "query", FriendlyName: "The database", Description: "reads rows",
		Risk: "read_only",
	}
	if err := st.Tools().CreateCustom(ctx(), tool, maker); err != nil {
		t.Fatalf("create custom tool: %v", err)
	}
	tool.FriendlyName = "The database, renamed"
	if err := st.Tools().UpdateCustom(ctx(), tool, editor); err != nil {
		t.Fatalf("update custom tool: %v", err)
	}
	readTool, err := st.Tools().GetByID(ctx(), ws.ID, tool.ID)
	if err != nil {
		t.Fatalf("read tool: %v", err)
	}
	if readTool.CreatedByName != "The Maker" || readTool.UpdatedByName != "The Editor" {
		t.Errorf("the tool says %q / %q", readTool.CreatedByName, readTool.UpdatedByName)
	}

	// And the person leaves. Every id goes to NULL, every name stays.
	if err := st.Users().Delete(ctx(), maker.UserID); err != nil {
		t.Fatalf("delete the maker: %v", err)
	}
	after, err := st.Vendors().GetByID(ctx(), ws.ID, vendor.ID)
	if err != nil {
		t.Fatalf("read vendor: %v", err)
	}
	if after.CreatedBy != 0 {
		t.Errorf("the deleted person is still linked: %d", after.CreatedBy)
	}
	if after.CreatedByName != "The Maker" {
		t.Errorf("who made it went with them: %q", after.CreatedByName)
	}
}
