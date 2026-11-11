package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/contract/event"
)

func (s *updateSink) requestAsk(ctx context.Context, a event.Ask) {
	if s.answer == nil {
		return
	}
	// A server's form drawn in the permission prompt reads as the agent asking
	// leave, so it goes through elicitation/create when the client advertised the
	// mode and is declined otherwise.
	if a.Origin != nil {
		s.answer(a.ID, s.elicit(ctx, a))
		return
	}
	answers := make([]event.AskAnswer, 0, len(a.Questions))
	for _, q := range a.Questions {
		selected, ok := s.requestAskQuestion(ctx, a.ID, q)
		if !ok {
			s.answer(a.ID, nil)
			return
		}
		answers = append(answers, event.AskAnswer{QuestionID: q.ID, Selected: []string{selected}})
	}
	s.answer(a.ID, answers)
}

func (s *updateSink) requestAskQuestion(ctx context.Context, askID string, q event.AskQuestion) (string, bool) {
	title := strings.TrimSpace(q.Prompt)
	if title == "" {
		title = strings.TrimSpace(q.Header)
	}
	if title == "" {
		title = "Question"
	}
	content := []toolContent(nil)
	if q.Header != "" && q.Header != title {
		content = append(content, toolContent{Type: "content", Content: textBlock(q.Header)})
	}
	options := make([]PermissionOption, 0, len(q.Options)+1)
	labelsByID := make(map[string]string, len(q.Options))
	for i, opt := range q.Options {
		id := fmt.Sprintf("%s:%d", q.ID, i+1)
		name := strings.TrimSpace(opt.Label)
		if strings.TrimSpace(opt.Description) != "" {
			name += " - " + strings.TrimSpace(opt.Description)
		}
		options = append(options, PermissionOption{OptionID: id, Name: name, Kind: OptAllowOnce})
		labelsByID[id] = opt.Label
	}
	options = append(options, PermissionOption{OptionID: q.ID + ":cancel", Name: "Cancel", Kind: OptRejectOnce})

	rawInput, _ := json.Marshal(map[string]any{
		"id":       q.ID,
		"question": title,
		"options":  q.Options,
		"multi":    q.Multi,
	})
	params := PermissionRequestParams{
		SessionID: s.sessionID,
		ToolCall: PermissionToolCall{
			ToolCallID: "ask-" + askID + "-" + q.ID,
			Title:      title,
			Kind:       "other",
			Status:     "pending",
			Content:    content,
			RawInput:   rawInput,
		},
		Options: options,
	}

	raw, err := s.conn.Request(ctx, "session/request_permission", params)
	if err != nil {
		return "", false
	}
	var res PermissionRequestResult
	if json.Unmarshal(raw, &res) != nil || res.Outcome.Outcome != "selected" {
		return "", false
	}
	label, ok := labelsByID[res.Outcome.OptionID]
	return label, ok
}
