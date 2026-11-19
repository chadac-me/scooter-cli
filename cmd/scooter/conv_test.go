package main

import (
	"context"
	"flag"
	"reflect"
	"strings"
	"testing"
	"time"
)

func reply(text string) []map[string]any {
	return []map[string]any{
		{"type": "TEXT_MESSAGE_START", "messageId": "a1", "role": "assistant"},
		{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a1", "delta": text},
		{"type": "TEXT_MESSAGE_END", "messageId": "a1"},
	}
}

func TestSendWaitReturnsTheReply(t *testing.T) {
	f, srv := newFake(t)
	// An earlier finished turn must not end the wait early.
	run(f, "earlier", reply("old answer")...)
	f.onMessage = func(f *fakeScooter, text string) {
		time.Sleep(50 * time.Millisecond)
		run(f, text, reply("done <!-- hidden -->here")...)
	}
	c := newClient(srv.URL, headerSource{})
	out, err := sendMessage(context.Background(), c, "c1", "go", true, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := out.(waitResult)
	if r.State != stateIdle || r.LastMessage != "done here" || r.URL != srv.URL+"/chat/c1" {
		t.Fatalf("got %+v", r)
	}
}

func TestWaitReportsPendingQuestions(t *testing.T) {
	f, srv := newFake(t)
	f.emit(map[string]any{"type": "RUN_STARTED"})
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.emit(map[string]any{"type": "RUN_FINISHED", "outcome": map[string]any{
			"type": "interrupt",
			"interrupts": []any{map[string]any{
				"id": "q1", "toolCallId": "q1", "message": "Which DB?",
				"metadata": map[string]any{
					"kind":    kindQuestions,
					"options": []any{map[string]any{"optionId": "answer", "name": "Answer", "kind": "allow_once"}},
					"data": map[string]any{"questions": []any{map[string]any{
						"question": "Which DB?", "header": "DB",
						"options": []any{map[string]any{"label": "Postgres"}, map[string]any{"label": "SQLite"}},
					}}},
				},
			}},
		}})
	}()
	r, err := waitTurn(context.Background(), newClient(srv.URL, headerSource{}), "c1", 5*time.Second, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != stateNeedsUser || len(r.Pending) != 1 || r.Pending[0].Kind != "questions" || r.Pending[0].Questions[0].Options[1].Label != "SQLite" {
		t.Fatalf("got %+v", r)
	}
}

func TestWaitOnIdleConversationSettles(t *testing.T) {
	f, srv := newFake(t)
	run(f, "hi", reply("hello")...)
	start := time.Now()
	r, err := waitTurn(context.Background(), newClient(srv.URL, headerSource{}), "c1", 5*time.Second, 100*time.Millisecond, -1)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != stateIdle || r.LastMessage != "hello" || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("got %+v after %s", r, time.Since(start))
	}
}

func TestWaitTimesOutAsRunning(t *testing.T) {
	f, srv := newFake(t)
	f.emit(map[string]any{"type": "RUN_STARTED"})
	r, err := waitTurn(context.Background(), newClient(srv.URL, headerSource{}), "c1", 200*time.Millisecond, 0, -1)
	if err != nil || r.State != stateRunning {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestStateFolding(t *testing.T) {
	cases := []struct {
		name   string
		events []string
		want   string
	}{
		{"user message not yet run", []string{`{"type":"TEXT_MESSAGE_START","messageId":"u","role":"user"}`}, stateRunning},
		{"finished run with its prompt", []string{
			`{"type":"RUN_STARTED"}`,
			`{"type":"TEXT_MESSAGE_START","messageId":"u","role":"user"}`,
			`{"type":"TEXT_MESSAGE_START","messageId":"a","role":"assistant"}`,
			`{"type":"SYSTEM_MESSAGE","messageId":"s","source":"hook","text":"keep going"}`,
			`{"type":"RUN_FINISHED"}`,
		}, stateIdle},
		{"errored run with its prompt", []string{
			`{"type":"RUN_STARTED"}`,
			`{"type":"TEXT_MESSAGE_START","messageId":"u","role":"user"}`,
			`{"type":"RUN_ERROR","message":"boom"}`,
		}, stateError},
		{"running run with its prompt", []string{
			`{"type":"RUN_STARTED"}`,
			`{"type":"TEXT_MESSAGE_START","messageId":"u","role":"user"}`,
		}, stateRunning},
		{"queue drained", []string{
			`{"type":"QUEUE_UPDATED","items":[{"text":"x"}]}`,
			`{"type":"QUEUE_UPDATED","items":[]}`,
			`{"type":"RUN_STARTED"}`,
			`{"type":"TEXT_MESSAGE_START","messageId":"u","role":"user"}`,
			`{"type":"RUN_FINISHED"}`,
		}, stateIdle},
		{"queued", []string{`{"type":"QUEUE_UPDATED","items":[{"text":"x"}]}`}, stateRunning},
		{"run error", []string{`{"type":"RUN_STARTED"}`, `{"type":"RUN_ERROR","message":"boom"}`}, stateError},
		{"answered approval", []string{
			`{"type":"RUN_STARTED"}`,
			`{"type":"RUN_FINISHED","outcome":{"type":"interrupt","interrupts":[{"id":"t1","message":"rm?"}]}}`,
			`{"type":"PERMISSION_RESOLVED","toolCallId":"t1"}`,
		}, stateIdle},
		{"resumed run", []string{
			`{"type":"RUN_FINISHED","outcome":{"type":"interrupt","interrupts":[{"id":"t1","message":"rm?"}]}}`,
			`{"type":"RUN_STARTED"}`,
		}, stateRunning},
	}
	for _, tc := range cases {
		s := newConvState()
		for _, e := range tc.events {
			s.apply([]byte(e))
		}
		if got := s.state(); got != tc.want {
			t.Errorf("%s: state %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestListSkipsSubagentsAndFilters(t *testing.T) {
	f, srv := newFake(t)
	started := int64(1)
	f.list = []map[string]any{
		{"id": "a", "title": "busy", "status": "running", "runStartedAt": started, "lastActivityAt": 0},
		{"id": "b", "title": "child", "status": "running", "parentId": "a", "lastActivityAt": 0},
		{"id": "c", "title": "quiet", "status": "suspended", "lastActivityAt": 0},
		{"id": "d", "title": "asks", "status": "running", "awaitingInputAt": started, "lastActivityAt": 0},
	}
	c := newClient(srv.URL, headerSource{})
	all, err := listConversations(context.Background(), c, false, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range all {
		got = append(got, e.ID+":"+e.State)
	}
	if want := []string{"a:running", "c:idle", "d:needs_user"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	active, _ := listConversations(context.Background(), c, false, true, 1)
	if len(active) != 1 || active[0].ID != "a" {
		t.Fatalf("active, limit 1: %+v", active)
	}
}

func TestShowTrimsToLastMessages(t *testing.T) {
	f, srv := newFake(t)
	run(f, "one", reply("first")...)
	run(f, "two", reply(strings.Repeat("x", maxShownText+10))...)
	r, err := showConversation(context.Background(), newClient(srv.URL, headerSource{}), "c1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 2 || r.Messages[0].Text != "two" || !strings.HasSuffix(r.Messages[1].Text, "[... truncated]") || r.State != stateIdle {
		t.Fatalf("got %+v", r)
	}
}

func TestParseArgsAllowsFlagsAnywhere(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	wait := fs.Bool("wait", false, "")
	rest := parseArgs(fs, []string{"c1", "--wait", "fix", "it", "--", "--not-a-flag"}, 2)
	if !*wait || !reflect.DeepEqual(rest, []string{"c1", "fix", "it", "--not-a-flag"}) {
		t.Fatalf("wait=%v rest=%q", *wait, rest)
	}
}
