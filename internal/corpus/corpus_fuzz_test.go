package corpus

import "testing"

// FuzzCorpusReader fuzzes Corpus.Read and Corpus.List — the corpus's two
// reading entry points — with arbitrary ids, byte caps, tags, limits and
// cursors. Both methods must reject bad input with a plain error and must
// never panic, since a malformed request (e.g. an id crafted to look like a
// path-traversal attempt, or a corrupted cursor) is exactly the kind of
// input an MCP client could send.
//
// The corpus is loaded once, outside the fuzz function, and reused across
// every fuzz iteration: Corpus is immutable after Load and therefore safe
// for concurrent use, and reloading the (tiny) embedded corpus on every
// iteration would only slow the fuzzer down for no additional coverage.
func FuzzCorpusReader(f *testing.F) {
	c, err := Load()
	if err != nil {
		f.Fatalf("Load() error: %v", err)
	}

	seeds := []struct {
		id       string
		maxBytes int
		tag      string
		limit    int
		cursor   string
	}{
		{"src-001", 0, "", 0, ""},
		{"src-001", 10, "climate", 2, ""},
		{"", 0, "", 0, ""},
		{"does-not-exist", -1, "no-such-tag", -5, "!!not-valid-base64!!"},
		{"../../etc/passwd", 1_000_000, "", 1000, "AAAA"},
		{"src-006", 1, "air-quality", 1, "c3JjLTAwMQ"},
	}
	for _, s := range seeds {
		f.Add(s.id, s.maxBytes, s.tag, s.limit, s.cursor)
	}

	f.Fuzz(func(t *testing.T, id string, maxBytes int, tag string, limit int, cursor string) {
		// Neither call should ever panic, regardless of input shape; errors
		// are an expected, valid outcome and are otherwise unchecked here.
		_, _ = c.Read(id, maxBytes)
		_, _, _ = c.List(tag, limit, cursor)
	})
}
