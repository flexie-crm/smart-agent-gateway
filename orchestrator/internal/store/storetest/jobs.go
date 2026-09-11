package storetest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The durable job queue. What matters here is not that a job can be enqueued
// and run, which is the easy half: it is that a worker dying at any point in
// the lifecycle costs at most one duplicate delivery and never a lost job.

func enqueueJob(t *testing.T, st store.Store, ws int64, kind, subject string) *model.Job {
	t.Helper()
	j := &model.Job{WorkspaceID: ws, Kind: kind, Subject: subject, Payload: []byte(`{"a":1}`)}
	if err := st.Jobs().Enqueue(context.Background(), j); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return j
}

func testJobs(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "jobs")

	j := enqueueJob(t, st, ws.ID, "chat.title", "jobs.default.chat.title")
	// AFTER the rows exist. A job is runnable from its run_after, which is set
	// at enqueue, so a `now` captured first silently makes everything invisible
	// and the test passes only when the two land in the same millisecond.
	if j.ID == "" {
		t.Fatal("enqueue did not mint an id")
	}
	if j.Status != model.JobPending || j.MaxAttempts != model.DefaultJobAttempts {
		t.Fatalf("a fresh job should be pending with the default attempts: %+v", j)
	}

	// Claimed by SUBJECT PREFIX, which is what a worker's capabilities are.
	claimed, err := st.Jobs().Claim(ctx, "node-a#1", "jobs.default.", 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != j.ID {
		t.Fatalf("the job was not claimed: %+v", claimed)
	}
	if claimed[0].Attempts != 1 {
		t.Fatalf("claiming is the attempt, so it counts at claim time: %d", claimed[0].Attempts)
	}
	if string(claimed[0].Payload) != `{"a":1}` {
		t.Fatalf("the payload did not survive: %q", claimed[0].Payload)
	}

	// A job in flight is claimed by nobody else, however many ask.
	again, err := st.Jobs().Claim(ctx, "node-b#1", "jobs.default.", 10)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a running job was handed to a second worker: %+v", again)
	}

	if err := st.Jobs().Complete(ctx, j.ID, "node-a#1"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	done, err := st.Jobs().Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if done.Status != model.JobCompleted || done.CompletedAt == nil || done.LockedBy != "" {
		t.Fatalf("a completed job should be finished and unlocked: %+v", done)
	}
}

// A worker that took a job and stopped must not take its work with it.
func testJobReclaim(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-reclaim")

	j := enqueueJob(t, st, ws.ID, "chat.title", "jobs.default.chat.title")
	if _, err := st.Jobs().Claim(ctx, "dead-node#1", "jobs.default.", 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Nothing is reclaimed while the claim still looks alive.
	n, err := st.Jobs().Reclaim(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("early reclaim: %v", err)
	}
	if n != 0 {
		t.Fatalf("a live claim was reclaimed: %d", n)
	}

	// Once it is stale, the job goes back and another worker gets it.
	if n, err = st.Jobs().Reclaim(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("reclaim: n=%d err=%v", n, err)
	}
	back, err := st.Jobs().Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if back.Status != model.JobPending || back.LockedBy != "" {
		t.Fatalf("a reclaimed job should be pending and unlocked: %+v", back)
	}
	if back.Attempts != 1 {
		t.Fatalf("a reclaimed job keeps its spent attempt, or a job that kills its worker runs forever: %d", back.Attempts)
	}
	if back.LastError == "" {
		t.Fatal("a reclaimed job should say why it came back")
	}

	retaken, err := st.Jobs().Claim(ctx, "live-node#1", "jobs.default.", 10)
	if err != nil || len(retaken) != 1 {
		t.Fatalf("a reclaimed job was not claimable: %d %v", len(retaken), err)
	}

	// And the dead worker, if it ever wakes, cannot finish what it lost.
	err = st.Jobs().Complete(ctx, j.ID, "dead-node#1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a lost claim was allowed to complete the job: %v", err)
	}
}

