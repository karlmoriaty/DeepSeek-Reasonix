package acp

// Background jobs that outlive a turn. ACP clients may park the agent once a
// turn ends with jobs pending (see ReasonixTurnOutcome.Kind "awaiting_jobs"),
// so the completion has to reach them as its own update and the session must
// stay resumable through an ordinary session/prompt.

import (
	"strings"
	"time"

	"reasonix/internal/contract/event"
	"reasonix/internal/state/sessioninbox"
	"reasonix/internal/tools/jobs"
)

const (
	// jobCompletedUpdateKind discriminates the vendor session/update variant
	// that reports a finished background job. It follows the same
	// "_reasonix.io/" namespace rule as extensionSurfaceUpdateKind: ACP owns
	// the bare discriminators.
	jobCompletedUpdateKind = "_reasonix.io/job_completed"
	// jobCompletedSchemaVersion versions the payload carried by that variant.
	jobCompletedSchemaVersion = 1
	// maxPendingJobCompletions bounds the durable replay queue. A build that
	// finishes forty jobs at once must not turn the sidecar into a transcript.
	maxPendingJobCompletions = 32
	// jobCompletionTailBytes is how much of a job's output rides the notice.
	// The result is context the client passes on, not a terminal transcript.
	jobCompletionTailBytes = 2_000
)

// jobCompletedUpdate is the vendor session/update variant that reports one
// finished job to a client that parked the session. Everything it knows rides
// _meta["reasonix.io"]; the discriminator alone says what happened.
type jobCompletedUpdate struct {
	SessionUpdate string         `json:"sessionUpdate"`
	Meta          map[string]any `json:"_meta"`
}

// ReasonixJobCompleted is what one job_completed update carries: the identity
// and outcome of the job, how long it ran, and a bounded tail of its output.
type ReasonixJobCompleted struct {
	SchemaVersion int    `json:"schemaVersion"`
	JobID         string `json:"jobId"`
	Kind          string `json:"kind,omitempty"`
	Label         string `json:"label,omitempty"`
	Status        string `json:"status"`
	ExitCode      *int   `json:"exitCode,omitempty"`
	Signal        *int   `json:"signal,omitempty"`
	DurationMs    int64  `json:"durationMs,omitempty"`
	OutputTail    string `json:"outputTail,omitempty"`
	// Level is "info" for a job that finished and "warn" for one that failed,
	// so a client can style the row without parsing the status.
	Level     string `json:"level,omitempty"`
	Completed string `json:"completedAt,omitempty"`
}

// acpJobController is the slice of the controller the job notices need. It is
// an optional capability: controller fakes that do not track jobs simply skip
// awaiting_jobs and send notices without the ended-job detail.
type acpJobController interface {
	Jobs() []jobs.View
	EndedJobs() []jobs.View
	JobPeek(id string, max int) (jobs.Peek, bool)
}

// awaitingJobRefs lists the background jobs still running when a turn ends.
// The command is the one a person would recognize (Detail, then Description,
// then the short Label); StartedAt is RFC3339 so a client does not have to
// know the host's millisecond epoch.
func awaitingJobRefs(ctrl any) []ReasonixJobRef {
	tracked, ok := ctrl.(acpJobController)
	if !ok {
		return nil
	}
	running := tracked.Jobs()
	if len(running) == 0 {
		return nil
	}
	refs := make([]ReasonixJobRef, 0, len(running))
	for _, view := range running {
		ref := ReasonixJobRef{ID: view.ID, Command: firstNonEmpty(firstNonEmpty(view.Detail, view.Description), view.Label)}
		if view.StartedAt > 0 {
			ref.StartedAt = time.UnixMilli(view.StartedAt).UTC().Format(time.RFC3339)
		}
		refs = append(refs, ref)
	}
	return refs
}

// jobCompletedFromNotice turns one terminal job notice into the update payload.
// The notice already carries the job's identity and, for a failure, its exit
// status; the controller lookup adds the ending the notice does not repeat (a
// successful exit code, the start/end times, the output tail). Both are best
// effort: a notice that cannot be enriched still tells the client what ended.
func jobCompletedFromNotice(notice event.JobNotice, level event.Level, ctrl acpJobController) ReasonixJobCompleted {
	payload := ReasonixJobCompleted{
		SchemaVersion: jobCompletedSchemaVersion,
		JobID:         notice.ID,
		Kind:          notice.Kind,
		Label:         notice.Label,
		Status:        jobStatusFromNotice(level),
		Level:         "info",
		Completed:     time.Now().UTC().Format(time.RFC3339),
	}
	if level == event.LevelWarn {
		payload.Level = "warn"
	}
	if notice.ExitCode != nil {
		code := *notice.ExitCode
		payload.ExitCode = &code
	}
	if notice.Signal != 0 {
		signal := notice.Signal
		payload.Signal = &signal
	}
	if ctrl == nil {
		return payload
	}
	for _, ended := range ctrl.EndedJobs() {
		if ended.ID != notice.ID {
			continue
		}
		if status := strings.TrimSpace(ended.Status); status != "" {
			payload.Status = status
		}
		if payload.ExitCode == nil && ended.ExitCode != nil {
			code := *ended.ExitCode
			payload.ExitCode = &code
		}
		if ended.StartedAt > 0 && ended.EndedAt >= ended.StartedAt {
			payload.DurationMs = ended.EndedAt - ended.StartedAt
		}
		if payload.Label == "" {
			payload.Label = ended.Label
		}
		break
	}
	if peek, ok := ctrl.JobPeek(notice.ID, jobCompletionTailBytes); ok {
		payload.OutputTail = clipStatusText(peek.Text, jobCompletionTailBytes)
	}
	return payload
}

