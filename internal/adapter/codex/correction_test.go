package codex

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type functionRunner func(context.Context, Invocation) Process

func (f functionRunner) Run(ctx context.Context, i Invocation) Process { return f(ctx, i) }

func message(text string) string {
	b, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"id": "message-1", "type": "agent_message", "text": text}})
	return string(b) + "\n"
}

const streamPrefix = "{\"type\":\"thread.started\",\"thread_id\":\"11111111-1111-4111-8111-111111111111\"}\n{\"type\":\"turn.started\"}\n"
const streamEnd = "{\"type\":\"turn.completed\"}\n"

func completedProcess() Process {
	return Process{ExitCode: 0, Stdout: []byte(streamPrefix + message(`{"status":"completed","summary":"fixture"}`) + streamEnd)}
}

func TestCorrectionExclusiveDispatch(t *testing.T) {
	r, _ := request(t)
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	runner := functionRunner(func(context.Context, Invocation) Process {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return completedProcess()
	})
	results := make(chan Result, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- NewFake(runner).Execute(context.Background(), r, time.Now)
		}()
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first dispatch missing")
	}
	for range 7 {
		select {
		case got := <-results:
			if got.Outcome != "EXECUTION_ALREADY_RESERVED" || got.PID != 0 {
				t.Fatalf("duplicate not denied: %+v", got)
			}
		case <-time.After(time.Second):
			close(release)
			t.Fatal("duplicate entered runner")
		}
	}
	close(release)
	group.Wait()
	if got := <-results; got.Outcome != "COMPLETED_UNVERIFIED" || calls.Load() != 1 {
		t.Fatalf("first dispatch: %+v calls=%d", got, calls.Load())
	}
	for _, kind := range []string{"same-grant", "new-run-same-claim", "same-run-new-claim"} {
		t.Run(kind, func(t *testing.T) {
			copy := r
			grant := *r.Grant
			copy.Grant = &grant
			if kind == "new-run-same-claim" {
				copy.RunID, grant.RunID = "run-2", "run-2"
			}
			if kind == "same-run-new-claim" {
				grant.ClaimNonce = "claim-2"
			}
			got := NewFake(runner).Execute(context.Background(), copy, time.Now)
			if got.Outcome != "EXECUTION_ALREADY_RESERVED" || calls.Load() != 1 {
				t.Fatalf("replay: %+v calls=%d", got, calls.Load())
			}
		})
	}
	other, _ := request(t)
	if got := NewFake(functionRunner(func(context.Context, Invocation) Process { return completedProcess() })).Execute(context.Background(), other, time.Now); got.Outcome != "COMPLETED_UNVERIFIED" {
		t.Fatalf("independent scope denied: %+v", got)
	}
	t.Log("first dispatch1, concurrent duplicates7 denied, replay3 denied, independent scope admitted")
}

func TestCorrectionLeaseDeadline(t *testing.T) {
	for _, ignoreCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "cooperative", true: "late-return"}[ignoreCancel], func(t *testing.T) {
			r, _ := request(t)
			r.Timeout = time.Second
			r.Grant.LeaseUntil = time.Now().Add(60 * time.Millisecond)
			r.Store.(*fixtureStore).task.LeaseUntil = r.Grant.LeaseUntil
			calls := 0
			runner := functionRunner(func(ctx context.Context, _ Invocation) Process {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || deadline.After(r.Grant.LeaseUntil) {
					t.Fatal("execution deadline exceeds lease")
				}
				if ignoreCancel {
					time.Sleep(90 * time.Millisecond)
				} else {
					<-ctx.Done()
				}
				return completedProcess()
			})
			got := NewFake(runner).Execute(context.Background(), r, time.Now)
			if got.Outcome != "LEASE_EXPIRED" || calls != 1 || time.Now().Before(r.Grant.LeaseUntil) {
				t.Fatalf("expired lease success: %+v calls=%d", got, calls)
			}
		})
	}
}

