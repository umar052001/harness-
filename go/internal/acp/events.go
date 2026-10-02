package acp

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// updatePayload is the inner object of a session/update notification.
//
// It is a named type rather than an inline anonymous struct so the helper
// functions below can take it as a parameter. An anonymous struct would have to
// be redeclared field-for-field at every call site, and getting one tag wrong
// makes the types silently incompatible.
type updatePayload struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Type          string          `json:"type"`
	Message       json.RawMessage `json:"message"`
	// Tool call fields, flattened because the protocol inlines them.
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	// RawInput and RawOutput are the tool's arguments and its result. They are
	// where the only useful description of a call lives: the title is the bare
	// tool name ("bash") on the first frame of a call, so a client that shows
	// titles alone says "bash" and hides the command entirely.
	RawInput  json.RawMessage `json:"rawInput"`
	RawOutput json.RawMessage `json:"rawOutput"`
	// Content, rawContent and locations are carried, not interpreted.
	Content    json.RawMessage `json:"content"`
	RawContent json.RawMessage `json:"rawContent"`
	Locations  json.RawMessage `json:"locations"`
}

// sessionUpdate is the payload of a session/update notification.
//
// Agents ship two dialects of this frame. opencode wraps everything in an
// "update" object keyed by "sessionUpdate"; spec-styled agents (and the
// reference mock) put "type" and the message at the params level. Fields are
// kept raw so both parse.
type sessionUpdate struct {
	SessionID string          `json:"sessionId"`
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message"`
	Update    updatePayload   `json:"update"`
}

// onSessionUpdate turns a notification into events and accumulates the answer.
//
// Accumulation lives here rather than in the UI because the caller of Prompt
// needs the agent's text as a value: a planner deciding what to do next cannot
// parse a stream of display events to find the answer.
func (c *Client) onSessionUpdate(raw json.RawMessage) {
	var su sessionUpdate
	if err := json.Unmarshal(raw, &su); err != nil {
		return
	}
	upd := su.Update
	base := Event{At: time.Now()}

	// opencode discriminates with "update.sessionUpdate"; spec-styled agents use
	// "type", either wrapped in update or at the params level.
	kind := upd.SessionUpdate
	if kind == "" {
		kind = upd.Type
	}
	if kind == "" {
		kind = su.Type
	}
	// The spec wraps the content in a params-level message; opencode puts a
	// block (or an array of blocks) in update.content.
	msg := upd.Message
	if msg == nil {
		msg = su.Message
	}

	switch kind {
	case "agent_message_chunk", "agent_thought_chunk":
		// Message text lives either inline (opencode) or nested in a spec-style
		// message whose content is an array of blocks, or in a bare array. The
		// helper resolves all three instead of guessing which agent shipped it.
		text := updateText(msg, upd.Content)
		if text == "" {
			return
		}
		if kind == "agent_thought_chunk" {
			base.Type = EventThought
		} else {
			base.Type = EventMessage
			// Only the answer is accumulated. A thought chunk reaching the
			// planner as if it were the agent's conclusion would be worse than
			// losing it, so reasoning is shown but never returned as text.
			c.coll.addText(text)
		}
		base.Text = text
		base.Raw = raw
		c.emit(base)

	case "tool_call", "tool_call_update":
		c.coll.addTool(upd.ToolCallID)
		base.Type = EventToolCall
		if kind == "tool_call_update" {
			base.Type = EventToolUpdate
		}
		base.ToolCallID = upd.ToolCallID
		base.ToolTitle = upd.Title
		base.ToolStatus = upd.Status
		base.ToolKind = upd.Kind
		base.Text = upd.Title
		base.Tool = c.toolDetail(upd)
		base.Raw = raw
		c.emit(base)

	case "plan":
		base.Type = EventPlan
		base.Raw = raw
		if upd.Title != "" {
			base.Text = upd.Title
		}
		c.emit(base)

	case "current_mode_update", "available_commands_update", "session_title":
		base.Type = EventAvailable
		base.Raw = raw
		c.emit(base)

	case "":
		// An update with no discriminator. Carried raw so it is visible rather
		// than dropped, which matters when an agent ships a new variant.
		base.Type = EventAvailable
		base.Raw = raw
		c.emit(base)
	}
}

