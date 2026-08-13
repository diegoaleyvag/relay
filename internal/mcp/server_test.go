package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/diegoaleyvag/relay/internal/core"
)

// fakeSource is one entry in fakeSources' tiny in-memory corpus.
type fakeSource struct {
	ref     core.SourceRef
	content string
}

// fakeSources is a minimal Sources implementation backed by a fixed slice,
// used across this package's tests so they don't depend on the real corpus
// adapter (internal/corpus), which is a separate, later milestone.
type fakeSources struct {
	items []fakeSource
}

// newFakeSources returns a fakeSources with two known entries: "s1" (tagged
// "alpha") and "s2" (tagged "beta").
func newFakeSources() *fakeSources {
	return &fakeSources{items: []fakeSource{
		{
			ref:     core.SourceRef{ID: "s1", Title: "Alpha Doc", MediaType: "text/plain", Bytes: 5, Tags: []string{"alpha"}},
			content: "alpha",
		},
		{
			ref:     core.SourceRef{ID: "s2", Title: "Beta Doc", MediaType: "text/plain", Bytes: 4, Tags: []string{"beta"}},
			content: "beta",
		},
	}}
}

func (f *fakeSources) List(tag string, limit int, _ string) ([]core.SourceRef, string, error) {
	var out []core.SourceRef
	for _, it := range f.items {
		if tag != "" && !hasTag(it.ref.Tags, tag) {
			continue
		}
		out = append(out, it.ref)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, "", nil
}

func (f *fakeSources) Read(id string, maxBytes int) (core.ReadSourceOutput, error) {
	for _, it := range f.items {
		if it.ref.ID != id {
			continue
		}
		content := it.content
		truncated := false
		if maxBytes > 0 && len(content) > maxBytes {
			content = content[:maxBytes]
			truncated = true
		}
		return core.ReadSourceOutput{
			ID:        it.ref.ID,
			Title:     it.ref.Title,
			MediaType: it.ref.MediaType,
			Content:   content,
			Bytes:     len(content),
			Truncated: truncated,
		}, nil
	}
	return core.ReadSourceOutput{}, fmt.Errorf("source not found: %q", id)
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// TestNewServerRegistersExactlyFourTools asserts the allowlist: the server
// must expose list_sources and read_source (both ReadOnlyHint) plus
// record_finding and request_human_review, and nothing else.
func TestNewServerRegistersExactlyFourTools(t *testing.T) {
	ctx := context.Background()
	port, closeFn, err := InMemory(ctx, newFakeSources())
	if err != nil {
		t.Fatalf("InMemory: %v", err)
	}
	defer func() {
		if err := closeFn(); err != nil {
			t.Fatalf("closeFn: %v", err)
		}
	}()

	res, err := port.Session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 4 {
		t.Fatalf("expected exactly 4 registered tools, got %d: %+v", len(res.Tools), res.Tools)
	}

	seen := map[string]bool{}
	for _, tool := range res.Tools {
		seen[tool.Name] = true
		switch tool.Name {
		case string(core.ToolListSources), string(core.ToolReadSource):
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("tool %q must be annotated ReadOnlyHint", tool.Name)
			}
		case string(core.ToolRecordFinding), string(core.ToolRequestReview):
			// side-effecting: no ReadOnlyHint expected.
		default:
			t.Errorf("unexpected tool registered: %q", tool.Name)
		}
	}
	for _, want := range []core.ToolName{
		core.ToolListSources, core.ToolReadSource, core.ToolRecordFinding, core.ToolRequestReview,
	} {
		if !seen[string(want)] {
			t.Errorf("expected tool %q to be registered", want)
		}
	}
}
