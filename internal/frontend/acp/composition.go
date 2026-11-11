package acp

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"reasonix/internal/assembly/boot"
	"reasonix/internal/contract/agentcomposition"
	"reasonix/internal/contract/event"
	"reasonix/internal/ext/plugin"
	"reasonix/internal/session/control"
	"reasonix/internal/state/sessionstore"
	"reasonix/internal/tools/builtin"
)

type SessionParams struct {
	AgentComposition   string
	Composition        *agentcomposition.Binding
	Cwd                string
	MCPServers         []plugin.Spec
	AdditionalDirs     []string
	Sink               event.Sink
	Model              string
	EffortOverride     *string
	RuntimeProfile     string
	OnSessionRecovered func(control.SessionRecoveryInfo) error
	NewConversation    bool
	FileOverlay        builtin.FileOverlay
	Terminal           builtin.TerminalRunner
}

func (s *service) bindCompositionIO(sess *acpSession, ctrl *control.Controller) {
	sess.sink.bindApprove(ctrl.Approve)
	sess.sink.bindAnswer(ctrl.AnswerQuestion)
	s.bindCompositionSelector(sess)
	s.bindControllerIdle(sess)
}

func (s *service) newPersistedSession(ctx context.Context, p SessionParams, path string) (*control.Controller, error) {
	binding, err := compositionFromPath(path)
	if err != nil {
		return nil, err
	}
	p.Composition = binding
	ctrl, err := s.factory.NewSession(ctx, p)
	if err != nil && binding != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return adoptSessionPolicy(boot.BuildCompositionHistory(boot.Options{Composition: binding, WorkspaceRoot: p.Cwd, SessionDir: s.sessionDir(), Sink: p.Sink, Model: p.Model}, err).Controller, p.Sink), nil
	}
	return adoptSessionPolicy(ctrl, p.Sink), err
}

// adoptSessionPolicy gives an ACP controller the policy its session shares: a
// client that declared job completions learns about a finished job from its own
// notification and decides when the session works again, so the completion must
// not start a turn behind its back. The capability comes from the session's
// update sink, because rebuild paths already hold the service lock.
func adoptSessionPolicy(ctrl *control.Controller, sink event.Sink) *control.Controller {
	updates, _ := sink.(*updateSink)
	if ctrl != nil && updates != nil && updates.jobCompletionsSupported() {
		ctrl.SetExternalBackgroundResume(true)
	}
	return ctrl
}

func compositionFromPath(path string) (*agentcomposition.Binding, error) {
	if path == "" {
		return nil, nil
	}
	s, err := sessionstore.LoadSession(path)
	if err != nil {
		return nil, err
	}
	return s.Composition(), nil
}

func withCompositionConfig(state SessionConfigState, ctrl acpController) SessionConfigState {
	catalog, err := ctrl.AgentPresets()
	if err != nil {
		return state
	}
	current := "builtin:general"
	locked := control.CompositionStarted(ctrl) || ctrl.PendingPrompt() || ctrl.RuntimeStatus().BackgroundJobs > 0
	currentName := "Standard"
	var summary *agentcomposition.Summary
	if lightweight, ok := ctrl.(interface {
		CompositionSummary() *agentcomposition.Summary
	}); ok {
		summary = lightweight.CompositionSummary()
	} else {
		summary = ctrl.CompositionBinding().Summary()
	}
	if b := summary; b != nil {
		current = b.ID
		currentName = b.Name
		locked = locked || b.Started
	}
	options := []SessionConfigSelectOption{}
	for _, v := range catalog.Presets {
		options = append(options, SessionConfigSelectOption{Value: v.Definition.ID, Name: v.Definition.Name, Description: v.Definition.Description})
	}
	description := "Choose a composition before the first prompt. Models are configured independently."
	if locked {
		options = []SessionConfigSelectOption{{Value: current, Name: currentName}}
		description = "This conversation keeps its bound version. Create a new session to use another composition."
	}
	state.ConfigOptions = append(state.ConfigOptions, SessionConfigOption{ID: "agent_composition", Category: "agent_composition", Name: "Agent composition", Description: description, Type: "select", CurrentValue: current, Options: options})
	return state
}

func (s *service) bindCompositionSelector(sess *acpSession) {
	ctrl, ok := sess.currentCtrl().(*control.Controller)
	if !ok {
		return
	}
	ctrl.ConfigureCompositions(ctrl.PresetStore().Home, func(ctx context.Context, id string) error { return s.switchComposition(ctx, sess, ctrl, id) })
}

func (s *service) switchComposition(ctx context.Context, sess *acpSession, cur *control.Controller, id string) error {
	if !sess.stateChangeMu.TryLock() {
		return agentcomposition.ErrLocked
	}
	defer sess.stateChangeMu.Unlock()
	sess.mu.Lock()
	if sess.deleted || sess.running || sess.maintenanceDone != nil || sess.ctrl != cur {
		sess.mu.Unlock()
		return agentcomposition.ErrLocked
	}
	params := SessionParams{AgentComposition: id, Cwd: sess.cwd, MCPServers: clonePluginSpecs(sess.mcpServers), AdditionalDirs: slices.Clone(sess.additionalDirs), Sink: sess.sink, Model: sess.model, EffortOverride: cloneStringPtr(sess.effortOverride), RuntimeProfile: sess.runtimeProfile, OnSessionRecovered: s.sessionRecoveredHandler(sess.id)}
	done := make(chan struct{})
	sess.maintenanceDone = done
	sess.mu.Unlock()
	defer sess.finishMaintenance(done)
	s.bindClientIO(&params, sess.id)
	next, err := s.factory.NewSession(ctx, params)
	if err != nil {
		return err
	}
	next = adoptSessionPolicy(next, sess.sink)
	if err := boot.ApplyRuntimeMigration(next, cur, boot.CaptureRuntimeMigration(cur)); err != nil {
		next.ReleaseResources()
		return err
	}
	next.EnableInteractiveApproval()
	if err := s.prepareACPReplacementAuthority(sess, next, cur, cur.SessionPath(), "snapshot Agent composition"); err != nil {
		next.ReleaseResources()
		return err
	}
	sess.mu.Lock()
	if sess.deleted || sess.ctrl != cur {
		sess.mu.Unlock()
		next.ReleaseResources()
		return fmt.Errorf("%w: session changed", agentcomposition.ErrLocked)
	}
	sess.ctrl = next
	sess.mu.Unlock()
	sess.sink.bindApprove(next.Approve)
	sess.sink.bindAnswer(next.AnswerQuestion)
	s.bindCompositionSelector(sess)
	s.bindControllerIdle(sess)
	cur.ReleaseResources()
	s.sendAvailableCommands(sess)
	return nil
}
