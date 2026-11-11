package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reasonix/internal/base/testenv"
	"reasonix/internal/contract/event"
	"reasonix/internal/session/control"
	"reasonix/internal/state/sessioninbox"
	"reasonix/internal/state/sessionstore"
	"reasonix/internal/tools/jobs"
)

// fakeJobController is the acpJobController a notice can consult when a test
// does not want a whole control.Controller.
type fakeJobController struct {
	running []jobs.View
	ended   []jobs.View
	peek    jobs.Peek
	peekOK  bool
}

func (f *fakeJobController) Jobs() []jobs.View                     { return f.running }
func (f *fakeJobController) EndedJobs() []jobs.View                { return f.ended }
func (f *fakeJobController) JobPeek(string, int) (jobs.Peek, bool) { return f.peek, f.peekOK }

// refusingNotifier is a transport that is gone: every notification fails, which
// is the condition the durable replay queue exists for.
type refusingNotifier struct{ fakeNotifier }

func (r *refusingNotifier) Notify(string, any) error { return errors.New("connection closed") }

func TestFinishTurnWithJobsReportsAwaiting(t *testing.T) {
	telemetry := newStatusTelemetry()
	telemetry.beginTurn()
	refs := []ReasonixJobRef{{ID: "job-1", Command: "make test", StartedAt: "2026-11-11T10:00:00Z"}}
	if eventName := telemetry.finishTurnWithJobs(nil, false, "", "done for now", refs); eventName != "pause" {
		t.Fatalf("event = %q, want pause", eventName)
	}
	snapshot := telemetry.snapshot()
	if snapshot.phase != "awaiting_jobs" || snapshot.turnOutcome.Kind != "awaiting_jobs" {
		t.Fatalf("phase/outcome = %q/%q, want awaiting_jobs", snapshot.phase, snapshot.turnOutcome.Kind)
	}
	if len(snapshot.turnOutcome.Jobs) != 1 || snapshot.turnOutcome.Jobs[0].ID != "job-1" {
		t.Errorf("jobs = %+v, want job-1", snapshot.turnOutcome.Jobs)
	}
	if snapshot.finalReadiness.ReadyForReview {
		t.Error("awaiting_jobs reported ready for review while work still runs")
	}
	restored := restoreStatusTelemetry(telemetry.persisted()).snapshot()
	if restored.phase != "awaiting_jobs" || len(restored.turnOutcome.Jobs) != 1 {
		t.Errorf("restored = %q/%+v, want awaiting_jobs with its job", restored.phase, restored.turnOutcome.Jobs)
	}
}

func TestFinishTurnWithJobsKeepsFailureAndCancel(t *testing.T) {
	refs := []ReasonixJobRef{{ID: "job-1"}}
	failed := newStatusTelemetry()
	failed.beginTurn()
	failed.finishTurnWithJobs(errors.New("provider down"), false, "", "", refs)
	if got := failed.snapshot().turnOutcome.Kind; got != "error" {
		t.Errorf("failed turn outcome = %q, want error", got)
	}
	cancelled := newStatusTelemetry()
	cancelled.beginTurn()
	cancelled.finishTurnWithJobs(nil, true, "", "", refs)
	if got := cancelled.snapshot().turnOutcome.Kind; got != "cancelled" {
		t.Errorf("cancelled turn outcome = %q, want cancelled", got)
	}
	clean := newStatusTelemetry()
	clean.beginTurn()
	clean.finishTurnWithJobs(nil, false, "", "", nil)
	if got := clean.snapshot().turnOutcome.Kind; got != "completed" {
		t.Errorf("turn without jobs outcome = %q, want completed", got)
	}
}

func TestAwaitingJobRefsNamesRunningWork(t *testing.T) {
	started := time.Date(2026, 11, 11, 10, 30, 0, 0, time.UTC)
	refs := awaitingJobRefs(&fakeJobController{running: []jobs.View{{
		ID: "job-7", Detail: "gh pr checks --watch", StartedAt: started.UnixMilli(),
	}}})
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want one job", refs)
	}
	if refs[0].ID != "job-7" || refs[0].Command != "gh pr checks --watch" {
		t.Errorf("ref = %+v, want job-7 with its command", refs[0])
	}
	if refs[0].StartedAt != "2026-11-11T10:30:00Z" {
		t.Errorf("startedAt = %q, want RFC3339", refs[0].StartedAt)
	}
	if got := awaitingJobRefs(&fakeJobController{}); got != nil {
		t.Errorf("refs without running jobs = %+v, want nil", got)
	}
	// A controller that does not expose jobs (headless fakes) must not panic.
	if got := awaitingJobRefs(struct{}{}); got != nil {
		t.Errorf("refs for an untracked controller = %+v, want nil", got)
	}
}

