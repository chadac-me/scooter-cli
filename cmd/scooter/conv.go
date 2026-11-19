package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Turn states reported by wait, show and ls.
const (
	stateIdle      = "idle"
	stateRunning   = "running"
	stateNeedsUser = "needs_user"
	stateError     = "error"
)

type message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// pendingRequest is an approval or question awaiting the user.
type pendingRequest struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"` // "questions" or "approval"
	Message   string     `json:"message"`
	Questions []question `json:"questions,omitempty"`
}

// convState folds a conversation's events into its turn state.
type convState struct {
	tr          *translator
	messages    []message
	byID        map[string]int // message id to its index in messages
	running     bool
	queued      bool
	userPending bool // a user message with no run started since
	runs        int  // RUN_STARTED events seen
	pending     map[string]interrupt
	order       []string // pending ids, oldest first
	lastError   string
}

func newConvState() *convState {
	return &convState{tr: newTranslator(), byID: map[string]int{}, pending: map[string]interrupt{}}
}

func (s *convState) apply(raw json.RawMessage) {
	var ev aguiEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "TEXT_MESSAGE_START":
		s.byID[ev.MessageID] = len(s.messages)
		s.messages = append(s.messages, message{Role: ev.Role})
		// agent-host logs a run's prompt after its RUN_STARTED.
		if ev.Role == "user" && !s.running {
			s.userPending = true
		}
	case "SYSTEM_MESSAGE":
		s.messages = append(s.messages, message{Role: "system", Text: "[" + ev.Source + "] " + ev.Text})
	}
	updates, sig := s.tr.apply(ev)
	if i, ok := s.byID[ev.MessageID]; ok && ev.Type != "RUN_ERROR" {
		for _, u := range updates {
			if c, ok := u["content"].(map[string]any); ok {
				s.messages[i].Text += fmt.Sprint(c["text"])
			}
		}
	}
	switch sig {
	case sigRunStarted:
		s.running, s.userPending, s.lastError = true, false, ""
		s.runs++
		// A run resumes only once its requests are answered.
		s.pending, s.order = map[string]interrupt{}, nil
	case sigRunEnded:
		s.running = false
	case sigRunError:
		s.running, s.lastError = false, ev.Message
	case sigInterrupt:
		s.running = false
		for _, in := range ev.Outcome.Interrupts {
			if _, dup := s.pending[in.ID]; !dup {
				s.order = append(s.order, in.ID)
			}
			s.pending[in.ID] = in
		}
	case sigResolved:
		delete(s.pending, ev.ToolCallID)
	case sigQueue:
		var items []json.RawMessage
		_ = json.Unmarshal(ev.Items, &items)
		s.queued = len(items) > 0
	}
}

func (s *convState) state() string {
	switch {
	case len(s.pendingRequests()) > 0:
		return stateNeedsUser
	case s.running || s.queued || s.userPending:
		return stateRunning
	case s.lastError != "":
		return stateError
	}
	return stateIdle
}

func (s *convState) pendingRequests() []pendingRequest {
	out := []pendingRequest{}
	for _, id := range s.order {
		in, ok := s.pending[id]
		if !ok {
			continue
		}
		p := pendingRequest{ID: in.ID, Kind: "approval", Message: in.Message}
		if qs := questionsOf(in.Metadata.Data); in.Metadata.Kind == kindQuestions && len(qs) > 0 {
			p.Kind, p.Questions = "questions", qs
		} else if title, body := describe(in); body != "" {
			p.Message = title + "\n" + body
		}
		out = append(out, p)
	}
	return out
}

// lastAgentMessage is the newest non-empty assistant text.
func (s *convState) lastAgentMessage() string {
	for i := len(s.messages) - 1; i >= 0; i-- {
		if m := s.messages[i]; m.Role == "assistant" && strings.TrimSpace(m.Text) != "" {
			return strings.TrimSpace(m.Text)
		}
	}
	return ""
}

// follower keeps a convState current from history and stream.
type follower struct {
	c       *client
	id      string
	st      *convState
	held    string
	applied int
	synced  bool
}

func newFollower(c *client, id string) *follower {
	return &follower{c: c, id: id, st: newConvState()}
}

func convPath(id, suffix string) string { return "/conversations/" + url.PathEscape(id) + suffix }

