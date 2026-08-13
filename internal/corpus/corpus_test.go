package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func loadTestCorpus(t *testing.T) *Corpus {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	return c
}

func TestLoadSucceeds(t *testing.T) {
	loadTestCorpus(t)
}

func TestListOrderedByID(t *testing.T) {
	c := loadTestCorpus(t)
	refs, next, err := c.List("", 100, "")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	wantIDs := []string{"src-001", "src-002", "src-003", "src-004", "src-005", "src-006"}
	if len(refs) != len(wantIDs) {
		t.Fatalf("List returned %d refs, want %d", len(refs), len(wantIDs))
	}
	for i, want := range wantIDs {
		if refs[i].ID != want {
			t.Fatalf("refs[%d].ID = %q, want %q (full: %+v)", i, refs[i].ID, want, refs)
		}
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty (all results returned)", next)
	}
}

func TestListTagFilter(t *testing.T) {
	c := loadTestCorpus(t)
	refs, next, err := c.List("climate", 100, "")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	want := []string{"src-001", "src-004"}
	if len(refs) != len(want) {
		t.Fatalf("List(tag=climate) returned %d refs, want %d: %+v", len(refs), len(want), refs)
	}
	for i, id := range want {
		if refs[i].ID != id {
			t.Fatalf("refs[%d].ID = %q, want %q", i, refs[i].ID, id)
		}
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty", next)
	}
}

func TestListUnknownTagReturnsEmpty(t *testing.T) {
	c := loadTestCorpus(t)
	refs, next, err := c.List("no-such-tag", 100, "")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("List(unknown tag) returned %d refs, want 0", len(refs))
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty", next)
	}
}

func TestListPagination(t *testing.T) {
	c := loadTestCorpus(t)

	var gotIDs []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("pagination did not terminate")
		}
		refs, next, err := c.List("", 2, cursor)
		if err != nil {
			t.Fatalf("List() error: %v", err)
		}
		for _, r := range refs {
			gotIDs = append(gotIDs, r.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}

	want := []string{"src-001", "src-002", "src-003", "src-004", "src-005", "src-006"}
	if len(gotIDs) != len(want) {
		t.Fatalf("paginated ids = %v, want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("paginated ids = %v, want %v", gotIDs, want)
		}
	}
}

func TestListLimitDefaultsAndCaps(t *testing.T) {
	c := loadTestCorpus(t)

	refs, next, err := c.List("", 1, "")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("List(limit=1) returned %d refs, want 1", len(refs))
	}
	if refs[0].ID != "src-001" {
		t.Fatalf("refs[0].ID = %q, want src-001", refs[0].ID)
	}
	if next == "" {
		t.Fatal("next cursor is empty, want a cursor since more sources remain")
	}

	// limit <= 0 falls back to a sensible default that comfortably covers
	// this small corpus, so every source comes back on one page.
	refs, next, err = c.List("", 0, "")
	if err != nil {
		t.Fatalf("List(limit=0) error: %v", err)
	}
	if len(refs) != 6 {
		t.Fatalf("List(limit=0) returned %d refs, want 6", len(refs))
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty", next)
	}
}

func TestListInvalidCursorErrors(t *testing.T) {
	c := loadTestCorpus(t)
	if _, _, err := c.List("", 10, "not valid base64!!"); err == nil {
		t.Fatal("List with an invalid cursor returned nil error, want an error")
	}
}

func TestReadContentMatchesFileAndMetadata(t *testing.T) {
	c := loadTestCorpus(t)
	out, err := c.Read("src-001", 0)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if out.ID != "src-001" {
		t.Fatalf("out.ID = %q, want src-001", out.ID)
	}
	if out.Title == "" {
		t.Fatal("out.Title is empty")
	}
	if out.MediaType != "text/markdown" {
		t.Fatalf("out.MediaType = %q, want text/markdown", out.MediaType)
	}
	if out.Truncated {
		t.Fatal("out.Truncated = true, want false (content fits under the cap)")
	}
	if out.Bytes != len(out.Content) {
		t.Fatalf("out.Bytes = %d, len(out.Content) = %d, want equal", out.Bytes, len(out.Content))
	}

	want, err := os.ReadFile("testdata/corpus/sources/src-001-urban-heat-islands.md")
	if err != nil {
		t.Fatalf("reading reference file: %v", err)
	}
	if out.Content != string(want) {
		t.Fatalf("out.Content does not match the on-disk file (got %d bytes, want %d)", len(out.Content), len(want))
	}
}

