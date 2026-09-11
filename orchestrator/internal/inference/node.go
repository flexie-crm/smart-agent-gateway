// Package inference is how the orchestrator talks to a machine that runs
// models we own (KB/35).
//
// The division of labour it implements is the one that makes the whole thing
// work: the node is the authority on its own disk and this side is the only
// writer of rows. So everything here is a QUESTION or an INSTRUCTION, never a
// synchronisation. We ask what is on a node, we tell it to fetch or load
// something, and we record the answer in our own tables. Nothing reaches past
// this into the node's storage and the node reaches into nothing of ours.
//
// A node is an `ai_vendors` row: `base_url` says where it is and the sealed
// credential is its key, so a node needs no table of its own. Inference goes to
// the same row through the OpenAI-compatible adapter, unchanged; what is here is
// only the control surface beside it.
//
// Every call is mutually authenticated TLS. Both ends hold a certificate from
// the deployment's own authority (internal/nodeca): we check the machine chains
// to it AND that the machine which answered is the one this client was built
// for, and the machine checks the same of us before it reads a byte of the
// request. That is what lets a machine sit on a public address; the key below
// still travels on every call, now inside that channel, and is what tells one
// machine from another to the machine itself.
//
// Deliberately NOT behind the SSRF guard that `http_request` uses. That guard
// exists to stop a model reaching a private address; a node IS a private
// address, put there by an administrator, and refusing to reach it would refuse
// the entire feature.
package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// How long to wait on a control call.
//
// Every call this package makes is a question about state or an instruction to
// start something, and none of them transfer weights, so none of them should
// take long. Loading a model is the exception and has its own ceiling below: it
// really does read tens of gigabytes off a disk.
const (
	callTimeout = 30 * time.Second
	loadTimeout = 30 * time.Minute
	// probeTimeout bounds the one question a SCREEN waits on: is this machine
	// there, and what does it say about itself.
	//
	// Short on purpose, and much shorter than the rest. A machine that has gone
	// away does not refuse a connection, it says nothing at all, so the caller
	// waits out the whole ceiling; at thirty seconds the Machines screen spins
	// for half a minute because ONE row is a box that was decommissioned. Five
	// seconds is longer than any healthy answer takes and short enough that an
	// unreachable machine reads as an answer rather than as a hang.
	//
	// It buys latency for certainty, deliberately: a machine on a slow link
	// might be called unreachable while it is merely slow. The screen says
	// which machine and why, and refreshing asks again, so the cost of being
	// wrong here is a retry rather than a wrong belief.
	probeTimeout = 5 * time.Second
)

// Node is one machine, addressed through the vendor row that describes it.
type Node struct {
	control string
	key     string
	http    *http.Client
}

// New builds a client for the node a vendor row points at.
//
// baseURL is the vendor's own, which is the INFERENCE url and ends in the
// version segment the OpenAI-compatible adapter needs. The control surface is
// its sibling, so that segment is trimmed here rather than an administrator
// being asked for the same machine twice.
//
// http is BORROWED, not built here, and that is the point of the parameter. A
// Node is a per-request value: it is made to ask a machine one question and
// then dropped. An http.Client is not, because the transport under it is a
// connection pool, and a pool that is dropped is not closed. Building one in
// here therefore leaked a live socket per call until the machine ran out of
// them (KB/29), which is why the caller now supplies one that outlives the
// question. It carries our certificate and the authority to judge the
// machine's by, and is required: a machine serves nothing without a
// certificate, so a client built without one could only ever fail, and failing
// HERE says why.
func New(baseURL, key string, client *http.Client) (*Node, error) {
	control, err := controlRoot(baseURL)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, errors.New("inference: the node has no key configured")
	}
	if client == nil {
		return nil, errors.New("inference: there is nothing to authenticate the node with")
	}
	return &Node{control: control, key: key, http: client}, nil
}

// controlRoot turns an inference url into the node's own root.
//
// https only. A machine holds a certificate and serves nothing without one, so a
// plain address is one no call could be made on; refusing it here names the
// problem instead of leaving a connection to fail with something about a
// protocol.
func controlRoot(baseURL string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return "", errors.New("inference: the node has no address configured")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" {
		return "", fmt.Errorf("inference: %q is not a node address", baseURL)
	}
	parsed.Path = strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/"), "/v1")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

