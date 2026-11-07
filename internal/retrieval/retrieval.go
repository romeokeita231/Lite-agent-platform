package retrieval

import (
	"context"
	"sort"
	"strings"
)

type Entry struct {
	StepIndex int    `json:"step_index"`
	StepType  string `json:"step_type"`
	Text      string `json:"text"`
}

type Retriever interface {
	Search(ctx context.Context, query string, entries []Entry) []Entry
}

type KeywordRetriever struct{}

func (KeywordRetriever) Search(ctx context.Context, query string, entries []Entry) []Entry {
	results := []Entry{}
	if ctx.Err() != nil {
		return results
	}
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	if len(terms) == 0 {
		return results
	}
	type hit struct {
		entry Entry
		score int
	}
	hits := make([]hit, 0, len(entries))
	for _, entry := range entries {
		haystack := strings.ToLower(entry.Text + " " + entry.StepType)
		score := 0
		for _, term := range terms {
			if strings.Contains(haystack, term) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, hit{entry: entry, score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].entry.StepIndex < hits[j].entry.StepIndex
	})
	for _, result := range hits {
		results = append(results, result.entry)
	}
	return results
}
