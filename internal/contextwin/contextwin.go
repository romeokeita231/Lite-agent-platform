package contextwin

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	defaultRecent      = 8
	defaultMaxKeep     = 16
	summaryResultLimit = 40
	stepTokenOverhead  = 8
)

type Step struct {
	StepIndex int             `json:"step_index"`
	StepType  string          `json:"step_type"`
	Result    json.RawMessage `json:"result"`
}

type Config struct {
	Recent  int
	MaxKeep int
}

type View struct {
	Recent       []Step
	Summary      string
	Dropped      []int
	Tokens       int
	DroppedSteps int
}

func Build(steps []Step, keep []int, cfg Config) View {
	if cfg.Recent <= 0 {
		cfg.Recent = defaultRecent
	}
	if cfg.MaxKeep <= 0 {
		cfg.MaxKeep = defaultMaxKeep
	}
	ordered := make([]Step, len(steps))
	copy(ordered, steps)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].StepIndex < ordered[j].StepIndex })

	window := cfg.Recent
	if window > len(ordered) {
		window = len(ordered)
	}
	windowStart := len(ordered) - window
	retained := make(map[int]bool, window+cfg.MaxKeep)
	for _, step := range ordered[windowStart:] {
		retained[step.StepIndex] = true
	}

	exists := make(map[int]bool, len(ordered))
	for _, step := range ordered {
		exists[step.StepIndex] = true
	}
	seen := make(map[int]bool, len(keep))
	candidates := make([]int, 0, len(keep))
	for _, index := range keep {
		if seen[index] || !exists[index] || retained[index] {
			seen[index] = true
			continue
		}
		seen[index] = true
		candidates = append(candidates, index)
	}
	sort.Ints(candidates)
	if len(candidates) > cfg.MaxKeep {
		candidates = candidates[:cfg.MaxKeep]
	}
	for _, index := range candidates {
		retained[index] = true
	}

	recent := make([]Step, 0, len(retained))
	dropped := make([]int, 0)
	lines := make([]string, 0, len(ordered)-len(retained))
	for _, step := range ordered {
		if retained[step.StepIndex] {
			recent = append(recent, step)
			continue
		}
		dropped = append(dropped, step.StepIndex)
		lines = append(lines, summaryLine(step))
	}

	summary := strings.Join(lines, "\n")
	tokens := estimateTokens(recent, summary)
	return View{
		Recent:       recent,
		Summary:      summary,
		Dropped:      dropped,
		Tokens:       tokens,
		DroppedSteps: len(dropped),
	}
}

func KeepOf(result json.RawMessage) []int {
	if len(result) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(result, &object); err != nil {
		return nil
	}
	raw, ok := object["keep"]
	if !ok {
		return nil
	}
	var keep []int
	if err := json.Unmarshal(raw, &keep); err != nil {
		return nil
	}
	if len(keep) == 0 {
		return nil
	}
	return keep
}

func summaryLine(step Step) string {
	short := shortenResult(step.Result)
	line := strconv.Itoa(step.StepIndex) + " " + step.StepType
	if short != "" {
		line += " " + short
	}
	return line
}

func estimateTokens(recent []Step, summary string) int {
	encoded, err := json.Marshal(recent)
	if err != nil {
		encoded = []byte("[]")
	}
	runes := utf8.RuneCount(encoded) + utf8.RuneCountInString(summary)
	return (runes+3)/4 + stepTokenOverhead*len(recent)
}

var preferredKeys = []string{"output", "content", "tool", "result", "route", "model", "prompt", "input"}

func shortenResult(result json.RawMessage) string {
	if len(result) == 0 {
		return ""
	}
	decoder := json.NewDecoder(bytes.NewReader(result))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return truncate(strings.TrimSpace(string(result)))
	}
	return truncate(shortValue(value))
}

func shortValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return typed
	case map[string]any:
		for _, key := range preferredKeys {
			if nested, ok := typed[key]; ok {
				return shortValue(nested)
			}
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			return "{}"
		}
		return shortValue(typed[keys[0]])
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

func truncate(value string) string {
	runes := []rune(value)
	if len(runes) <= summaryResultLimit {
		return value
	}
	return string(runes[:summaryResultLimit-3]) + "..."
}
