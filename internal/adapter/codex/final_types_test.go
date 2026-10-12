package codex

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestFinalResponseStringTypes(t *testing.T) {
	for _, field := range []string{"status", "summary"} {
		for _, value := range []string{"null", "17", "[]", "{}", "true"} {
			t.Run(field+"-"+value, func(t *testing.T) {
				object := map[string]json.RawMessage{"status": json.RawMessage(`"completed"`), "summary": json.RawMessage(`"fixture"`)}
				object[field] = json.RawMessage(value)
				data, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				if status, valid := finalStatus(string(data)); valid {
					t.Fatalf("non-string accepted: status=%q value=%s", status, data)
				}
				output := []byte(streamPrefix + message(string(data)) + streamEnd)
				if got, _ := parse(output); got != "MALFORMED_OUTPUT" {
					t.Fatalf("non-string stream: %s", got)
				}
				r, _ := request(t)
				calls := 0
				runner := functionRunner(func(context.Context, Invocation) Process {
					calls++
					return Process{ExitCode: 0, Stdout: output}
				})
				got := NewFake(runner).Execute(context.Background(), r, time.Now)
				if got.Outcome != "MALFORMED_OUTPUT" || calls != 1 || got.PID != 0 {
					t.Fatalf("non-string fake result: %+v calls=%d", got, calls)
				}
			})
		}
	}
	for _, tc := range []struct{ text, status, outcome string }{
		{`{"status":"completed","summary":"fixture"}`, "completed", "COMPLETED_UNVERIFIED"},
		{`{"status":"blocked","summary":"denied"}`, "blocked", "SOFT_DENIED"},
		{`{"status":"completed","summary":""}`, "completed", "COMPLETED_UNVERIFIED"},
		{`{"summary":"escaped \"text\" 한글","status":"completed"}`, "completed", "COMPLETED_UNVERIFIED"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			if got, valid := finalStatus(tc.text); !valid || got != tc.status {
				t.Fatalf("valid string rejected: status=%q valid=%v", got, valid)
			}
			output := []byte(streamPrefix + message("checking") + message(tc.text) + streamEnd)
			if got, _ := parse(output); got != tc.outcome {
				t.Fatalf("valid string stream: %s", got)
			}
			r, _ := request(t)
			calls := 0
			got := NewFake(functionRunner(func(context.Context, Invocation) Process {
				calls++
				return Process{ExitCode: 0, Stdout: output}
			})).Execute(context.Background(), r, time.Now)
			if got.Outcome != tc.outcome || calls != 1 || got.PID != 0 {
				t.Fatalf("valid string fake result: %+v calls=%d", got, calls)
			}
		})
	}
}