func TestCorrectionEventAndFinalBoundary(t *testing.T) {
	good := message(`{"status":"completed","summary":"fixture"}`)
	blocked := message(`{"status":"blocked","summary":"fixture"}`)
	for _, tc := range []struct{ name, body, outcome string }{
		{"duplicate-type", good + `{"type":"error","message":"denied","type":"turn.completed"}`, "MALFORMED_OUTPUT"},
		{"escaped-type", good + `{"type":"error","\u0074ype":"turn.completed"}`, "MALFORMED_OUTPUT"},
		{"type-alias", good + `{"type":"error","Type":"turn.completed"}`, "MALFORMED_OUTPUT"},
		{"alias-only", `{"Type":"item.completed","item":{"type":"agent_message","text":"ignored"}}`, "MALFORMED_OUTPUT"},
		{"duplicate-item-type", `{"type":"item.completed","item":{"type":"file_change","type":"agent_message","text":"ignored"}}`, "MALFORMED_OUTPUT"},
		{"duplicate-status", good + `{"type":"item.completed","item":{"type":"command_execution","status":"failed","status":"completed"}}`, "MALFORMED_OUTPUT"},
		{"duplicate-identity", good + `{"type":"item.completed","item":{"id":"one","id":"two","type":"reasoning"}}`, "MALFORMED_OUTPUT"},
		{"identity-alias", good + `{"type":"item.completed","item":{"id":"one","Id":"two","type":"reasoning"}}`, "MALFORMED_OUTPUT"},
		{"unsupported-phase", `{"type":"item.completed","item":{"type":"agent_message","phase":"commentary","text":"progress"}}`, "UNSUPPORTED_OUTPUT"},
		{"nested-extension", good + `{"type":"turn.completed","usage":{"extra":[{"key":1,"key":2}]}}`, "MALFORMED_OUTPUT"},
		{"conflicting-finals", blocked + good + streamEnd, "UNSUPPORTED_OUTPUT"},
		{"identical-finals", good + good + streamEnd, "UNSUPPORTED_OUTPUT"},
		{"malformed-final-before-good", message(`{"status":"blocked","status":"completed","summary":"ambiguous"}`) + good + streamEnd, "MALFORMED_OUTPUT"},
		{"final-alias", message(`{"Status":"completed","summary":"alias"}`) + streamEnd, "MALFORMED_OUTPUT"},
		{"progress-after-final", good + message("more progress") + streamEnd, "MALFORMED_OUTPUT"},
		{"prose-progress", message("checking source") + good + streamEnd, "COMPLETED_UNVERIFIED"},
		{"json-progress", message(`{"progress":"checking"}`) + good + streamEnd, "COMPLETED_UNVERIFIED"},
		{"valid-tool-progress", `{"type":"item.started","item":{"id":"command-1","type":"command_execution","status":"in_progress"}}` + "\n" + `{"type":"item.completed","item":{"id":"command-1","type":"command_execution","status":"completed","exit_code":0}}` + "\n" + good + streamEnd, "COMPLETED_UNVERIFIED"},
		{"blocked-final", message("checking") + blocked + streamEnd, "SOFT_DENIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := parse([]byte(streamPrefix + strings.TrimRight(tc.body, "\n") + "\n"))
			if got != tc.outcome || got == "ACCEPTED" || got == "DONE" {
				t.Fatalf("outcome=%s want=%s", got, tc.outcome)
			}
		})
	}
	if !uniqueJSON([]byte(`{"text":"literal } [","number":1e99}`)) || uniqueJSON([]byte(`{} {}`)) || uniqueJSON([]byte(strings.Repeat("[", 3))) {
		t.Fatal("JSON validator boundary")
	}
}

func TestCorrectionCompletionLeaseRecheck(t *testing.T) {
	r, now := request(t)
	ticks := 0
	clock := func() time.Time {
		ticks++
		if ticks >= 5 {
			return r.Grant.LeaseUntil
		}
		return now
	}
	got := NewFake(functionRunner(func(context.Context, Invocation) Process { return completedProcess() })).Execute(context.Background(), r, clock)
	if got.Outcome != "LEASE_EXPIRED" {
		t.Fatalf("completion escaped lease recheck: %+v", got)
	}
}

func TestCorrectionLeaseStopsFixtureProcess(t *testing.T) {
	r, _ := request(t)
	r.Grant.LeaseUntil = time.Now().Add(150 * time.Millisecond)
	r.Store.(*fixtureStore).task.LeaseUntil = r.Grant.LeaseUntil
	runner := &fixtureRunner{t: t, name: "timeout"}
	start := time.Now()
	got := NewFake(runner).Execute(context.Background(), r, time.Now)
	if got.Outcome != "LEASE_EXPIRED" || got.PID <= 0 || runner.calls != 1 || time.Since(start) > time.Second {
		t.Fatalf("lease process stop: %+v calls=%d", got, runner.calls)
	}
}