// Error is a refusal the node itself sent, kept whole.
//
// The node's own words are the only account of why it said no, and its code is
// stable where its prose is not, so a caller branches on Code and shows Message.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// NotFound reports whether this is the node saying it does not have the thing.
func (e *Error) NotFound() bool { return e.Status == http.StatusNotFound }

// AsError pulls the node's own refusal out of an error chain, if that is what
// it is. A transport failure is not one of these and must not be reported to a
// person as though the node had an opinion.
func AsError(err error) (*Error, bool) {
	var nodeErr *Error
	ok := errors.As(err, &nodeErr)
	return nodeErr, ok
}

// --- what a node answers -----------------------------------------------------

// Machine is what the node reports about the hardware it is on.
type Machine struct {
	MemoryTotal uint64 `json:"memory_total"`
	MemoryFree  uint64 `json:"memory_free"`
	DiskTotal   uint64 `json:"disk_total"`
	DiskFree    uint64 `json:"disk_free"`
	Processors  int    `json:"processors"`
}

// Info is a node describing itself.
type Info struct {
	Name string `json:"name"`
	// CanInfer is false for a build that serves the control surface and cannot
	// run a model. Worth surfacing rather than discovering when somebody asks a
	// question.
	CanInfer        bool    `json:"can_infer"`
	Version         string  `json:"version"`
	UptimeSeconds   int64   `json:"uptime_seconds"`
	Models          int     `json:"models"`
	Resident        int     `json:"resident"`
	DownloadsActive int     `json:"downloads_active"`
	Machine         Machine `json:"machine"`
}

// Facts are what was read off the weights once and cannot be changed.
type Facts struct {
	Architecture    string `json:"architecture,omitempty"`
	Parameters      uint64 `json:"parameters,omitempty"`
	ContextLength   int    `json:"context_length,omitempty"`
	License         string `json:"license,omitempty"`
	PublishedFormat string `json:"published_format,omitempty"`
	Files           int    `json:"files"`
	SizeBytes       int64  `json:"size_bytes"`
}

// Model is one model on a node.
type Model struct {
	UID      string `json:"uid"`
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	Name     string `json:"name"`
	// Handle is what the gateway asks for: readable, unique on the node, and
	// what an `ai_models` row stores as its key. UID addresses the same model on
	// the control surface, and is a uuid.
	Handle   string            `json:"handle"`
	Kind     string            `json:"kind"`
	Facts    Facts             `json:"facts"`
	Settings map[string]string `json:"settings"`
	// Resident is what an administrator decided; Residency is where the model
	// actually is. They disagree while a node is coming back up, which is
	// exactly when somebody wants to see both.
	Resident  bool      `json:"resident"`
	Residency string    `json:"residency"`
	AddedAt   time.Time `json:"added_at"`
	// LoadError is why the last attempt to bring it into memory failed.
	//
	// Loading is started by a request and finishes long after it, so a failure
	// has no reply to travel back on. Without this the two fields above are the
	// whole story a screen can tell: wanted, and not there. True, and it does
	// not say whether the machine ran out of memory or the engine cannot load
	// that kind of model at all, which are the same picture and different
	// problems.
	LoadError string `json:"load_error,omitempty"`
}

// Field and Section are the settings form the node declares and the console
// renders without knowing what any of it means. Deliberately the same shape as
// `tools/template` and the datasource drivers, so one component draws all three.
type Field struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Secret   bool     `json:"secret,omitempty"`
	Options  []string `json:"options,omitempty"`
	Default  string   `json:"default,omitempty"`
	Help     string   `json:"help,omitempty"`
	Span     int      `json:"span,omitempty"`
}

type Section struct {
	Title  string  `json:"title"`
	Hint   string  `json:"hint,omitempty"`
	Fields []Field `json:"fields"`
}

// Form is the settings form and what is currently in it, in one answer.
type Form struct {
	Sections []Section         `json:"sections"`
	Values   map[string]string `json:"values"`
}