// Failing is not the same as being finished with: a job is tried again until
// its attempts are spent, and only then left dead for a person to find.
func testJobRetryAndDeath(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-retry")

	// One job with an attempt to spare: failing sends it back to pending, with
	// the reason, and its backoff really holds it there.
	retryable := &model.Job{
		WorkspaceID: ws.ID, Kind: "chat.title", Subject: "jobs.default.chat.title",
		MaxAttempts: 2,
	}
	if err := st.Jobs().Enqueue(ctx, retryable); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.Jobs().Claim(ctx, "n#1", "jobs.default.", 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.Jobs().Fail(ctx, retryable.ID, "n#1", "the vendor said no", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("fail: %v", err)
	}
	got, err := st.Jobs().Get(ctx, retryable.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.JobPending || got.LastError != "the vendor said no" {
		t.Fatalf("a failed job with attempts left should be pending, with the reason: %+v", got)
	}
	early, err := st.Jobs().Claim(ctx, "n#2", "jobs.default.", 10)
	if err != nil {
		t.Fatalf("early claim: %v", err)
	}
	if len(early) != 0 {
		t.Fatalf("a backed-off job was claimed before its time: %+v", early)
	}

	// And one with no attempt to spare: failing leaves it dead, whatever
	// backoff was asked for, and nothing hands it out again.
	last := &model.Job{
		WorkspaceID: ws.ID, Kind: "chat.title", Subject: "jobs.other.chat.title",
		MaxAttempts: 1,
	}
	if err := st.Jobs().Enqueue(ctx, last); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.Jobs().Claim(ctx, "n#3", "jobs.other.", 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.Jobs().Fail(ctx, last.ID, "n#3", "the vendor said no again", time.Now().UTC()); err != nil {
		t.Fatalf("second fail: %v", err)
	}
	dead, err := st.Jobs().Get(ctx, last.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if dead.Status != model.JobDead || dead.CompletedAt == nil {
		t.Fatalf("a job out of attempts should be dead, not retried forever: %+v", dead)
	}
	if !dead.Terminal() {
		t.Fatal("a dead job should read as finished with")
	}
	none, err := st.Jobs().Claim(ctx, "n#4", "jobs.other.", 10)
	if err != nil {
		t.Fatalf("claim after death: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("a dead job was claimed: %+v", none)
	}
}

// Being asked to stop is not a failure. A worker shutting down hands its
// unstarted work back, and the attempt goes back with it.
func testJobRelease(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-release")

	j := enqueueJob(t, st, ws.ID, "chat.title", "jobs.default.chat.title")
	if _, err := st.Jobs().Claim(ctx, "n#1", "jobs.default.", 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.Jobs().Release(ctx, j.ID, "n#1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	back, _ := st.Jobs().Get(ctx, j.ID)
	if back.Status != model.JobPending {
		t.Fatalf("a released job should be pending: %+v", back)
	}
	if back.Attempts != 0 {
		t.Fatalf("a released job keeps its attempts: three deploys must not exhaust a job that never ran: %d", back.Attempts)
	}
}

// A worker only picks up what it serves.
func testJobSubjectScoping(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-subjects")

	enqueueJob(t, st, ws.ID, "chat.title", "jobs.default.chat.title")
	gpu := enqueueJob(t, st, ws.ID, "model.pull", "jobs.model-host.model.pull")

	claimed, err := st.Jobs().Claim(ctx, "gpu-node#1", "jobs.model-host.", 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != gpu.ID {
		t.Fatalf("a worker took work outside its capabilities: %+v", claimed)
	}
}

// The claim is unique per CALL, not per worker: two goroutines on one node
// claiming at the same moment must not read back each other's rows.
func testJobConcurrentClaims(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-concurrent")

	const total = 12
	for i := 0; i < total; i++ {
		enqueueJob(t, st, ws.ID, "chat.title", "jobs.default.chat.title")
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]string{} // job id -> the claim that took it
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			claim := "node-a#" + string(rune('a'+n))
			got, err := st.Jobs().Claim(ctx, claim, "jobs.default.", 3)
			if err != nil {
				t.Errorf("claim %s: %v", claim, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, j := range got {
				if other, dup := seen[j.ID]; dup {
					t.Errorf("job %s claimed by both %s and %s", j.ID, other, claim)
				}
				seen[j.ID] = claim
				if j.LockedBy != claim {
					t.Errorf("claim %s read back a row locked by %s", claim, j.LockedBy)
				}
			}
		}(i)
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("four workers taking three each should take all twelve, got %d", len(seen))
	}
}

// Deleting a workspace takes its jobs, so a tenant leaves nothing behind.
func testJobWorkspaceCascade(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-cascade")
	j := enqueueJob(t, st, ws.ID, "chat.title", "jobs.default.chat.title")

	if err := st.Workspaces().Delete(ctx, ws.ID); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	if _, err := st.Jobs().Get(ctx, j.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a deleted workspace left its jobs behind: %v", err)
	}
}

// claimed says whether a claim took a particular job, which is what turns "the
// heartbeat says not found" into "the job under test was never claimed".
func claimed(held []*model.Job, id string) bool {
	for _, j := range held {
		if j.ID == id {
			return true
		}
	}
	return false
}

// A worker that is still working says so, and keeps its job. Without this a
// model pull is reclaimed underneath itself, run again on another node while
// the first is still downloading, and finally marked dead while it is in fact
// succeeding.
func testJobHeartbeat(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-heartbeat")

	// A subject of this test's own. Three tests here claim "jobs.model-host."
	// and a claim is ORDER BY run_after LIMIT 10: rows enqueued in the same
	// millisecond tie, MySQL is free to order a tie however it likes, and this
	// test then heartbeats a job somebody else was given. That is the whole of
	// the intermittent "heartbeat job: store: not found" it used to fail with.
	j := enqueueJob(t, st, ws.ID, "model.pull", "jobs.heartbeat.model.pull")
	held, err := st.Jobs().Claim(ctx, "puller#1", "jobs.heartbeat.", 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed(held, j.ID) {
		t.Fatalf("the claim did not take the job under test: %v", held)
	}

	// A long job that keeps saying it is alive is never reclaimed, however long
	// it takes. The lease is renewed, not extended once.
	for i := 0; i < 3; i++ {
		if err := st.Jobs().Heartbeat(ctx, j.ID, "puller#1"); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
		n, err := st.Jobs().Reclaim(ctx, time.Minute, 0)
		if err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		if n != 0 {
			t.Fatal("a job whose worker is still reporting was taken away from it")
		}
	}
	// Reporting OFTEN is still reporting. The lease is a datetime(3), so
	// heartbeats close together write the same value and change no rows, and an
	// UPDATE counts rows changed rather than rows matched: read that as "you
	// have lost the job" and a worker holding a perfectly good claim is told to
	// stop, which for a model pull means abandoning a download that was going to
	// succeed. Two hundred in a row here is a few dozen collisions on any real
	// machine, and none of them is a lost job.
	for i := 0; i < 200; i++ {
		if err := st.Jobs().Heartbeat(ctx, j.ID, "puller#1"); err != nil {
			t.Fatalf("heartbeat %d of a job still held: %v", i, err)
		}
	}

	if err := st.Jobs().Complete(ctx, j.ID, "puller#1"); err != nil {
		t.Fatalf("complete after heartbeats: %v", err)
	}

	// And a worker that HAS lost its job is told so by the heartbeat, which is
	// how it learns to stop rather than finding out when Complete is refused.
	k := enqueueJob(t, st, ws.ID, "model.pull", "jobs.heartbeat.model.pull")
	lost, err := st.Jobs().Claim(ctx, "slow#1", "jobs.heartbeat.", 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed(lost, k.ID) {
		t.Fatalf("the claim did not take the job under test: %v", lost)
	}
	if _, err := st.Jobs().Reclaim(ctx, 0, 0); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if err := st.Jobs().Heartbeat(ctx, k.ID, "slow#1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a worker that lost its job was told it still holds it: %v", err)
	}
}

// A job that reliably kills whatever picks it up must run out of attempts
// rather than circulate through the fleet forever. This is the branch of
// Reclaim that says so, and nothing else exercises it.
func testJobReclaimDeath(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-reclaim-death")

	j := &model.Job{
		WorkspaceID: ws.ID, Kind: "poison", Subject: "jobs.default.poison",
		MaxAttempts: 1,
	}
	if err := st.Jobs().Enqueue(ctx, j); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.Jobs().Claim(ctx, "victim#1", "jobs.default.", 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.Jobs().Reclaim(ctx, 0, 0); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	dead, err := st.Jobs().Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if dead.Status != model.JobDead || dead.CompletedAt == nil {
		t.Fatalf("a job that spent its last attempt killing a worker should be dead: %+v", dead)
	}
	none, err := st.Jobs().Claim(ctx, "next#1", "jobs.default.", 10)
	if err != nil || len(none) != 0 {
		t.Fatalf("a job dead by reclaim was handed out again: %d %v", len(none), err)
	}
}

// A worker that lost its job cannot finish it, fail it, or hand it back. Only
// Complete was covered; these are the other two doors into the same row.
func testJobLostClaim(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-lost-claim")

	j := enqueueJob(t, st, ws.ID, "chat.title", "jobs.default.chat.title")
	if _, err := st.Jobs().Claim(ctx, "gone#1", "jobs.default.", 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.Jobs().Reclaim(ctx, 0, 0); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	if err := st.Jobs().Fail(ctx, j.ID, "gone#1", "too late", time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a lost claim failed somebody else's job: %v", err)
	}
	if err := st.Jobs().Release(ctx, j.ID, "gone#1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a lost claim released somebody else's job: %v", err)
	}
}

// The reason a job failed is the one thing a person opens a dead job to read,
// and it must survive being long and not being English.
func testJobLongFailureReason(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-long-error")

	j := enqueueJob(t, st, ws.ID, "chat.title", "jobs.default.chat.title")
	if _, err := st.Jobs().Claim(ctx, "n#1", "jobs.default.", 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Three-byte runes, so a cut at a byte boundary lands mid-character two
	// times in three, and the connection refuses the whole statement.
	reason := strings.Repeat("模型返回了一个错误", 500)
	if err := st.Jobs().Fail(ctx, j.ID, "n#1", reason, time.Now().UTC()); err != nil {
		t.Fatalf("fail with a long non-ASCII reason: %v", err)
	}
	got, err := st.Jobs().Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastError == "" {
		t.Fatal("the reason was lost, which is the moment it was worth keeping")
	}
	if !utf8.ValidString(got.LastError) {
		t.Fatal("the stored reason is not valid text")
	}
	// And the backoff was written, which the failed statement would have taken
	// with it.
	if got.Status != model.JobPending {
		t.Fatalf("a failed job did not go back to pending: %+v", got)
	}
}

// A capability name is not a pattern. Under LIKE, `_` matches any character, so
// a node serving "default_" would quietly claim "default." work.
func testJobPrefixIsNotAPattern(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-prefix")

	enqueueJob(t, st, ws.ID, "chat.title", "jobs.default.chat.title")

	for _, prefix := range []string{"jobs.default_", "jobs.defaul%", "jobs.%"} {
		got, err := st.Jobs().Claim(ctx, "sneaky#"+prefix, prefix, 10)
		if err != nil {
			t.Fatalf("claim %q: %v", prefix, err)
		}
		if len(got) != 0 {
			t.Fatalf("prefix %q was read as a pattern and claimed %d job(s)", prefix, len(got))
		}
	}
}

// A job with nothing to say stores nothing, rather than falling foul of the
// column's json_valid check.
func testJobEmptyPayload(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-empty-payload")

	j := &model.Job{WorkspaceID: ws.ID, Kind: "tick", Subject: "jobs.default.tick"}
	if err := st.Jobs().Enqueue(ctx, j); err != nil {
		t.Fatalf("enqueue with no payload: %v", err)
	}
	got, err := st.Jobs().Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Payload) != 0 {
		t.Fatalf("an empty payload came back as %q", got.Payload)
	}
}

// Enqueue decides the status, not the caller. A row written as anything else
// matches no scan and would sit there forever, committed and invisible.
func testJobEnqueueForcesPending(t *testing.T, st store.Store) {
	ctx := context.Background()
	ws := mustWorkspace(t, st, "job-enqueue-status")

	j := &model.Job{
		WorkspaceID: ws.ID, Kind: "chat.title", Subject: "jobs.default.chat.title",
		Status: model.JobRunning, Attempts: 3,
	}
	if err := st.Jobs().Enqueue(ctx, j); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := st.Jobs().Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.JobPending || got.Attempts != 0 {
		t.Fatalf("the caller's status and attempts were trusted: %+v", got)
	}
	claimed, err := st.Jobs().Claim(ctx, "n#1", "jobs.default.", 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("the job was not claimable: %d %v", len(claimed), err)
	}
}
