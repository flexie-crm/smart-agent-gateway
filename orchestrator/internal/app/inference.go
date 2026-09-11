package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"flexie.io/sag/internal/inference"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/queue"
	"flexie.io/sag/internal/store"
)

// Inference nodes: machines that run models we own (KB/35).
//
// A machine belongs to the PLATFORM and a model belongs to a workspace, and the
// difference is the whole shape of this. A GPU box is racked once; every
// workspace on the deployment may have models on it. So a machine is an
// `inference_nodes` row with no workspace anywhere in it, and a workspace that
// has been GIVEN a model from a machine gets an `ai_vendors` row pointing at it,
// carrying no address and no key of its own.
//
// The consequence worth stating: adding a model to a second workspace downloads
// nothing. The weights are on the machine, loaded once, answering on one port.
// What a second workspace gets is two rows and permission to route there.
//
// Inference reaches a machine through the OpenAI-compatible adapter exactly as
// it reaches a hosted vendor, because the store resolves a pointer row into that
// machine's address and key. Nothing above the store knows what a machine is.
//
// **The node is the authority on its own disk and we are the only writer of
// rows.** So nothing here caches what a node holds: every answer is asked for
// when it is wanted. A download's progress in particular is NOT copied into a
// row of ours. The node already has it, keeps it for an hour after the download
// ends, and answers it in a millisecond; a second copy would need columns and
// would eventually disagree with the machine it describes.
//
// What IS a job is the obligation the node cannot carry: when a download
// finishes, an `ai_models` row has to exist, whether or not anybody was still
// looking at the screen. That is [App.RunModelPullJob].

// How often the pull job asks a node where a download has got to.
//
// The orchestrator polls and the node never calls back (KB/35), so this is the
// only clock in it. Two seconds is far finer than a transfer measured in
// minutes and costs one small request; the alternative would be a callback,
// which needs every machine that runs weights to be able to reach us and a
// second key in the other direction.
const modelPullPoll = 2 * time.Second

// A download that has not moved for this long is given up on.
//
// Not a timeout on the transfer, which may legitimately take hours: this is the
// gap between two ANSWERS from the node. A node that has stopped answering has
// been restarted or has gone, and its `incoming/` is cleared on the way back up,
// so nothing is still arriving.
const modelPullSilence = 5 * time.Minute

// modelPullPayload is what a pull job carries: which machine, and which of its
// downloads. Everything else about it is the machine's to say.
//
// It carries no workspace, because downloading is not a workspace's business:
// the weights land on a machine, and giving them to a workspace is a separate
// decision somebody makes afterwards.
type modelPullPayload struct {
	NodeID int64  `json:"node_id"`
	PullID string `json:"pull_id"`
	Repo   string `json:"repo"`
}

// ErrNotANode means there is no such machine.
var ErrNotANode = errors.New("no such machine")

// Node builds a client for one machine.
func (a *App) Node(ctx context.Context, nodeID int64) (*inference.Node, *model.InferenceNode, error) {
	row, err := a.Store.Nodes().Get(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}
	key, err := a.Keyring.Open(row.Key)
	if err != nil {
		a.Log.Error().Err(err).Int64("machine", nodeID).Msg("a machine's key cannot be opened")
		return nil, row, ErrCredentialsUnavailable
	}
	// The machine we MEAN to call, named, so a machine that took over another's
	// address cannot answer for it with a certificate of its own that is
	// perfectly valid and belongs to somebody else.
	//
	// The client is the one kept for this machine, not one made here. This
	// function is called per request, and a client made per request is a
	// connection pool made per request: see machineTrust.control.
	pinned := ""
	if row.PinnedCert != nil {
		pinned = *row.PinnedCert
	}
	client, err := a.MachineControlClient(ctx, row.NodeID, pinned)
	if err != nil {
		return nil, row, err
	}
	node, err := inference.New(row.BaseURL, string(key), client)
	if err != nil {
		return nil, row, fmt.Errorf("%w: %w", ErrNotANode, err)
	}
	return node, row, nil
}