func TestJobCompletedFromNoticeCarriesTheEnding(t *testing.T) {
	exit := 2
	ctrl := &fakeJobController{
		ended: []jobs.View{{
			ID: "job-9", Label: "build", Status: "failed",
			StartedAt: 1_000, EndedAt: 4_500, ExitCode: &exit,
		}},
		peek:   jobs.Peek{Text: "go: build failed\n"},
		peekOK: true,
	}
	notice := event.JobNotice{Kind: "bash", ID: "job-9", Label: "build", Error: "exit status 2"}
	payload := jobCompletedFromNotice(notice, event.LevelWarn, ctrl)

	if payload.JobID != "job-9" || payload.Status != "failed" || payload.Level != "warn" {
		t.Errorf("payload = %+v, want a failed job-9 notice", payload)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 2 {
		t.Errorf("exitCode = %v, want 2", payload.ExitCode)
	}
	if payload.DurationMs != 3_500 {
		t.Errorf("durationMs = %d, want 3500", payload.DurationMs)
	}
	if payload.OutputTail != "go: build failed" {
		t.Errorf("outputTail = %q, want the job's tail", payload.OutputTail)
	}
	if payload.SchemaVersion != jobCompletedSchemaVersion {
		t.Errorf("schemaVersion = %d", payload.SchemaVersion)
	}
}

func TestJobCompletedFromNoticeWithoutAJobTable(t *testing.T) {
	notice := event.JobNotice{Kind: "bash", ID: "job-3"}
	payload := jobCompletedFromNotice(notice, event.LevelInfo, nil)
	if payload.Status != string(jobs.Done) || payload.Level != "info" {
		t.Errorf("payload = %+v, want a done info notice", payload)
	}
}

func TestSinkEmitsJobCompletedVendorUpdate(t *testing.T) {
	fn := &fakeNotifier{}
	sink := newUpdateSink(fn, "sess-1")
	sink.bindJobCompletions(true)
	exit := 0
	sink.bindJobs(func() any {
		return &fakeJobController{
			ended:  []jobs.View{{ID: "job-4", Label: "tests", Status: "done", StartedAt: 10, EndedAt: 1_010, ExitCode: &exit}},
			peek:   jobs.Peek{Text: "ok"},
			peekOK: true,
		}
	}, nil)

	sink.Emit(event.Event{
		Kind: event.Notice, Level: event.LevelInfo, Code: event.NoticeCodeJobFinished,
		Detail: event.JobNotice{Kind: "bash", ID: "job-4", Label: "tests"}.Encode(),
	})

	update := fn.updateMap(t, 0)
	if update["sessionUpdate"] != jobCompletedUpdateKind {
		t.Fatalf("sessionUpdate = %v, want %q", update["sessionUpdate"], jobCompletedUpdateKind)
	}
	meta, _ := update["_meta"].(map[string]any)
	reasonix, _ := meta["reasonix.io"].(map[string]any)
	job, _ := reasonix["jobCompleted"].(map[string]any)
	if job["jobId"] != "job-4" || job["status"] != "done" {
		t.Errorf("jobCompleted = %+v, want job-4 done", job)
	}
	if job["durationMs"] != float64(1_000) || job["outputTail"] != "ok" {
		t.Errorf("jobCompleted = %+v, want duration and tail", job)
	}
}

func TestSinkQueuesJobCompletedWhenTransportRefuses(t *testing.T) {
	fn := &refusingNotifier{}
	sink := newUpdateSink(fn, "sess-1")
	sink.bindJobCompletions(true)
	var queued []ReasonixJobCompleted
	sink.bindJobs(func() any { return nil }, func(p ReasonixJobCompleted) { queued = append(queued, p) })

	sink.Emit(event.Event{
		Kind: event.Notice, Level: event.LevelWarn, Code: event.NoticeCodeJobFailed,
		Detail: event.JobNotice{Kind: "bash", ID: "job-5", Error: "boom"}.Encode(),
	})

	if len(queued) != 1 || queued[0].JobID != "job-5" {
		t.Fatalf("queued = %+v, want the refused completion", queued)
	}
}

func TestSinkSkipsJobCompletedWithoutOptIn(t *testing.T) {
	fn := &fakeNotifier{}
	sink := newUpdateSink(fn, "sess-1")
	sink.bindJobs(func() any { return nil }, nil)
	sink.Emit(event.Event{
		Kind: event.Notice, Level: event.LevelInfo, Code: event.NoticeCodeJobFinished,
		Detail: event.JobNotice{Kind: "bash", ID: "job-10"}.Encode(),
	})
	if len(fn.notifs) != 0 {
		t.Fatalf("notifications = %+v, want none without the jobCompletions declaration", fn.notifs)
	}
}

func TestJobCompletionsReplayOnceAndDrop(t *testing.T) {
	fn := &fakeNotifier{}
	sink := newUpdateSink(fn, "sess-1")
	sink.bindJobCompletions(true)
	sess := &acpSession{sink: sink, status: newStatusTelemetry()}
	payload := ReasonixJobCompleted{SchemaVersion: 1, JobID: "job-6", Status: "done"}
	sess.queueJobCompletion(payload)
	sess.queueJobCompletion(payload) // a retried flush must not duplicate it

	if delivered := sess.flushJobCompletions(); delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}
	if len(sess.pendingJobs) != 0 {
		t.Fatalf("pending = %+v, want empty after delivery", sess.pendingJobs)
	}
	if delivered := sess.flushJobCompletions(); delivered != 0 {
		t.Fatalf("second flush delivered = %d, want 0", delivered)
	}
}

