// Package tools installs the built-in tools into the one registry.
//
// Each tool lives in its own package under this one (its Schema beside its
// Handler, the same shape every tool follows) and shares internal/tools/toolkit
// for result shapes, the permission check, and the guards a tool that reaches
// outside needs. This file is the one place that names the tools this build
// ships and hands each its dependencies: adding a tool is a package plus a line
// here.
//
// Every tool goes through the same contract as any other, including the
// permission check: a tool is not a way around the permission system, it is
// another caller of it.
package tools

import (
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/brain"
	"flexie.io/sag/internal/tools/currenttime"
	"flexie.io/sag/internal/tools/httprequest"
	"flexie.io/sag/internal/tools/integrations"
	"flexie.io/sag/internal/tools/listmodels"
	"flexie.io/sag/internal/tools/machine"
	"flexie.io/sag/internal/tools/recall"
	"flexie.io/sag/internal/tools/setmodelstatus"
	"flexie.io/sag/internal/tools/toolguide"
	"flexie.io/sag/internal/tools/toolkit"
)

// Register installs the built-in tools. The registry is handed to tool_guide as
// well as filled by it: tool_guide reads the other tools' deep guides out of the
// registry at call time, so it must be registered alongside the tools it will
// describe.
// machines is how a tool that runs on the person's own computer gets there.
// Every edition has one, the personal one included: its gateway is on the same
// computer, and the link is still what carries a CALL whose far end is the chat
// application (KB/39). Nil only where a build genuinely has no link, and the
// tools are registered all the same: the catalogue is what an administrator
// governs, and it must not change shape depending on whose laptop is awake.
func Register(
	registry *tool.Registry,
	st store.Store,
	auth toolkit.Authorizer,
	machines machine.Machines,
	zones currenttime.Zones,
) error {
	for _, t := range append(machine.Tools(machines), []tool.Tool{
		currenttime.New(machines, zones),
		// Reading back what was remembered. Internal, like remembering itself:
		// the prompt says what is known OF, this is how it is read (KB/25).
		recall.New(st),
		// Reaching a connected service without carrying it. Internal: it is how
		// the product is put together, not an ability an administrator grants.
		integrations.New(nil, nil),
		listmodels.New(st, auth),
		setmodelstatus.New(st, auth),
		httprequest.New(),
		toolguide.New(registry),
		// The brain tools ship with an empty allow-list; the loadout rebinds their
		// handlers (and the write tool's pre-park validator) over the agent's own
		// brains each turn (BindBrains).
		brain.NewRead(st.Brains()),
		brain.NewWrite(st.Brains()),
	}...) {
		if err := registry.Register(t); err != nil {
			return err
		}
	}
	return nil
}