// NodeSummary is one machine on the list screen.
type NodeSummary struct {
	ID      int64  `json:"id"`
	NodeID  string `json:"node_id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Version string `json:"version"`
	// Reachable is false when the machine did not answer, and Problem says what
	// happened. A node that is off is a row that says so, never a row missing
	// from the list: somebody looking for a machine needs to be told it is down,
	// not left to wonder whether they imagined configuring it.
	Reachable bool   `json:"reachable"`
	Problem   string `json:"problem,omitempty"`
	// Info is what the machine said about itself, when it answered.
	Info *inference.Info `json:"info,omitempty"`
	// CertExpiresAt is when the certificate this machine serves with runs out.
	// It renews itself by checking back in, so this is here to make a machine
	// that has STOPPED checking in visible before the day it goes silent.
	CertExpiresAt *time.Time `json:"cert_expires_at,omitempty"`
}

// NodeDetail is one machine's own screen, in one answer (KB/19).
type NodeDetail struct {
	NodeSummary
	Models []NodeModel      `json:"models"`
	Pulls  []inference.Pull `json:"pulls"`
}

// NodeModel is a model on a machine, and which workspaces have been given it.
//
// That list is the point of the screen. The weights are downloaded once and sit
// on one disk; what varies is who may route to them, and this is the only place
// that is visible. A model with an empty list is one nothing can use yet.
type NodeModel struct {
	inference.Model
	Workspaces []ModelWorkspace `json:"workspaces"`
}

// ModelWorkspace is one workspace that has been given a model, and the registry
// row that lets it route there.
type ModelWorkspace struct {
	WorkspaceID int64  `json:"workspace_id"`
	Name        string `json:"name"`
	ModelID     int64  `json:"model_id"`
}

// Nodes is the list screen: every machine, asked in parallel whether it is up.
//
// Parallel because the answer is a network round trip to each and a screen must
// not take the sum of them. Asked at all, rather than stored, because whether a
// machine is up is a fact about the machine: the same row is a working node when
// the process is running on it and an unreachable address when it is not.
func (a *App) Nodes(ctx context.Context) ([]NodeSummary, error) {
	rows, err := a.Store.Nodes().List(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]NodeSummary, len(rows))
	done := make(chan struct{}, len(rows))
	for i, row := range rows {
		go func(i int, row *model.InferenceNode) {
			defer func() { done <- struct{}{} }()
			out[i] = a.probe(ctx, row)
		}(i, row)
	}
	for range rows {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return out, nil
}

// probe asks one machine how it is.
func (a *App) probe(ctx context.Context, row *model.InferenceNode) NodeSummary {
	summary := NodeSummary{
		ID: row.ID, NodeID: row.NodeID, Name: row.Name,
		BaseURL: row.BaseURL, Version: row.Version,
		CertExpiresAt: row.CertExpiresAt,
	}
	node, _, err := a.Node(ctx, row.ID)
	if err != nil {
		summary.Problem = nodeProblem(err)
		return summary
	}
	info, err := node.Info(ctx)
	if err != nil {
		summary.Problem = nodeProblem(err)
		return summary
	}
	summary.Reachable = true
	summary.Info = &info
	return summary
}

// NodeDetail answers one machine's screen: what it is, what is on its disk, who
// may use each of those, and anything downloading.
func (a *App) NodeDetail(ctx context.Context, nodeID int64) (NodeDetail, error) {
	row, err := a.Store.Nodes().Get(ctx, nodeID)
	if err != nil {
		return NodeDetail{}, err
	}
	detail := NodeDetail{NodeSummary: NodeSummary{
		ID: row.ID, NodeID: row.NodeID, Name: row.Name,
		BaseURL: row.BaseURL, Version: row.Version,
		CertExpiresAt: row.CertExpiresAt,
	}}

	node, _, err := a.Node(ctx, nodeID)
	if err != nil {
		detail.Problem = nodeProblem(err)
		return detail, nil
	}
	info, err := node.Info(ctx)
	if err != nil {
		detail.Problem = nodeProblem(err)
		return detail, nil
	}
	detail.Reachable = true
	detail.Info = &info

	// From here on a failure is REPORTED rather than raised. The machine has
	// already answered, so this screen has something true to show; turning a
	// second call's failure into an error empties the whole page, and an empty
	// page is the one outcome that says nothing at all. A machine that answers
	// and then stops mid-screen is exactly when somebody needs to see what it
	// did say, and what went wrong after.
	models, err := node.Models(ctx)
	if err != nil {
		detail.Problem = nodeProblem(fmt.Errorf("read the machine's models: %w", err))
		return detail, nil
	}
	pulls, err := node.Pulls(ctx)
	if err != nil {
		detail.Problem = nodeProblem(fmt.Errorf("read the machine's downloads: %w", err))
		return detail, nil
	}
	detail.Pulls = pulls

	reach, err := a.modelReach(ctx, nodeID)
	if err != nil {
		return detail, err
	}
	detail.Models = make([]NodeModel, len(models))
	for i, m := range models {
		// Empty, not nil, for a model no workspace has yet. A nil slice marshals
		// as `null`, and the screen that reads it is entitled to the list its
		// type promises rather than a guard on every use.
		shared := reach[m.Handle]
		if shared == nil {
			shared = []ModelWorkspace{}
		}
		detail.Models[i] = NodeModel{Model: m, Workspaces: shared}
	}
	return detail, nil
}

// modelReach works out, per model handle, which workspaces can route to it.
//
// One pass over every workspace's pointer at this machine, rather than a query
// per model: a machine with twenty models would otherwise be twenty round trips
// to draw one screen.
func (a *App) modelReach(ctx context.Context, nodeID int64) (map[string][]ModelWorkspace, error) {
	workspaces, err := a.Store.Workspaces().List(ctx)
	if err != nil {
		return nil, err
	}

	reach := map[string][]ModelWorkspace{}
	for _, ws := range workspaces {
		pointer, err := a.Store.Vendors().ByNodeID(ctx, ws.ID, nodeID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		models, err := a.Store.AIModels().List(ctx, ws.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			if m.VendorID != pointer.ID {
				continue
			}
			reach[m.ModelKey] = append(reach[m.ModelKey], ModelWorkspace{
				WorkspaceID: ws.ID, Name: ws.Name, ModelID: m.ID,
			})
		}
	}
	return reach, nil
}

// StartModelPull tells a node to fetch a model, and writes down the obligation
// to finish the job when it lands.
//
// The node has already validated everything it can before this returns (that
// the repository exists, that it publishes readable weights, that the choice
// between compression levels is not ours to make, that there is room, that this
// machine may fetch it), so a pull that starts is a pull that was going to work.
// Approval equals success (CLAUDE.md).
func (a *App) StartModelPull(ctx context.Context, workspaceID, nodeID int64, repo, revision, file string) (inference.Pull, error) {
	node, _, err := a.Node(ctx, nodeID)
	if err != nil {
		return inference.Pull{}, err
	}
	pull, err := node.StartPull(ctx, repo, revision, file)
	if err != nil {
		return inference.Pull{}, err
	}

	payload, err := json.Marshal(modelPullPayload{NodeID: nodeID, PullID: pull.ID, Repo: pull.Repo})
	if err != nil {
		return pull, fmt.Errorf("encode the download job: %w", err)
	}
	job := &model.Job{
		// The workspace here is WHO ASKED, not what the download belongs to: the
		// weights land on a machine and belong to no workspace, and giving them
		// to one is a separate decision made afterwards. The column is NOT NULL
		// with a foreign key, though, so a job has to live somewhere, and the
		// person who started it is the honest answer. The handler never reads it.
		//
		// It was left unset when downloads became platform-scoped, which made
		// every insert violate that constraint: the download ran and the job
		// that registers the model was never created at all.
		WorkspaceID: workspaceID,
		Kind:        model.JobKindModelPull,
		Subject:     queue.SubjectRoot + "." + queue.CapabilityDefault + "." + model.JobKindModelPull,
		Payload:     payload,
		// One attempt. Watching a download is not work that benefits from being
		// retried: if this process dies the download itself carries on, and the
		// scan hands the job to somebody else who picks the watching back up
		// from where the node says it is.
		MaxAttempts: 3,
	}
	if err := a.Store.Jobs().Enqueue(ctx, job); err != nil {
		// The download is under way on the node whatever happens here, so this
		// is not a failed pull. It is a pull nothing is waiting to finish, which
		// is worth saying loudly: the model will arrive and no row will point at
		// it until somebody looks.
		a.Log.Error().Err(err).Str("pull", pull.ID).Int64("machine", nodeID).
			Msg("download started with nothing watching it")
		return pull, nil
	}
	// A deployment with no broker is a deployment where the worker's scan is the
	// only way work is found, which is a supported shape (KB/05: the row is the
	// truth, the broker is a wake-up). Dereferencing it was a panic in a request
	// path, which is the one thing a request path may never do.
	if a.Queue == nil {
		a.Log.Info().Str("job", job.ID).Msg("no broker: the download job waits for the next scan")
	} else if err := a.Queue.Enqueue(ctx, job.Subject, queue.Task{
		ID: job.ID, Kind: job.Kind, WorkspaceID: job.WorkspaceID, Payload: job.Payload,
	}); err != nil {
		// The row is committed, so the sweep finds it. A lost signal costs the
		// sweep's interval and not the job (KB/05).
		a.Log.Warn().Err(err).Str("job", job.ID).Msg("download job signal not sent")
	}
	a.event().Str("pull", pull.ID).Str("repo", pull.Repo).Int64("machine", nodeID).
		Int64("bytes", pull.BytesTotal).Msg("model download started")
	return pull, nil
}

// RunModelPullJob watches a download to its end and writes the row that makes
// the model usable.
//
// This exists for the case where nobody is looking. The screen reads progress
// straight off the node and needs none of this; what needs it is the promise
// that a model which finishes downloading at three in the morning is routable
// at nine.
//
// Idempotent, because delivery is at least once and this job can be reclaimed
// after the row was written: a model that already has a row is left alone.
func (a *App) RunModelPullJob(ctx context.Context, job *model.Job) error {
	var p modelPullPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		// A job we cannot read cannot be retried into readability. The download
		// itself is unharmed and the node will still be holding the model.
		a.Log.Error().Err(err).Str("job", job.ID).Msg("unreadable download job")
		return nil
	}

	node, _, err := a.Node(ctx, p.NodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The machine was removed while this waited. There is nothing to ask
			// and nothing to record.
			return nil
		}
		return fmt.Errorf("reach the machine: %w", err)
	}

	ticker := time.NewTicker(modelPullPoll)
	defer ticker.Stop()
	lastAnswer := time.Now()

	for {
		pull, err := node.Pull(ctx, p.PullID)
		switch {
		case err == nil:
			lastAnswer = time.Now()
			if !pull.Finished() {
				break
			}
			return a.settleModelPull(ctx, node, pull)

		case isGone(err):
			// The node forgot this download, which it does an hour after one
			// ends and on every restart. Either way there is nothing left to
			// watch, and the model, if it arrived, is in the node's catalogue
			// where the screen will find it.
			a.Log.Info().Str("pull", p.PullID).Str("job", job.ID).
				Msg("the node no longer has this download")
			return nil

		default:
			// A machine that is briefly unreachable is not a failed download:
			// the transfer is the node's and carries on without us. We keep
			// asking until it has been quiet long enough to have restarted.
			if time.Since(lastAnswer) > modelPullSilence {
				return fmt.Errorf("the node stopped answering about %s: %w", p.Repo, err)
			}
			a.Log.Debug().Err(err).Str("pull", p.PullID).Msg("the node did not answer")
		}

		select {
		case <-ctx.Done():
			// Being asked to stop is not a failure (KB/35): the job goes back
			// whole and another worker, or this one after a restart, carries on
			// watching from wherever the node says the download has got to.
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// settleModelPull records how a download ended.
//
// A finished download creates NO rows. The weights are on the machine and that
// is all that has happened; which workspaces may use them is a decision somebody
// makes on the screen, and making it here would put a model in front of every
// workspace because one machine finished a transfer.
func (a *App) settleModelPull(ctx context.Context, node *inference.Node, pull inference.Pull) error {
	if pull.State != inference.PullReady {
		a.Log.Warn().Str("pull", pull.ID).Str("repo", pull.Repo).Str("state", pull.State).
			Str("reason", pull.Error).Msg("model download did not finish")
		return nil
	}

	m, err := node.Model(ctx, pull.ModelUID)
	if err != nil {
		return fmt.Errorf("read the model that arrived: %w", err)
	}
	a.event().Str("pull", pull.ID).Str("repo", pull.Repo).Str("model", m.Handle).
		Msg("model download ready")
	return nil
}

// AttachNodeModel gives one workspace permission to route to one model on one
// machine.
//
// This is the whole of what "adding a model to a workspace" does, and what it
// deliberately does NOT do is download anything. The weights are already on the
// machine, loaded once, answering on one port. A second workspace gets two rows:
// a pointer at the machine, made once and shared by every model it takes from
// there, and the registry row for this model.
//
// Idempotent: a workspace that already has this model is left as it is, because
// two administrators pressing the same button want the same outcome.
func (a *App) AttachNodeModel(ctx context.Context, nodeID, workspaceID int64, uid string) (*model.AIModel, error) {
	node, row, err := a.Node(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	m, err := node.Model(ctx, uid)
	if err != nil {
		return nil, err
	}

	pointer, err := a.pointerTo(ctx, workspaceID, row)
	if err != nil {
		return nil, err
	}

	existing, err := a.Store.AIModels().List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	for _, candidate := range existing {
		if candidate.VendorID == pointer.ID && candidate.ModelKey == m.Handle {
			return candidate, nil
		}
	}

	kind := m.Kind
	if !slices.Contains(model.KnownModelTypes, kind) {
		// A machine that grows a kind the registry does not have would otherwise
		// fail the insert on the column's enum. Chat is what most models are and
		// what an administrator is least surprised to have to correct.
		a.Log.Warn().Str("model", m.Handle).Str("kind", kind).
			Msg("the machine reported a kind the registry does not have")
		kind = model.ModelTypeChat
	}

	created := &model.AIModel{
		WorkspaceID:   workspaceID,
		VendorID:      pointer.ID,
		ModelKey:      m.Handle,
		Type:          kind,
		ContextWindow: m.Facts.ContextLength,
		Description:   describeNodeModel(m),
		Status:        model.StatusActive,
	}
	if err := a.Store.AIModels().Create(ctx, created); err != nil {
		return nil, fmt.Errorf("give the workspace this model: %w", err)
	}
	a.Log.Info().Int64("workspace", workspaceID).Str("model", m.Handle).Str("machine", row.Name).
		Msg("model given to a workspace")
	return created, nil
}

// pointerTo is the workspace's one vendor row for a machine, made if it has none.
//
// One per workspace and machine, however many models it takes from there. It
// carries no address and no key: both are the machine's, read through it, so
// there is nothing here to keep in step and nothing stored twice.
func (a *App) pointerTo(ctx context.Context, workspaceID int64, node *model.InferenceNode) (*model.AIVendor, error) {
	existing, err := a.Store.Vendors().ByNodeID(ctx, workspaceID, node.ID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	pointer := &model.AIVendor{
		WorkspaceID: workspaceID,
		VendorKey:   model.VendorOpenAICompatible,
		Name:        node.Name,
		NodeID:      node.ID,
		Status:      model.StatusActive,
	}
	if err := a.Store.Vendors().Create(ctx, pointer); err != nil {
		return nil, fmt.Errorf("point this workspace at the machine: %w", err)
	}
	return pointer, nil
}

// DetachNodeModel takes a model away from one workspace.
//
// The weights are untouched: this is about who may route there. The pointer row
// is left alone even when the last model goes, because it is cheap and a
// workspace that had one model from a machine usually gets another.
func (a *App) DetachNodeModel(ctx context.Context, nodeID, workspaceID int64, handle string) error {
	pointer, err := a.Store.Vendors().ByNodeID(ctx, workspaceID, nodeID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	models, err := a.Store.AIModels().List(ctx, workspaceID)
	if err != nil {
		return err
	}
	for _, m := range models {
		if m.VendorID == pointer.ID && m.ModelKey == handle {
			return a.Store.AIModels().Delete(ctx, workspaceID, m.ID)
		}
	}
	return nil
}

// ForgetNodeModel takes a model away from EVERY workspace, because it is no
// longer on the machine.
//
// A row pointing at weights that are gone is worse than no row: an agent can
// still be assigned to it, and the failure surfaces to somebody asking a
// question rather than to the administrator who deleted it.
func (a *App) ForgetNodeModel(ctx context.Context, nodeID int64, handle string) error {
	workspaces, err := a.Store.Workspaces().List(ctx)
	if err != nil {
		return err
	}
	for _, ws := range workspaces {
		if err := a.DetachNodeModel(ctx, nodeID, ws.ID, handle); err != nil {
			return err
		}
	}
	return nil
}

// describeNodeModel is the note the registry row carries: where the weights came
// from, so somebody reading the model list a year later knows what it is.
func describeNodeModel(m inference.Model) string {
	parts := []string{m.Repo}
	if m.Facts.Architecture != "" {
		parts = append(parts, m.Facts.Architecture)
	}
	if m.Facts.License != "" {
		parts = append(parts, m.Facts.License)
	}
	return strings.Join(parts, " · ")
}

// isGone reports whether the node said it has no such thing, as opposed to not
// answering at all.
func isGone(err error) bool {
	nodeErr, ok := inference.AsError(err)
	return ok && nodeErr.NotFound()
}

// nodeProblem turns whatever went wrong into one line for a screen.
//
// A vendor whose credential cannot be opened, a machine that is off and a
// machine that refused the key are three different things and are said
// differently, because the fix for each is different.
func nodeProblem(err error) string {
	switch {
	case errors.Is(err, ErrCredentialsUnavailable):
		return "this machine's key cannot be read"
	case errors.Is(err, ErrNotANode):
		return "this is not a machine of ours"
	case errors.Is(err, store.ErrNotFound):
		return "this machine is no longer configured"
	}
	if nodeErr, ok := inference.AsError(err); ok {
		return nodeErr.Message
	}
	return "this machine did not answer"
}