// SearchHit is a model in the library. Cheap, and promises nothing about
// whether it will run on this node.
type SearchHit struct {
	Repo      string `json:"repo"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Downloads uint64 `json:"downloads"`
	Likes     uint64 `json:"likes"`
}

// File is one file a pull would fetch.
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Verdict is the node's answer to whether something can be done here, with the
// reason when it cannot.
type Verdict struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// Repo is a library model resolved to one commit, with what it would cost and
// whether this node can take it. This is what an administrator says yes to.
type Repo struct {
	Repo     string `json:"repo"`
	Name     string `json:"name"`
	Revision string `json:"revision"`
	Kind     string `json:"kind"`
	License  string `json:"license,omitempty"`
	Gated    bool   `json:"gated"`
	// Architecture is what the weights declare themselves to be. Shown because
	// it is the thing the runtime verdict below was decided on, and a refusal
	// somebody wants to argue with should say what it was reading.
	Architecture string `json:"architecture,omitempty"`
	Files        []File `json:"files"`
	SizeBytes    int64  `json:"size_bytes"`
	// Choices lists every weights file on offer, so a caller refused for
	// ambiguity can be told what to choose between.
	Choices []File  `json:"choices"`
	Storage Verdict `json:"storage"`
	Memory  Verdict `json:"memory"`
	Access  Verdict `json:"access"`
	// Runtime is whether anything on that machine could load these weights at
	// all. The only one of the four about the model rather than the machine's
	// resources, and the last to exist: a model nothing could execute used to
	// pass the other three and fail after the download.
	Runtime Verdict `json:"runtime"`
	// AlreadyHere is the model this already is on the node, if it is.
	AlreadyHere string `json:"already_here,omitempty"`
}

// Pull states, as the node names them.
const (
	PullDownloading = "downloading"
	PullVerifying   = "verifying"
	PullReady       = "ready"
	PullFailed      = "failed"
	PullCancelled   = "cancelled"
)

// Pull is one download.
type Pull struct {
	ID         string    `json:"id"`
	Repo       string    `json:"repo"`
	Revision   string    `json:"revision"`
	State      string    `json:"state"`
	BytesDone  int64     `json:"bytes_done"`
	BytesTotal int64     `json:"bytes_total"`
	FilesDone  int       `json:"files_done"`
	FilesTotal int       `json:"files_total"`
	Error      string    `json:"error,omitempty"`
	ModelUID   string    `json:"model_uid,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Finished reports whether nothing more will happen to this download.
func (p *Pull) Finished() bool {
	return p.State == PullReady || p.State == PullFailed || p.State == PullCancelled
}

// --- asking -----------------------------------------------------------------

// Info describes the node. This is also the probe: a vendor row that answers it
// is one of ours, and one that does not is an ordinary self-hosted endpoint.
// Info is also the reachability check: it is the first call every screen makes
// and the one that decides whether a machine is shown as answering. So it gets
// the short ceiling rather than the ordinary one.
func (n *Node) Info(ctx context.Context) (Info, error) {
	return send[Info](ctx, n, http.MethodGet, "/node", nil, probeTimeout)
}

func (n *Node) Models(ctx context.Context) ([]Model, error) {
	return get[[]Model](ctx, n, "/node/models")
}

func (n *Node) Model(ctx context.Context, uid string) (Model, error) {
	return get[Model](ctx, n, "/node/models/"+url.PathEscape(uid))
}

func (n *Node) Form(ctx context.Context, uid string) (Form, error) {
	return get[Form](ctx, n, "/node/models/"+url.PathEscape(uid)+"/form")
}

func (n *Node) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	params := url.Values{"q": {query}}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	return get[[]SearchHit](ctx, n, "/node/library/search?"+params.Encode())
}

// Describe resolves a library model to one commit and asks the node whether it
// can take it. `file` names one weights file, for a repository published at
// several compression levels.
func (n *Node) Describe(ctx context.Context, repo, revision, file string) (Repo, error) {
	params := url.Values{"repo": {repo}}
	if revision != "" {
		params.Set("revision", revision)
	}
	if file != "" {
		params.Set("file", file)
	}
	return get[Repo](ctx, n, "/node/library/describe?"+params.Encode())
}

func (n *Node) Pulls(ctx context.Context) ([]Pull, error) {
	return get[[]Pull](ctx, n, "/node/pulls")
}

func (n *Node) Pull(ctx context.Context, id string) (Pull, error) {
	return get[Pull](ctx, n, "/node/pulls/"+url.PathEscape(id))
}

// --- telling ----------------------------------------------------------------

