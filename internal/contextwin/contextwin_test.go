package contextwin_test

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/romeokeita231/Lite-agent-platform/internal/contextwin"
)

func tenSteps() []contextwin.Step {
	steps := make([]contextwin.Step, 0, 10)
	for index := 0; index < 10; index++ {
		steps = append(steps, contextwin.Step{
			StepIndex: index,
			StepType:  "tool_call",
			Result:    json.RawMessage(`{"tool":"lookup","result":{"value":` + string(rune('0'+index)) + `}}`),
		})
	}
	return steps
}

func TestBuildKeepsLastRecentAndSummarizesTheRest(t *testing.T) {
	steps := tenSteps()
	view := contextwin.Build(steps, nil, contextwin.Config{Recent: 4, MaxKeep: 16})

	if len(view.Recent) != 4 {
		t.Fatalf("recent length = %d, want 4", len(view.Recent))
	}
	for offset, step := range view.Recent {
		want := 6 + offset
		if step.StepIndex != want {
			t.Fatalf("recent[%d] step_index = %d, want %d", offset, step.StepIndex, want)
		}
		if string(step.Result) != string(steps[want].Result) || step.StepType != steps[want].StepType {
			t.Fatalf("recent[%d] is not the complete checkpoint: %+v", offset, step)
		}
	}

	lines := splitLines(view.Summary)
	if len(lines) != 6 {
		t.Fatalf("summary lines = %d, want 6 (%q)", len(lines), view.Summary)
	}
	for index, line := range lines {
		if prefix := strconv.Itoa(index) + " tool_call"; !strings.HasPrefix(line, prefix) {
			t.Fatalf("summary line %d = %q, want prefix %q", index, line, prefix)
		}
	}
	if lines[0] != "0 tool_call lookup" {
		t.Fatalf("summary line 0 = %q, want %q", lines[0], "0 tool_call lookup")
	}

	wantDropped := []int{0, 1, 2, 3, 4, 5}
	if !reflect.DeepEqual(view.Dropped, wantDropped) {
		t.Fatalf("dropped = %v, want %v", view.Dropped, wantDropped)
	}
	if view.DroppedSteps != 6 {
		t.Fatalf("dropped steps = %d, want 6", view.DroppedSteps)
	}
	if view.Tokens <= 0 {
		t.Fatalf("tokens = %d, want positive", view.Tokens)
	}
}

func TestBuildAcceptsKeepWithinLimitAndDropsTheRest(t *testing.T) {
	steps := tenSteps()
	view := contextwin.Build(steps, []int{7, 1, 7}, contextwin.Config{Recent: 4, MaxKeep: 1})

	wantRecent := []int{1, 6, 7, 8, 9}
	if len(view.Recent) != len(wantRecent) {
		t.Fatalf("recent = %+v", view.Recent)
	}
	for offset, step := range view.Recent {
		if step.StepIndex != wantRecent[offset] {
			t.Fatalf("recent[%d] step_index = %d, want %d", offset, step.StepIndex, wantRecent[offset])
		}
	}
	wantDropped := []int{0, 2, 3, 4, 5}
	if !reflect.DeepEqual(view.Dropped, wantDropped) {
		t.Fatalf("dropped = %v, want %v", view.Dropped, wantDropped)
	}
	if view.DroppedSteps != len(wantDropped) {
		t.Fatalf("dropped steps = %d, want %d", view.DroppedSteps, len(wantDropped))
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	steps := tenSteps()
	keep := []int{1, 7}
	cfg := contextwin.Config{Recent: 4, MaxKeep: 2}

	first, err := json.Marshal(contextwin.Build(steps, keep, cfg))
	if err != nil {
		t.Fatalf("marshal first view: %v", err)
	}
	second, err := json.Marshal(contextwin.Build(steps, keep, cfg))
	if err != nil {
		t.Fatalf("marshal second view: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("views differ:\n%s\n%s", first, second)
	}

	rebuilt := make([]contextwin.Step, 0, len(steps))
	for _, step := range steps {
		rebuilt = append(rebuilt, contextwin.Step{
			StepIndex: step.StepIndex,
			StepType:  step.StepType,
			Result:    json.RawMessage(append([]byte(nil), step.Result...)),
		})
	}
	third, err := json.Marshal(contextwin.Build(rebuilt, []int{7, 1}, cfg))
	if err != nil {
		t.Fatalf("marshal rebuilt view: %v", err)
	}
	if string(first) != string(third) {
		t.Fatalf("rebuilt ledger views differ:\n%s\n%s", first, third)
	}
}

func TestBuildEmptyLedgerHasEmptyLists(t *testing.T) {
	view := contextwin.Build(nil, nil, contextwin.Config{})

	encoded, err := json.Marshal(struct {
		Recent  []contextwin.Step
		Summary string
		Dropped []int
	}{view.Recent, view.Summary, view.Dropped})
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	if string(encoded) != `{"Recent":[],"Summary":"","Dropped":[]}` {
		t.Fatalf("empty view = %s", encoded)
	}
}

func TestKeepOf(t *testing.T) {
	cases := []struct {
		name   string
		result string
		want   []int
	}{
		{"present", `{"output":{},"keep":[3,1]}`, []int{3, 1}},
		{"absent", `{"output":{}}`, nil},
		{"not_array", `{"keep":"nope"}`, nil},
		{"not_integers", `{"keep":["1"]}`, nil},
		{"fractional", `{"keep":[1.5]}`, nil},
		{"malformed", `{"keep":`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contextwin.KeepOf(json.RawMessage(tc.result)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("keep = %v, want %v", got, tc.want)
			}
		})
	}
}

func splitLines(summary string) []string {
	if summary == "" {
		return nil
	}
	var lines []string
	current := ""
	for _, r := range summary {
		if r == '\n' {
			lines = append(lines, current)
			current = ""
			continue
		}
		current += string(r)
	}
	return append(lines, current)
}