// jobStatusFromNotice is the fallback status when the job table no longer
// knows the job (a controller that adopted a replaced manager, or a test).
func jobStatusFromNotice(level event.Level) string {
	if level == event.LevelWarn {
		return string(jobs.Failed)
	}
	return string(jobs.Done)
}

// currentJobController resolves the session's live controller at notice time
// rather than binding one: an idle config rebuild swaps controllers while the
// job manager, and this sink, stay the session's.
func (s *updateSink) currentJobController() acpJobController {
	s.mu.Lock()
	lookup := s.jobLookup
	s.mu.Unlock()
	if lookup == nil {
		return nil
	}
	tracked, _ := lookup().(acpJobController)
	return tracked
}

// bindJobs installs the controller lookup and the durable queue a completion
// falls back to when the transport refuses the notification.
func (s *updateSink) bindJobs(lookup func() any, queue func(ReasonixJobCompleted)) {
	s.mu.Lock()
	s.jobLookup = lookup
	s.queueJob = queue
	s.mu.Unlock()
}

// jobCompletionsSupported reports whether the client opted in to the vendor
// update. The capability is fixed per connection, so reading it at send time is
// stable.
func (s *updateSink) jobCompletionsSupported() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobCompletions
}

// emitJobCompleted maps a terminal job notice onto the vendor update. A
// transport failure queues the payload for the session's next load or prompt,
// so a completion that happened while the client was away is not lost.
func (s *updateSink) emitJobCompleted(e event.Event) {
	if !s.jobCompletionsSupported() {
		return
	}
	notice, ok := event.DecodeJobNotice(e.Detail)
	if !ok {
		return
	}
	payload := jobCompletedFromNotice(notice, e.Level, s.currentJobController())
	if err := s.sendJobCompleted(payload); err != nil {
		s.mu.Lock()
		queue := s.queueJob
		s.mu.Unlock()
		if queue != nil {
			queue(payload)
		}
	}
}

func (s *updateSink) sendJobCompleted(payload ReasonixJobCompleted) error {
	return s.conn.Notify("session/update", SessionUpdateParams{
		SessionID: s.sessionID,
		Update: jobCompletedUpdate{
			SessionUpdate: jobCompletedUpdateKind,
			Meta:          map[string]any{"reasonix.io": map[string]any{"jobCompleted": payload}},
		},
	})
}

// sessionResumeReady reports whether an empty session/prompt has something to
// do: a host continuation left by a finished job, or a completion the client
// has not received yet. A paused queue does not count — the person stopped it,
// and an empty prompt must not override that.
func sessionResumeReady(sess *acpSession) bool {
	if sess == nil {
		return false
	}
	sess.mu.Lock()
	pending := len(sess.pendingJobs) > 0
	sess.mu.Unlock()
	if pending {
		return true
	}
	inbox, ok := sess.currentCtrl().(acpInboxController)
	if !ok {
		return false
	}
	snapshot := inbox.InboxSnapshot()
	if snapshot.Paused {
		return false
	}
	for _, item := range snapshot.Items {
		if item.State == sessioninbox.StateQueued && item.Origin.IsHost() {
			return true
		}
	}
	return false
}

// queueJobCompletion stores one undelivered completion in the session's replay
// queue and on disk. A repeated job id replaces its earlier entry, so a
// retried flush cannot show the same ending twice.
func (s *acpSession) queueJobCompletion(payload ReasonixJobCompleted) {
	if payload.JobID == "" {
		return
	}
	s.mu.Lock()
	if s.deleted {
		s.mu.Unlock()
		return
	}
	replaced := false
	for i := range s.pendingJobs {
		if s.pendingJobs[i].JobID == payload.JobID {
			s.pendingJobs[i] = payload
			replaced = true
			break
		}
	}
	if !replaced {
		s.pendingJobs = append(s.pendingJobs, payload)
	}
	if over := len(s.pendingJobs) - maxPendingJobCompletions; over > 0 {
		s.pendingJobs = append([]ReasonixJobCompleted(nil), s.pendingJobs[over:]...)
	}
	s.mu.Unlock()
	s.saveMetaIfPresent()
}

// flushJobCompletions delivers queued completions in order and stops at the
// first transport failure, leaving the rest durable. It reports how many were
// delivered, which is all a resume turn needs to tell "nothing was waiting"
// from "the client is still gone".
func (s *acpSession) flushJobCompletions() int {
	if !s.sink.jobCompletionsSupported() {
		return 0
	}
	s.mu.Lock()
	pending := append([]ReasonixJobCompleted(nil), s.pendingJobs...)
	s.mu.Unlock()
	delivered := make([]string, 0, len(pending))
	for _, payload := range pending {
		if err := s.sink.sendJobCompleted(payload); err != nil {
			break
		}
		delivered = append(delivered, payload.JobID)
	}
	if len(delivered) > 0 {
		s.dropJobCompletions(delivered)
	}
	return len(delivered)
}

// dropJobCompletions forgets the completions just delivered, in one meta
// write: a burst of thirty jobs is one replay, not thirty sidecar rewrites.
func (s *acpSession) dropJobCompletions(jobIDs []string) {
	done := make(map[string]struct{}, len(jobIDs))
	for _, id := range jobIDs {
		done[id] = struct{}{}
	}
	s.mu.Lock()
	kept := s.pendingJobs[:0]
	changed := false
	for _, payload := range s.pendingJobs {
		if _, ok := done[payload.JobID]; ok {
			changed = true
			continue
		}
		kept = append(kept, payload)
	}
	s.pendingJobs = kept
	s.mu.Unlock()
	if changed {
		s.saveMetaIfPresent()
	}
}