// load applies history events past those already applied.
func (f *follower) load(ctx context.Context) error {
	var h history
	if err := f.c.call(ctx, "GET", convPath(f.id, "/history"), nil, &h); err != nil {
		return err
	}
	for _, raw := range h.Events[min(f.applied, len(h.Events)):] {
		f.st.apply(raw)
	}
	f.applied, f.held, f.synced = len(h.Events), h.Checksum, false
	return nil
}

// streamItem is a frame, a connect, or a fatal error.
type streamItem struct {
	data      []byte
	connected bool
	err       error
}

// stream relays integrity frames, reconnecting with backoff.
func (f *follower) stream(ctx context.Context, items chan<- streamItem) {
	send := func(it streamItem) {
		select {
		case items <- it:
		case <-ctx.Done():
		}
	}
	backoff := time.Second
	for ctx.Err() == nil {
		err := f.c.stream(ctx, convPath(f.id, "/events.integrity"), func() {
			backoff = time.Second
			send(streamItem{connected: true})
		}, func(data []byte) { send(streamItem{data: append([]byte{}, data...)}) })
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errAuthWall) {
			send(streamItem{err: err})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// onFrame applies a chained frame; a gap reloads history.
func (f *follower) onFrame(ctx context.Context, data []byte) error {
	var fr frame
	if json.Unmarshal(data, &fr) != nil || fr.Kind != "event" || fr.Checksum == "" {
		return nil
	}
	switch {
	case fr.PrevChecksum == f.held:
		f.st.apply(fr.Event)
		f.held, f.synced = fr.Checksum, true
		f.applied++
		return nil
	case !f.synced:
		return nil // already in the history fetched at connect
	}
	return f.load(ctx)
}

type waitResult struct {
	ID          string           `json:"id"`
	State       string           `json:"state"`
	LastMessage string           `json:"last_message,omitempty"`
	Error       string           `json:"error,omitempty"`
	Pending     []pendingRequest `json:"pending"`
	URL         string           `json:"url"`
}

// waitTurn blocks until the turn ends, or a run past afterRuns.
func waitTurn(ctx context.Context, c *client, id string, timeout, settle time.Duration, afterRuns int) (waitResult, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	f := newFollower(c, id)
	if err := f.load(ctx); err != nil {
		return waitResult{}, err
	}
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	items := make(chan streamItem, 256)
	go f.stream(streamCtx, items)

	// Give a just-sent message time to start its run.
	var settled <-chan time.Time
	if afterRuns < 0 {
		settled = time.After(settle)
	}
	done := func() bool {
		st := f.st.state()
		if st == stateRunning {
			return false
		}
		if afterRuns >= 0 {
			return f.st.runs > afterRuns || st == stateNeedsUser
		}
		return settled == nil
	}
	for !done() {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return f.result(c), nil // the caller sees state "running"
			}
			return waitResult{}, ctx.Err()
		case <-settled:
			settled = nil
		case it := <-items:
			err := it.err
			switch {
			case it.connected:
				// Catch up on anything appended before the stream went live.
				err = f.load(ctx)
			case it.data != nil:
				err = f.onFrame(ctx, it.data)
			}
			if err != nil && ctx.Err() == nil {
				return waitResult{}, err
			}
		}
	}
	return f.result(c), nil
}

func (f *follower) result(c *client) waitResult {
	return waitResult{
		ID: f.id, State: f.st.state(), LastMessage: f.st.lastAgentMessage(),
		Error: f.st.lastError, Pending: f.st.pendingRequests(), URL: chatURL(c, f.id),
	}
}

func chatURL(c *client, id string) string { return c.ui + "/chat/" + url.PathEscape(id) }

// commandContext is cancelled by SIGINT or SIGTERM.
func commandContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// fatal exits 3 when a login is needed, else 1.
func fatal(err error) {
	log.Print(err)
	if errors.Is(err, errAuthWall) {
		os.Exit(3)
	}
	os.Exit(1)
}

// openServer builds a client using the stored login.
func openServer(flagValue string) *client {
	cfg, err := loadConfig()
	if err != nil {
		fatal(err)
	}
	name, s, err := cfg.resolve(flagValue)
	if err != nil {
		fatal(err)
	}
	c := newClient(s.base(), loginHeaders(name, s))
	c.ui = s.URL
	return c
}

