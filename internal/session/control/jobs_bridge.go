// jobs_bridge.go — a terminal background job becomes a turn the session runs.
package control

import (
	"context"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/runtime/taskmonitor"
	"reasonix/internal/state/sessioninbox"
	"reasonix/internal/state/sessionstore"
	"reasonix/internal/tools/jobs"
)

// hostContinuationSource labels inbox items the runtime authored for itself.
const hostContinuationSource = "host:background-job"

// borrowsJobs reports whether the job manager and write lease still belong to
// another controller: one that adopted them is not the one they answer to until
// TakeOverJobs, so a switch that never publishes leaves them as they were.
func (c *Controller) borrowsJobs() bool {
	return c.jobs != nil && c.jobs.Observed() && !c.jobs.ObservedBy(c)
}

// observeJobs binds what the job table and the write lease answer to — the
// session identity the claim is filed under, the task-store recorder, and this
// controller as the completion observer — unless they are still borrowed. The
// recorder swallows its own failures, and resolves the session id lazily because
// the session path is only fixed once the first turn begins.
func (c *Controller) observeJobs(taskStore taskmonitor.WriteStore) {
	if c.borrowsJobs() {
		return
	}
	c.workspaceLease.SetSessionID(func() string { return sessionstore.BranchID(c.SessionPath()) })
	if c.jobs == nil {
		return
	}
	if rec, ok := c.jobs.Recorder().(*taskmonitor.TaskRecorder); ok && rec != nil {
		rec.Rebind(func() string { return c.parentSessionID() })
	} else if c.workspaceRoot != "" {
		if taskStore == nil {
			taskStore = taskmonitor.NewFileStore(taskmonitor.StoreDir)
		}
		c.jobs.SetTaskRecorder(taskmonitor.NewTaskRecorder(taskStore, c.workspaceRoot,
			func() string { return c.parentSessionID() }))
	}
	// The manager keeps its own note either way, so a controller that cannot
	// take an event costs nothing.
	c.jobs.SetCompletionObserver(c)
	c.jobs.SetActiveSessionPath(c.parentSessionID(), c.SessionPath())
}

// TakeOverJobs makes this controller the owner of the job manager and write
// lease it adopted. A switch calls it once the replacement is published, so a
// completion arriving before then still reaches the controller that is serving.
func (c *Controller) TakeOverJobs() {
	if c.jobs != nil {
		c.jobs.SetCompletionObserver(nil)
	}
	c.observeJobs(c.taskStore)
}

// SetExternalBackgroundResume leaves the turn a finished background job would
// open to the frontend. The completion still becomes a durable host
// continuation; it just waits for the frontend to run it through its own turn
// admission, which is what an ACP client needs to keep control of the slot.
func (c *Controller) SetExternalBackgroundResume(external bool) {
	if c == nil {
		return
	}
	c.externalBackgroundResume.Store(external)
}

// OnJobCompletion implements jobs.CompletionObserver: a terminal background job
// gives its session a turn instead of waiting for the user's next message. An
// error means the event never reached the durable queue, and the manager's own
// note carries it into the next real turn instead.
func (c *Controller) OnJobCompletion(_ context.Context, ev jobs.CompletionEvent) error {
	if c == nil {
		return fmt.Errorf("control: no controller for job %s", ev.JobID)
	}
	// Jobs owned by a session that is not the active one keep their queued note
	// until that session is active again — the same rule the drain path applies.
	if session := strings.TrimSpace(ev.SessionID); session != "" && session != c.parentSessionID() {
		return fmt.Errorf("control: job %s belongs to another session", ev.JobID)
	}
	// A run with no persisted session (headless `run`, e2ebench) has no durable
	// inbox to own the continuation, and must degrade rather than fail.
	if c.SessionPath() == "" {
		return fmt.Errorf("control: session has no durable inbox")
	}
	// A burst is one interruption, not one per job: thirty failing jobs queued
	// thirty turns. While a continuation is still unread, later completions stay
	// in the manager's note and ride that turn's <background-jobs> block.
	if c.hostContinuationPending() {
		return fmt.Errorf("control: job %s folds into a queued continuation", ev.JobID)
	}
	display, submit := c.completionTexts(ev)
	if submit == "" {
		return fmt.Errorf("control: job %s produced no continuation", ev.JobID)
	}
	_, err := c.enqueueHostContinuation(InboxRequest{
		Display:     display,
		Submit:      submit,
		Source:      hostContinuationSource,
		Idempotency: ev.ID,
	}, !c.externalBackgroundResume.Load())
	return err
}

// completionTexts is the host's account of what finished, with the result the
// model would otherwise have to read back through wait. The job's own output is
// bounded: a continuation is context the model pays for, not a transcript.
func (c *Controller) completionTexts(ev jobs.CompletionEvent) (display, submit string) {
	var view jobs.View
	for _, ended := range c.EndedJobs() {
		if ended.ID == ev.JobID {
			view = ended
			break
		}
	}
	peek, _ := c.JobPeek(ev.JobID, completionOutputBytes)
	return backgroundCompletionTexts(ev, view, peek)
}

const completionOutputBytes = 2_000