// StartPull begins a download and returns as soon as it is under way. Nothing
// waits for weights (KB/35): the answer carries an id to ask about later.
func (n *Node) StartPull(ctx context.Context, repo, revision, file string) (Pull, error) {
	body := map[string]string{"repo": repo}
	if revision != "" {
		body["revision"] = revision
	}
	if file != "" {
		body["file"] = file
	}
	return send[Pull](ctx, n, http.MethodPost, "/node/pulls", body, callTimeout)
}

// CancelPull stops a download and KEEPS what it fetched, so it can be resumed.
func (n *Node) CancelPull(ctx context.Context, id string) (Pull, error) {
	return send[Pull](ctx, n, http.MethodPost,
		"/node/pulls/"+url.PathEscape(id)+"/cancel", nil, callTimeout)
}

// ResumePull carries a stopped download on from where it stopped: whole files
// are skipped and a partial one continues. On a model measured in tens of
// gigabytes that is the difference between resuming and starting over.
func (n *Node) ResumePull(ctx context.Context, id string) (Pull, error) {
	return send[Pull](ctx, n, http.MethodPost,
		"/node/pulls/"+url.PathEscape(id)+"/resume", nil, callTimeout)
}

// DeletePull forgets a download and removes what it fetched. The only call here
// that throws work away.
func (n *Node) DeletePull(ctx context.Context, id string) error {
	_, err := send[struct {
		Removed bool `json:"removed"`
	}](ctx, n, http.MethodDelete, "/node/pulls/"+url.PathEscape(id), nil, callTimeout)
	return err
}

func (n *Node) SaveSettings(ctx context.Context, uid string, values map[string]string) (Model, error) {
	if values == nil {
		values = map[string]string{}
	}
	return send[Model](ctx, n, http.MethodPut,
		"/node/models/"+url.PathEscape(uid)+"/settings",
		map[string]any{"values": values}, callTimeout)
}

// Load brings a model into memory and keeps it there.
//
// The only call here with a long ceiling, because it is the only one that reads
// weights off a disk. A caller wanting to answer a browser quickly should run
// it away from the request.
func (n *Node) Load(ctx context.Context, uid string) (Model, error) {
	return send[Model](ctx, n, http.MethodPost,
		"/node/models/"+url.PathEscape(uid)+"/load", nil, loadTimeout)
}

func (n *Node) Unload(ctx context.Context, uid string) (Model, error) {
	return send[Model](ctx, n, http.MethodPost,
		"/node/models/"+url.PathEscape(uid)+"/unload", nil, loadTimeout)
}

// Removed is what a node says it took away.
type Removed struct {
	UID        string `json:"uid"`
	Name       string `json:"name"`
	FreedBytes int64  `json:"freed_bytes"`
}

func (n *Node) Delete(ctx context.Context, uid string) (Removed, error) {
	return send[Removed](ctx, n, http.MethodDelete,
		"/node/models/"+url.PathEscape(uid), nil, callTimeout)
}

// --- the wire ---------------------------------------------------------------

func get[T any](ctx context.Context, n *Node, path string) (T, error) {
	return send[T](ctx, n, http.MethodGet, path, nil, callTimeout)
}

func send[T any](ctx context.Context, n *Node, method, path string, body any, timeout time.Duration) (T, error) {
	var out T

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return out, fmt.Errorf("inference: encoding the request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, n.control+path, payload)
	if err != nil {
		return out, fmt.Errorf("inference: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+n.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := n.http.Do(req)
	if err != nil {
		return out, fmt.Errorf("inference: the node did not answer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded, because everything a node answers is small and a body that is not
	// is a machine misbehaving rather than a model list.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return out, fmt.Errorf("inference: reading the answer: %w", err)
	}

	if resp.StatusCode >= 400 {
		return out, decodeError(resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("inference: the node answered something unexpected: %w", err)
	}
	return out, nil
}

// decodeError turns a refusal into the node's own words, falling back to the
// status when the body is not one of ours.
//
// The fallback matters: a proxy or a load balancer in front of a node answers
// with HTML, and reporting that as though the node had said it would put a page
// of markup in front of an administrator.
func decodeError(status int, raw []byte) error {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		return &Error{Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	return &Error{
		Status:  status,
		Code:    "unexpected",
		Message: fmt.Sprintf("the node refused with status %d", status),
	}
}