func TestJobCompletionQueueKeepsTheNewest(t *testing.T) {
	sess := &acpSession{status: newStatusTelemetry()}
	for i := 0; i <= maxPendingJobCompletions; i++ {
		sess.queueJobCompletion(ReasonixJobCompleted{SchemaVersion: 1, JobID: string(rune('a' + i)), Status: "done"})
	}
	if len(sess.pendingJobs) != maxPendingJobCompletions {
		t.Fatalf("pending = %d, want the bound %d", len(sess.pendingJobs), maxPendingJobCompletions)
	}
	if sess.pendingJobs[0].JobID == "a" {
		t.Error("oldest completion survived past the bound")
	}
}

// ── empty prompt resume ─────────────────────────────────────────────────────

type inboxRecordingRunner struct{ seen chan string }

func (r *inboxRecordingRunner) Run(_ context.Context, input string) error {
	r.seen <- input
	return nil
}

func newResumableController(t *testing.T) (*control.Controller, chan string) {
	t.Helper()
	dir := testenv.TempDir(t)
	runner := &inboxRecordingRunner{seen: make(chan string, 4)}
	ctrl := control.New(control.Options{
		Runner:      runner,
		Sink:        event.Discard,
		SessionDir:  dir,
		SessionPath: filepath.Join(dir, "session.jsonl"),
	})
	t.Cleanup(ctrl.Close)
	return ctrl, runner.seen
}

func TestSessionResumeReadyOnlyForHostWork(t *testing.T) {
	ctrl, _ := newResumableController(t)
	sess := &acpSession{ctrl: ctrl, status: newStatusTelemetry()}

	if sessionResumeReady(sess) {
		t.Fatal("empty session reported resumable")
	}
	if _, err := ctrl.EnqueueInbox(control.InboxRequest{
		Intent: sessioninbox.IntentFollowup, Submit: "typed by the user",
	}); err != nil {
		t.Fatal(err)
	}
	if sessionResumeReady(sess) {
		t.Fatal("a user-authored follow-up made an empty prompt valid")
	}
	if _, err := ctrl.EnqueueInbox(control.InboxRequest{
		Intent: sessioninbox.IntentFollowup, Origin: sessioninbox.OriginHost, Submit: "job finished",
	}); err != nil {
		t.Fatal(err)
	}
	if !sessionResumeReady(sess) {
		t.Fatal("a queued host continuation did not make the session resumable")
	}
	if err := ctrl.SetInboxPaused(true); err != nil {
		t.Fatal(err)
	}
	if sessionResumeReady(sess) {
		t.Fatal("an empty prompt would have overridden the person's pause")
	}
	if err := ctrl.SetInboxPaused(false); err != nil {
		t.Fatal(err)
	}
	if !sessionResumeReady(sess) {
		t.Fatal("unpausing did not restore resumability")
	}
}