func TestReadCapsAndSetsTruncated(t *testing.T) {
	c := loadTestCorpus(t)
	out, err := c.Read("src-001", 50)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if len(out.Content) != 50 {
		t.Fatalf("len(out.Content) = %d, want 50", len(out.Content))
	}
	if out.Bytes != 50 {
		t.Fatalf("out.Bytes = %d, want 50", out.Bytes)
	}
	if !out.Truncated {
		t.Fatal("out.Truncated = false, want true")
	}
}

func TestReadMaxBytesAboveSourceSizeIsNotTruncated(t *testing.T) {
	c := loadTestCorpus(t)
	out, err := c.Read("src-001", 1_000_000)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if out.Truncated {
		t.Fatal("out.Truncated = true, want false (maxBytes exceeds the source size)")
	}
}

func TestReadUnknownIDReturnsErrSourceNotFound(t *testing.T) {
	c := loadTestCorpus(t)
	_, err := c.Read("does-not-exist", 0)
	if !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("Read() error = %v, want ErrSourceNotFound", err)
	}
}

// --- Manifest validation, exercised via loadFromManifestBytes against an
// in-memory fs.FS so each test can deliberately break one aspect of an
// otherwise-valid manifest fixture. ---

const fixtureContent = "hello world, this is a valid synthetic corpus entry.\n"

// validManifestFixture returns a fresh, valid manifest (as a mutable
// map[string]any, so tests can corrupt one field at a time) together with
// the in-memory filesystem backing it.
func validManifestFixture() (map[string]any, fstest.MapFS) {
	sum := sha256.Sum256([]byte(fixtureContent))
	entry := map[string]any{
		"id":         "src-999",
		"title":      "Fixture",
		"path":       "sources/src-999.txt",
		"media_type": "text/plain",
		"bytes":      len(fixtureContent),
		"sha256":     hex.EncodeToString(sum[:]),
		"tags":       []string{"fixture"},
	}
	mf := map[string]any{
		"version":          "1",
		"max_source_bytes": MaxSourceBytes,
		"sources":          []any{entry},
	}
	memFS := fstest.MapFS{
		"sources/src-999.txt": &fstest.MapFile{Data: []byte(fixtureContent)},
	}
	return mf, memFS
}

func marshalManifest(t *testing.T, mf map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(mf)
	if err != nil {
		t.Fatalf("marshal manifest fixture: %v", err)
	}
	return b
}

func TestLoadFromManifestBytesAcceptsValidFixture(t *testing.T) {
	mf, memFS := validManifestFixture()
	if _, err := loadFromManifestBytes(marshalManifest(t, mf), memFS); err != nil {
		t.Fatalf("loadFromManifestBytes() on a valid fixture returned %v, want nil", err)
	}
}

func TestLoadRejectsInvalidID(t *testing.T) {
	mf, memFS := validManifestFixture()
	mf["sources"].([]any)[0].(map[string]any)["id"] = "Not_Valid_ID!"
	if _, err := loadFromManifestBytes(marshalManifest(t, mf), memFS); err == nil {
		t.Fatal("expected an error for an invalid id, got nil")
	}
}

func TestLoadRejectsPathTraversal(t *testing.T) {
	mf, memFS := validManifestFixture()
	mf["sources"].([]any)[0].(map[string]any)["path"] = "../../etc/passwd"
	if _, err := loadFromManifestBytes(marshalManifest(t, mf), memFS); err == nil {
		t.Fatal("expected an error for a path-traversal path, got nil")
	}
}

func TestLoadRejectsAbsolutePath(t *testing.T) {
	mf, memFS := validManifestFixture()
	mf["sources"].([]any)[0].(map[string]any)["path"] = "/etc/passwd"
	if _, err := loadFromManifestBytes(marshalManifest(t, mf), memFS); err == nil {
		t.Fatal("expected an error for an absolute path, got nil")
	}
}

func TestLoadRejectsSizeMismatch(t *testing.T) {
	mf, memFS := validManifestFixture()
	mf["sources"].([]any)[0].(map[string]any)["bytes"] = len(fixtureContent) + 5
	if _, err := loadFromManifestBytes(marshalManifest(t, mf), memFS); err == nil {
		t.Fatal("expected an error for a byte-size mismatch, got nil")
	}
}