// toolDetail pulls the interesting parts out of a tool call.
//
// The reason this exists: the protocol's title is the bare tool name on the
// first frame of a call ("bash") and only becomes the real command on a later
// update. A control plane that renders titles shows a column of "bash" and the
// user cannot tell what the agent is doing, which is the single most important
// thing to know while it is doing it.
//
// Nothing here is required for correctness, so every field is best-effort: a
// vendor that spells its fields differently loses detail, not the event.
func (c *Client) toolDetail(upd updatePayload) EventTool {
	out, cut := toolOutput(upd)
	d := EventTool{
		Name:      upd.Title,
		Status:    upd.Status,
		Kind:      upd.Kind,
		Output:    out,
		Truncated: cut,
		Paths:     toolLocations(upd.Locations),
	}

	// The first frame of a call names the tool; later frames retitle it with the
	// argument. Remembering the first sighting is what lets a display say "bash"
	// and show the command rather than losing one or the other.
	var in struct {
		Command     string `json:"command"`
		Cmd         string `json:"cmd"`
		Path        string `json:"path"`
		File        string `json:"file"`
		FilePath    string `json:"filePath"`
		Notebook    string `json:"notebook_path"`
		Pattern     string `json:"pattern"`
		Query       string `json:"query"`
		URL         string `json:"url"`
		Prompt      string `json:"prompt"`
		Content     string `json:"content"`
		Description string `json:"description"`
	}
	if len(upd.RawInput) > 0 {
		_ = json.Unmarshal(upd.RawInput, &in)
	}

	d.Command = firstNonEmpty(in.Command, in.Cmd)
	switch {
	case d.Command != "":
	case in.Path != "":
		d.Command = in.Path
	case in.File != "":
		d.Command = in.File
	case in.FilePath != "":
		d.Command = in.FilePath
	case in.Pattern != "":
		d.Command = in.Pattern
	case in.URL != "":
		d.Command = in.URL
	case in.Query != "":
		d.Command = in.Query
	case in.Prompt != "":
		d.Command = truncate(in.Prompt, 160)
	case in.Content != "":
		d.Command = truncate(in.Content, 160)
	case in.Description != "":
		d.Command = truncate(in.Description, 160)
	case len(in.Notebook) > 0:
		d.Command = in.Notebook
	default:
		// No recognisable argument field. A title that is not a bare identifier
		// is still the best description available, which is exactly the case the
		// second frame of a bash call produces.
		d.Command = titleAsCommand(upd.Title)
	}
	if d.Command == "" {
		d.Command = titleAsCommand(upd.Title)
	}

	if d.Name == "" {
		d.Name = upd.Kind
	}

	// A call's identity is spread across its frames: the name and the touched
	// paths arrive on the first one and are omitted from later updates, and the
	// final "completed" frame routinely carries neither. Keeping the first
	// sighting is what stops a finished tool call from losing what it did.
	if upd.ToolCallID != "" {
		if prev, seen := c.tools[upd.ToolCallID]; seen {
			if d.Name == "" || !isBareIdentifier(d.Name) {
				d.Name = prev.Name
			}
			if len(d.Paths) == 0 {
				d.Paths = prev.Paths
			}
		} else {
			c.tools[upd.ToolCallID] = EventTool{Name: d.Name, Paths: d.Paths}
		}
	}
	return d
}