func TestSessionResumeReadyAfterLostCompletion(t *testing.T) {
	sess := &acpSession{status: newStatusTelemetry()}
	sess.queueJobCompletion(ReasonixJobCompleted{SchemaVersion: 1, JobID: "job-8", Status: "done"})
	if !sessionResumeReady(sess) {
		t.Fatal("an undelivered job_completed did not make the session resumable")
	}
}

func TestEmptyPromptStillInvalidWithoutPendingWork(t *testing.T) {
	if sessionResumeReady(&acpSession{status: newStatusTelemetry()}) {
		t.Fatal("a session with no controller or queue reported resumable")
	}
	if sessionResumeReady(nil) {
		t.Fatal("a nil session reported resumable")
	}
}

// capturingFactory keeps the controller session/new built so a test can queue
// the host continuation a finished background job would leave.
type capturingFactory struct {
	*configurableFactory
	ctrl *control.Controller
}

func (f *capturingFactory) NewSession(ctx context.Context, p SessionParams) (*control.Controller, error) {
	ctrl, err := f.configurableFactory.NewSession(ctx, p)
	if err != nil {
		return nil, err
	}
	f.ctrl = ctrl
	return ctrl, nil
}

func TestEmptyPromptRunsQueuedHostContinuation(t *testing.T) {
	seen := make(chan string, 4)
	factory := &capturingFactory{configurableFactory: &configurableFactory{
		dir: testenv.TempDir(t),
		behavior: func(_ context.Context, sink event.Sink, input string, _ SessionParams) error {
			seen <- input
			sink.Emit(event.Event{Kind: event.Text, Text: "continued"})
			return nil
		},
	}}
	client, stop := startServer(t, factory)
	defer stop()
	client.call(t, "initialize", InitializeParams{ProtocolVersion: 1})
	resp := client.call(t, "session/new", SessionNewParams{Cwd: testenv.TempDir(t)})
	var nr SessionNewResult
	if err := json.Unmarshal(resp.Result, &nr); err != nil || nr.SessionID == "" {
		t.Fatalf("session/new: %v (%q)", err, nr.SessionID)
	}
	ctrl := factory.ctrl
	if ctrl == nil {
		t.Fatal("session/new built no controller")
	}
	ctrl.EnsureSessionPath()
	if _, err := ctrl.EnqueueInbox(control.InboxRequest{
		Intent: sessioninbox.IntentFollowup, Origin: sessioninbox.OriginHost,
		Submit: "job-7 finished — done",
	}); err != nil {
		t.Fatalf("queue host continuation: %v", err)
	}

	promptCh := client.callAsync("session/prompt", SessionPromptParams{
		SessionID: nr.SessionID,
		Prompt:    []ContentBlock{{Type: "text", Text: ""}},
	})
	_, promptResp := drainPrompt(t, client, promptCh)
	if promptResp.Error != nil {
		t.Fatalf("empty resume prompt errored: %+v", promptResp.Error)
	}
	var pr SessionPromptResult
	if err := json.Unmarshal(promptResp.Result, &pr); err != nil {
		t.Fatalf("prompt result: %v", err)
	}
	if pr.StopReason != StopEndTurn {
		t.Errorf("stopReason = %q, want %q", pr.StopReason, StopEndTurn)
	}
	select {
	case got := <-seen:
		if !strings.Contains(got, "job-7 finished") {
			t.Errorf("resumed turn ran %q, want the queued continuation", got)
		}
	default:
		t.Fatal("the resumed turn never ran the queued continuation")
	}

	empty := client.call(t, "session/prompt", SessionPromptParams{
		SessionID: nr.SessionID,
		Prompt:    []ContentBlock{{Type: "text", Text: ""}},
	})
	if empty.Error == nil || empty.Error.Code != ErrInvalidParams {
		t.Fatalf("second empty prompt = %+v, want InvalidParams", empty)
	}
}

// ── wire-level wake and resume ──────────────────────────────────────────────

var jobCompletionsCaps = ClientCapabilities{Meta: map[string]any{"reasonix.io": map[string]any{
	"jobCompletions": map[string]any{"supported": true},
}}}