func TestLoadRejectsOversizeDeclaredBytes(t *testing.T) {
	mf, memFS := validManifestFixture()
	mf["sources"].([]any)[0].(map[string]any)["bytes"] = MaxSourceBytes + 1
	if _, err := loadFromManifestBytes(marshalManifest(t, mf), memFS); err == nil {
		t.Fatal("expected an error for bytes exceeding MaxSourceBytes, got nil")
	}
}

func TestLoadRejectsSHA256Mismatch(t *testing.T) {
	mf, memFS := validManifestFixture()
	mf["sources"].([]any)[0].(map[string]any)["sha256"] = strings.Repeat("f", 64)
	if _, err := loadFromManifestBytes(marshalManifest(t, mf), memFS); err == nil {
		t.Fatal("expected an error for a sha256 mismatch, got nil")
	}
}

func TestLoadRejectsMissingFile(t *testing.T) {
	mf, _ := validManifestFixture()
	emptyFS := fstest.MapFS{}
	if _, err := loadFromManifestBytes(marshalManifest(t, mf), emptyFS); err == nil {
		t.Fatal("expected an error for a manifest entry with no backing file, got nil")
	}
}

func TestLoadRejectsDuplicateID(t *testing.T) {
	mf, memFS := validManifestFixture()
	sources := mf["sources"].([]any)
	dup := map[string]any{}
	for k, v := range sources[0].(map[string]any) {
		dup[k] = v
	}
	mf["sources"] = append(sources, dup)
	if _, err := loadFromManifestBytes(marshalManifest(t, mf), memFS); err == nil {
		t.Fatal("expected an error for duplicate ids, got nil")
	}
}

func TestLoadRejectsWrongDeclaredMaxSourceBytes(t *testing.T) {
	mf, memFS := validManifestFixture()
	mf["max_source_bytes"] = 123
	if _, err := loadFromManifestBytes(marshalManifest(t, mf), memFS); err == nil {
		t.Fatal("expected an error for a manifest max_source_bytes mismatch, got nil")
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	if _, err := loadFromManifestBytes([]byte("{not json"), fstest.MapFS{}); err == nil {
		t.Fatal("expected an error for malformed JSON, got nil")
	}
}

func TestManifestEntriesMatchOnDiskFiles(t *testing.T) {
	// Cross-check: re-read manifest.json directly (independent of Load's own
	// validation) and confirm every declared sha256/bytes value matches the
	// corresponding file under testdata/corpus/sources, and that Load()
	// exposes the same content via Read.
	raw, err := os.ReadFile("testdata/corpus/manifest.json")
	if err != nil {
		t.Fatalf("reading manifest.json: %v", err)
	}
	var mf rawManifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatalf("parsing manifest.json: %v", err)
	}
	if len(mf.Sources) == 0 {
		t.Fatal("manifest.json declares no sources")
	}

	c := loadTestCorpus(t)

	for _, rs := range mf.Sources {
		if !idPattern.MatchString(rs.ID) {
			t.Errorf("manifest id %q does not match %s", rs.ID, idPattern.String())
		}

		data, err := os.ReadFile("testdata/corpus/" + rs.Path)
		if err != nil {
			t.Errorf("source %q: reading %q: %v", rs.ID, rs.Path, err)
			continue
		}
		if len(data) != rs.Bytes {
			t.Errorf("source %q: manifest bytes %d != file bytes %d", rs.ID, rs.Bytes, len(data))
		}
		if rs.Bytes > MaxSourceBytes {
			t.Errorf("source %q: bytes %d exceeds MaxSourceBytes %d", rs.ID, rs.Bytes, MaxSourceBytes)
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got != rs.SHA256 {
			t.Errorf("source %q: manifest sha256 %s != actual %s", rs.ID, rs.SHA256, got)
		}

		out, err := c.Read(rs.ID, 0)
		if err != nil {
			t.Errorf("Corpus.Read(%q) error: %v", rs.ID, err)
			continue
		}
		if out.Content != string(data) {
			t.Errorf("Corpus.Read(%q) content does not match the on-disk file", rs.ID)
		}
	}
}
