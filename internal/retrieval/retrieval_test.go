package retrieval_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/romeokeita231/Lite-agent-platform/internal/retrieval"
)

type fakeRetriever struct {
	called bool
}

func (f *fakeRetriever) Search(_ context.Context, query string, entries []retrieval.Entry) []retrieval.Entry {
	f.called = true
	if query == "" {
		return []retrieval.Entry{}
	}
	return []retrieval.Entry{entries[len(entries)-1]}
}

func searchWith(t *testing.T, retriever retrieval.Retriever, query string, entries []retrieval.Entry) []retrieval.Entry {
	t.Helper()
	return retriever.Search(context.Background(), query, entries)
}

func TestRetrieverIsReplaceable(t *testing.T) {
	fake := &fakeRetriever{}
	entries := []retrieval.Entry{{StepIndex: 0, Text: "alpha"}, {StepIndex: 1, Text: "beta"}}

	got := searchWith(t, fake, "beta", entries)
	if !fake.called {
		t.Fatalf("fake retriever was not called")
	}
	if len(got) != 1 || got[0].StepIndex != 1 {
		t.Fatalf("fake results = %+v", got)
	}
}

func TestKeywordRetrieverMatchesCaseInsensitively(t *testing.T) {
	retriever := retrieval.KeywordRetriever{}
	entries := []retrieval.Entry{
		{StepIndex: 0, StepType: "tool_call", Text: "Lookup ORDER A1"},
		{StepIndex: 1, StepType: "llm_call", Text: "summarize order"},
	}
	got := searchWith(t, retriever, "a1", entries)
	if len(got) != 1 || got[0].StepIndex != 0 {
		t.Fatalf("case-insensitive results = %+v", got)
	}

	got = searchWith(t, retriever, "ORDER", entries)
	if len(got) != 2 || got[0].StepIndex != 0 || got[1].StepIndex != 1 {
		t.Fatalf("case-insensitive results = %+v", got)
	}
}

func TestKeywordRetrieverRanksAndBreaksTiesByStepIndex(t *testing.T) {
	retriever := retrieval.KeywordRetriever{}
	entries := []retrieval.Entry{
		{StepIndex: 5, StepType: "tool_call", Text: "order lookup"},
		{StepIndex: 2, StepType: "tool_call", Text: "order lookup"},
		{StepIndex: 7, StepType: "tool_call", Text: "order"},
		{StepIndex: 9, StepType: "llm_call", Text: "unrelated"},
	}
	got := searchWith(t, retriever, "order lookup", entries)
	if len(got) != 3 {
		t.Fatalf("results = %+v", got)
	}
	if got[0].StepIndex != 2 || got[1].StepIndex != 5 || got[2].StepIndex != 7 {
		t.Fatalf("results order = %+v", got)
	}
}

func TestKeywordRetrieverEmptyQueryReturnsEmpty(t *testing.T) {
	retriever := retrieval.KeywordRetriever{}
	entries := []retrieval.Entry{{StepIndex: 0, Text: "anything"}}

	got := searchWith(t, retriever, "  ", entries)
	if got == nil || len(got) != 0 {
		t.Fatalf("empty query results = %+v", got)
	}
	if !reflect.DeepEqual(got, []retrieval.Entry{}) {
		t.Fatalf("empty query results = %+v", got)
	}
}