// promptArg joins the arguments, or reads stdin for "-".
func promptArg(args []string) (string, error) {
	if len(args) == 1 && args[0] == "-" {
		b, err := io.ReadAll(os.Stdin)
		return strings.TrimSpace(string(b)), err
	}
	text := strings.TrimSpace(strings.Join(args, " "))
	if text == "" {
		return "", errors.New("empty prompt")
	}
	return text, nil
}

// runsSoFar counts runs, so a wait can expect the next.
func runsSoFar(ctx context.Context, c *client, id string) (int, error) {
	f := newFollower(c, id)
	err := f.load(ctx)
	return f.st.runs, err
}

// sendMessage posts text and optionally waits for its turn.
func sendMessage(ctx context.Context, c *client, id, text string, wait bool, timeout time.Duration) (any, error) {
	runs := 0
	if wait {
		var err error
		if runs, err = runsSoFar(ctx, c, id); err != nil {
			return nil, err
		}
	}
	if err := c.call(ctx, "POST", convPath(id, "/messages"), map[string]string{"text": text}, nil); err != nil {
		return nil, err
	}
	if !wait {
		return map[string]any{"id": id, "url": chatURL(c, id)}, nil
	}
	return waitTurn(ctx, c, id, timeout, 0, runs)
}

// listRow is the slice of GET /conversations an agent needs.
type listRow struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Status          string  `json:"status"`
	ParentID        *string `json:"parentId"`
	Kind            *string `json:"kind"`
	LastActivityAt  int64   `json:"lastActivityAt"`
	RunStartedAt    *int64  `json:"runStartedAt"`
	AwaitingInputAt *int64  `json:"awaitingInputAt"`
}

type listEntry struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Sandbox      string `json:"sandbox"`
	State        string `json:"state"`
	LastActivity string `json:"last_activity"`
	URL          string `json:"url"`
}

func listConversations(ctx context.Context, c *client, all, active bool, limit int) ([]listEntry, error) {
	scope := "mine"
	if all {
		scope = "all"
	}
	var rows []listRow
	if err := c.call(ctx, "GET", "/conversations?scope="+scope, nil, &rows); err != nil {
		return nil, err
	}
	out := []listEntry{}
	for _, r := range rows {
		if r.ParentID != nil || r.Kind != nil {
			continue // subagents and sessions belong to their parent
		}
		state := stateIdle
		switch {
		case r.AwaitingInputAt != nil:
			state = stateNeedsUser
		case r.RunStartedAt != nil:
			state = stateRunning
		}
		if active && state == stateIdle {
			continue
		}
		out = append(out, listEntry{
			ID: r.ID, Title: r.Title, Sandbox: r.Status, State: state,
			LastActivity: time.UnixMilli(r.LastActivityAt).UTC().Format(time.RFC3339), URL: chatURL(c, r.ID),
		})
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

type showResult struct {
	ID       string           `json:"id"`
	Title    string           `json:"title"`
	Sandbox  string           `json:"sandbox"`
	State    string           `json:"state"`
	Error    string           `json:"error,omitempty"`
	Pending  []pendingRequest `json:"pending"`
	Messages []message        `json:"messages"`
	URL      string           `json:"url"`
}

// maxShownText bounds each message so show stays cheap to read.
const maxShownText = 4000

func showConversation(ctx context.Context, c *client, id string, last int) (showResult, error) {
	var conv struct {
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	if err := c.call(ctx, "GET", convPath(id, ""), nil, &conv); err != nil {
		return showResult{}, err
	}
	f := newFollower(c, id)
	if err := f.load(ctx); err != nil {
		return showResult{}, err
	}
	msgs := []message{}
	for _, m := range f.st.messages {
		if m.Text = strings.TrimSpace(m.Text); m.Text == "" {
			continue
		}
		if len(m.Text) > maxShownText {
			m.Text = m.Text[:maxShownText] + "\n[... truncated]"
		}
		msgs = append(msgs, m)
	}
	if last > 0 && len(msgs) > last {
		msgs = msgs[len(msgs)-last:]
	}
	return showResult{
		ID: id, Title: conv.Title, Sandbox: conv.Status, State: f.st.state(), Error: f.st.lastError,
		Pending: f.st.pendingRequests(), Messages: msgs, URL: chatURL(c, id),
	}, nil
}