// toolOutput extracts a tool's result text.
//
// The two carriers are the content array and rawOutput, and agents use either or
// both. Only the head is kept: it is the bounded, predictable choice, and the tail
// of a long shell log is where a stack trace goes, which the user can reach with
// the raw event log.
func toolOutput(upd updatePayload) (string, bool) {
	const limit = 1200
	if len(upd.RawOutput) > 0 {
		var ro struct {
			Output string `json:"output"`
			Text   string `json:"text"`
			Result string `json:"result"`
		}
		if json.Unmarshal(upd.RawOutput, &ro) == nil {
			if s := firstNonEmpty(ro.Output, ro.Text, ro.Result); s != "" {
				return truncate(s, limit), len(s) > limit
			}
		}
	}
	if len(upd.Content) > 0 {
		var blocks []struct {
			Type    string `json:"type"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Text string `json:"text"`
		}
		if json.Unmarshal(upd.Content, &blocks) == nil {
			var parts []string
			for _, b := range blocks {
				if s := firstNonEmpty(b.Content.Text, b.Text); s != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				joined := strings.Join(parts, "\n")
				return truncate(joined, limit), len(joined) > limit
			}
		}
	}
	return "", false
}

func toolLocations(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var locs []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &locs); err != nil {
		return nil
	}
	var out []string
	for _, l := range locs {
		if l.Path != "" {
			out = append(out, l.Path)
		}
	}
	return out
}

// titleAsCommand returns the title when it reads as an argument rather than a
// tool name.
func titleAsCommand(title string) string {
	if title == "" || isBareIdentifier(title) {
		return ""
	}
	return truncate(title, 200)
}

// isBareIdentifier reports whether a title is just a tool name: no spaces, no
// slashes, no shell punctuation.
func isBareIdentifier(s string) bool {
	if s == "" {
		return false
	}
	return !strings.ContainsAny(s, " \t\n/\\|&;<>$`'\"(){}$")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// The spec wraps it in a message whose content is an array of blocks; opencode
// puts the block inline or sends an array directly. Trying one shape and
// falling back to the next means a frame from a new agent is read instead of
// silently dropped, which is what happened to the first spec-shaped frame.
func updateText(message, content json.RawMessage) string {
	if s := textIn(message); s != "" {
		return s
	}
	return textIn(content)
}

// textIn pulls the first non-empty "text" out of a block, an array of blocks,
// or a message wrapper containing either. It descends through nested maps and
// arrays rather than matching one exact frame shape, so a wrapper that adds a
// field is still readable.
func textIn(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return ""
	}
	var first func(any) string
	first = func(x any) string {
		switch t := x.(type) {
		case map[string]any:
			if s, ok := t["text"].(string); ok && s != "" {
				return s
			}
			for _, v := range t {
				if s := first(v); s != "" {
					return s
				}
			}
		case []any:
			for _, el := range t {
				if s := first(el); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return first(v)
}

// onPermission answers a tool permission request.
//
// The default is denial. This is the single most security-relevant decision in
// the whole client: an agent asking to run a command is the moment a policy
// should apply, and defaulting to allow would mean a manifest typo — or an agent
// compromise — silently obtains command execution. AutoApprove is opt-in and is
// only ever set by an operator who has decided the workspace is disposable.
func (c *Client) onPermission(m *message) {
	var req struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(m.Params, &req)

	c.emit(Event{
		Type:       EventPermission,
		Text:       fmt.Sprintf("%s %s", req.ToolCall.Kind, req.ToolCall.Title),
		ToolCallID: req.ToolCall.ToolCallID,
		ToolTitle:  req.ToolCall.Title,
		ToolKind:   req.ToolCall.Kind,
		Raw:        m.Params,
		At:         time.Now(),
	})

	choice := c.pickOption(req.Options, c.autoApprove)
	_ = c.write(&message{
		JSONRPC: "2.0",
		ID:      m.ID,
		Result:  mustJSON(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": choice}}),
	})
}

// pickOption chooses an option id, or declines when nothing is acceptable.
func (c *Client) pickOption(opts []struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}, autoApprove bool) string {
	if len(opts) == 0 {
		return ""
	}
	// ACP encodes allow as kind "allow_once"/"allow_always" and refusal as
	// "reject_once"/"reject_always". Preferring a refusal is why the default is
	// safe: if an agent offers only allow options, autoApprove still governs.
	var reject string
	for _, o := range opts {
		switch {
		case strings.Contains(o.Kind, "reject"), strings.Contains(o.Kind, "deny"):
			if reject == "" {
				reject = o.OptionID
			}
		}
	}
	if !autoApprove {
		return reject
	}
	for _, o := range opts {
		if strings.Contains(o.Kind, "allow") {
			return o.OptionID
		}
	}
	return reject
}