// hostContinuationPending reports whether a runtime-authored continuation is
// still waiting for a turn to read it. Consumed and running items are past
// that point, so a completion arriving after one of those opens its own turn.
func (c *Controller) hostContinuationPending() bool {
	for _, it := range c.InboxSnapshot().Items {
		if it.Source != hostContinuationSource {
			continue
		}
		switch it.State {
		case sessioninbox.StateQueued, sessioninbox.StateSteerAccepted,
			sessioninbox.StateBlocked, sessioninbox.StateUncertain:
			return true
		}
	}
	return false
}

// EnqueueHostContinuation durably queues runtime-authored work and delivers it
// into the live turn, or as the next one. Which of the two is never decided by
// reading whether a turn is running — the turn can end in that gap — but by the
// steer attempt itself, whose refusal converts the item and kicks the
// dispatcher. A nil error means durable, not read.
func (c *Controller) EnqueueHostContinuation(req InboxRequest) (sessioninbox.InboxReceipt, error) {
	return c.enqueueHostContinuation(req, true)
}

// enqueueHostContinuation is EnqueueHostContinuation with the wake-up made
// explicit. wake=false keeps the item durable and steerable but leaves the
// dispatcher alone, so a frontend that owns turn admission can submit it.
func (c *Controller) enqueueHostContinuation(req InboxRequest, wake bool) (sessioninbox.InboxReceipt, error) {
	req.Intent = sessioninbox.IntentSteer
	req.Origin = sessioninbox.OriginHost
	rec, err := c.EnqueueInbox(req)
	if err != nil {
		return rec, err
	}
	if rec.Idempotent {
		return rec, nil
	}
	steered, steerErr := c.steerInboxItem(rec.ItemID, false, wake)
	if steerErr != nil {
		// Durable and still queued: delivery is the dispatcher's now, unless the
		// frontend asked to run it through its own turn admission.
		if wake {
			c.maybeDispatchInbox()
		}
		return rec, nil
	}
	return steered, nil
}

// backgroundCompletionTexts is the host's own account of what finished, in the
// two forms it is read in. The model gets the tagged block; the queue row and
// the transcript get the fact alone, because Display is persisted as the turn's
// RawContent — one string for both is how <background-jobs> markup reached the
// pending-queue rows as if the user had typed it.
func backgroundCompletionTexts(ev jobs.CompletionEvent, view jobs.View, peek jobs.Peek) (display, submit string) {
	tag := ev.JobID
	if ev.Label != "" {
		tag = fmt.Sprintf("%s (%s)", ev.JobID, ev.Label)
	}
	if tag == "" {
		return "", ""
	}
	display = fmt.Sprintf("%s — %s", tag, ev.Status)
	ending := display
	if view.ExitCode != nil {
		ending += fmt.Sprintf(", exit %d", *view.ExitCode)
	}
	if view.EndedAt > 0 && view.StartedAt > 0 && view.EndedAt >= view.StartedAt {
		ending += fmt.Sprintf(" in %s", (time.Duration(view.EndedAt-view.StartedAt) * time.Millisecond).Round(time.Second))
	}
	var b strings.Builder
	b.WriteString("<background-jobs>\n")
	b.WriteString(ending + "\n")
	if tail := strings.TrimSpace(peek.Text); tail != "" {
		b.WriteString("last output:\n")
		b.WriteString(tail)
		if !strings.HasSuffix(tail, "\n") {
			b.WriteString("\n")
		}
	} else {
		b.WriteString("It produced no output.\n")
	}
	b.WriteString("</background-jobs>\n\n")
	b.WriteString("A background job you started has finished; its result is above. Continue the work it was part of. Read more with wait or bash_output only if the tail is not enough, do not redo what it already did, and end the turn if nothing is left to do.")
	return display, b.String()
}

// Jobs returns the still-running background jobs for the status bar (nil when
// background jobs are disabled).
func (c *Controller) Jobs() []jobs.View {
	if c.jobs == nil {
		return nil
	}
	return c.jobs.RunningForSession(c.parentSessionID())
}

// EndedJobs returns this session's background jobs that have finished, with
// how each ended, for as long as the session keeps them.
func (c *Controller) EndedJobs() []jobs.View {
	if c.jobs == nil {
		return nil
	}
	return c.jobs.EndedForSession(c.parentSessionID())
}

// KillJob cancels a running background job by ID.
func (c *Controller) KillJob(id string) bool {
	if c.jobs == nil {
		return false
	}
	return c.jobs.Kill(id)
}

// JobPeek is the end of one running job's output, for a person looking at it.
// It does not consume what the model's bash_output has yet to read.
func (c *Controller) JobPeek(id string, max int) (jobs.Peek, bool) {
	if c.jobs == nil {
		return jobs.Peek{}, false
	}
	return c.jobs.PeekForSession(c.parentSessionID(), id, max)
}

// CancelJob stops one background job owned by this controller's session.
func (c *Controller) CancelJob(id string) bool {
	if c.jobs == nil {
		return false
	}
	return c.jobs.KillForSession(c.parentSessionID(), id)
}

// CancelToolCall stops one running tool call of the active turn. The turn goes
// on: the model is told the call was stopped and why.
func (c *Controller) CancelToolCall(id string) bool {
	if c.executor == nil {
		return false
	}
	return c.executor.CancelToolCall(id)
}