// A declared client sees the turn end awaiting jobs, receives the completion as
// its own update, and resumes with an empty prompt that runs the queued result.
func TestE2EJobCompletionReachesADeclaredClient(t *testing.T) {
	dir := testenv.TempDir(t)
	seen := make(chan string, 4)
	factory := &configurableFactory{dir: dir, managers: []*jobs.Manager{},
		behavior: func(_ context.Context, sink event.Sink, input string, _ SessionParams) error {
			seen <- input
			sink.Emit(event.Event{Kind: event.Text, Text: "looked"})
			return nil
		}}
	client, stop := startServer(t, factory)
	defer stop()
	client.call(t, "initialize", InitializeParams{ProtocolVersion: 1, ClientCapabilities: jobCompletionsCaps})
	resp := client.call(t, "session/new", SessionNewParams{Cwd: testenv.TempDir(t)})
	var nr SessionNewResult
	if err := json.Unmarshal(resp.Result, &nr); err != nil || nr.SessionID == "" {
		t.Fatalf("session/new: %v (%q)", err, nr.SessionID)
	}

	jm := factory.managerAt(t, 0)
	release := make(chan struct{})
	started := make(chan struct{})
	job := jm.StartForSession(sessionstore.BranchID(transcriptPath(dir, nr.SessionID)), "bash", "ci watch",
		func(ctx context.Context, _ io.Writer) (string, error) {
			close(started)
			select {
			case <-release:
				return "checks passed", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
	select {
	case <-started:
	case <-time.After(rpcCallBudget(t)):
		t.Fatal("background job never started")
	}

	promptCh := client.callAsync("session/prompt", SessionPromptParams{
		SessionID: nr.SessionID,
		Prompt:    []ContentBlock{{Type: "text", Text: "push and watch CI"}},
	})
	if _, promptResp := drainPrompt(t, client, promptCh); promptResp.Error != nil {
		t.Fatalf("prompt errored: %+v", promptResp.Error)
	} else {
		var pr SessionPromptResult
		if err := json.Unmarshal(promptResp.Result, &pr); err != nil {
			t.Fatalf("prompt result: %v", err)
		}
		if pr.StopReason != StopEndTurn {
			t.Errorf("stopReason = %q, want end_turn", pr.StopReason)
		}
		reasonix, _ := pr.Meta["reasonix.io"].(map[string]any)
		outcome, _ := reasonix["turnOutcome"].(map[string]any)
		if outcome["kind"] != "awaiting_jobs" {
			t.Fatalf("turnOutcome = %+v, want awaiting_jobs", outcome)
		}
		jobsOut, _ := outcome["jobs"].([]any)
		if len(jobsOut) != 1 {
			t.Fatalf("outcome jobs = %+v, want one job", jobsOut)
		}
		if ref, _ := jobsOut[0].(map[string]any); ref["id"] != job.ID {
			t.Errorf("outcome job = %+v, want %s", jobsOut[0], job.ID)
		}
	}

	close(release)
	var completed map[string]any
	for completed == nil {
		select {
		case f := <-client.notifs:
			if updateKind(t, f) != jobCompletedUpdateKind {
				continue
			}
			var update struct {
				Update struct {
					Meta map[string]any `json:"_meta"`
				} `json:"update"`
			}
			if err := json.Unmarshal(f.Params, &update); err != nil {
				t.Fatalf("decode completion update: %v", err)
			}
			reasonix, _ := update.Update.Meta["reasonix.io"].(map[string]any)
			completed, _ = reasonix["jobCompleted"].(map[string]any)
		case <-time.After(rpcCallBudget(t)):
			t.Fatal("job completion never reached the declared client")
		}
	}
	if completed["jobId"] != job.ID || completed["status"] != string(jobs.Done) {
		t.Errorf("jobCompleted = %+v, want %s done", completed, job.ID)
	}

	resumeCh := client.callAsync("session/prompt", SessionPromptParams{
		SessionID: nr.SessionID,
		Prompt:    []ContentBlock{{Type: "text", Text: ""}},
	})
	if _, resumeResp := drainPrompt(t, client, resumeCh); resumeResp.Error != nil {
		t.Fatalf("resume prompt errored: %+v", resumeResp.Error)
	}
	// The first entry is the original turn; the resume turn follows it.
	select {
	case <-seen:
	default:
		t.Fatal("the original prompt never ran")
	}
	select {
	case got := <-seen:
		if !strings.Contains(got, "<background-jobs>") || !strings.Contains(got, job.ID) {
			t.Errorf("resumed turn ran %q, want the queued job result", got)
		}
	default:
		t.Fatal("the resumed turn never ran the queued job result")
	}
}
