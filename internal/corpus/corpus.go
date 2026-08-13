// Package corpus is the read-only research corpus adapter for Relay's
// reliability lab. It embeds a small, synthetic, original set of documents
// (see testdata/corpus) and exposes them through the same List/Read shape
// the record_finding/read_source MCP tools present to the planner, so the
// MCP adapter can be a thin pass-through over this package.
//
// The corpus is validated once, at Load time: every manifest entry's id,
// path and size are checked, and its declared sha256 is verified against the
// embedded file's actual content. Load either returns a Corpus every one of
// whose entries is known-good, or an error — callers never see a partially
// validated corpus.
package corpus

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/diegoaleyvag/relay/internal/core"
)

// corpusData embeds the entire synthetic corpus directory: the manifest and
// every source file it references. Embedding (rather than reading from disk
// at runtime) makes the corpus part of the compiled binary, so it is
// available in any deployment without shipping a separate data directory.
//
//go:embed testdata/corpus
var corpusData embed.FS

const (
	// MaxSourceBytes is the hard cap on any single source's content, both at
	// manifest-validation time (Load rejects a manifest entry whose declared
	// size exceeds it) and at read time (Read never returns more than this
	// many bytes even if the caller asks for more).
	MaxSourceBytes = 64 * 1024

	// corpusRoot is corpusData's embedded subdirectory holding manifest.json
	// and the sources/ directory it references.
	corpusRoot = "testdata/corpus"

	// manifestName is the manifest file's name within corpusRoot.
	manifestName = "manifest.json"

	// defaultListLimit and maxListLimit bound List's page size: an unset or
	// non-positive limit falls back to defaultListLimit, and any requested
	// limit above maxListLimit is capped to it.
	defaultListLimit = 50
	maxListLimit     = 100
)

// idPattern is the allowed shape of a source id: lowercase letters, digits
// and hyphens, 1-64 characters. It exists so ids are always safe to use as
// map keys, log fields and (indirectly, via the manifest path they name)
// filesystem lookups.
var idPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// ErrSourceNotFound is returned by Read when id does not name a source in
// the loaded corpus.
var ErrSourceNotFound = errors.New("corpus: source not found")

// ErrSourceTooLarge is returned by Read when the requested source exceeds
// MaxSourceBytes and the caller did not supply a maxBytes cap (i.e. did not
// explicitly opt in to receiving a truncated read).
var ErrSourceTooLarge = errors.New("corpus: source exceeds max size and no max_bytes was requested")

// rawManifest is the on-disk shape of manifest.json.
type rawManifest struct {
	Version        string      `json:"version"`
	MaxSourceBytes int         `json:"max_source_bytes"`
	Sources        []rawSource `json:"sources"`
}

// rawSource is one manifest entry as it appears in JSON.
type rawSource struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Path      string   `json:"path"`
	MediaType string   `json:"media_type"`
	Bytes     int      `json:"bytes"`
	SHA256    string   `json:"sha256"`
	Tags      []string `json:"tags"`
}

// sourceEntry is the validated, in-memory form of one corpus source: a
// rawSource whose id, path and size have been checked and whose sha256 has
// been verified against the actual embedded file.
type sourceEntry struct {
	id        string
	title     string
	relPath   string // cleaned, slash-separated, relative to corpusRoot
	mediaType string
	bytes     int
	tags      []string
}

// Corpus is a validated, loaded view of the embedded research corpus. It is
// immutable after Load returns, so a *Corpus is safe for concurrent use by
// many goroutines (e.g. many concurrent MCP tool calls).
type Corpus struct {
	fs      fs.FS         // rooted at corpusRoot, via fs.Sub
	entries []sourceEntry // sorted by id
	byID    map[string]sourceEntry
}

// Load reads and validates the embedded corpus manifest. For every entry it
// checks that:
//
//   - the id matches ^[a-z0-9-]{1,64}$;
//   - the path, once cleaned, is a valid relative path that cannot escape
//     the corpus root (no absolute paths, no ".." components);
//   - the declared byte size does not exceed MaxSourceBytes;
//   - the referenced file exists, and its actual size and sha256 match the
//     manifest exactly.
//
// Load returns an error describing the first problem found; a *Corpus is
// only ever returned once every entry has passed all of the above.
func Load() (*Corpus, error) {
	sub, err := fs.Sub(corpusData, corpusRoot)
	if err != nil {
		return nil, fmt.Errorf("corpus: open corpus root: %w", err)
	}
	raw, err := fs.ReadFile(sub, manifestName)
	if err != nil {
		return nil, fmt.Errorf("corpus: read manifest: %w", err)
	}
	return loadFromManifestBytes(raw, sub)
}

// loadFromManifestBytes is Load's testable core: it parses raw as a
// manifest and validates every entry against sub. Factoring it out of Load
// lets tests (and the fuzz target) exercise the validation logic against
// deliberately malformed manifests and in-memory filesystems, without
// needing a second copy of the embedded corpus.
func loadFromManifestBytes(raw []byte, sub fs.FS) (*Corpus, error) {
	var mf rawManifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return nil, fmt.Errorf("corpus: parse manifest: %w", err)
	}
	if mf.MaxSourceBytes != MaxSourceBytes {
		return nil, fmt.Errorf("corpus: manifest max_source_bytes %d does not match corpus.MaxSourceBytes %d", mf.MaxSourceBytes, MaxSourceBytes)
	}

	entries := make([]sourceEntry, 0, len(mf.Sources))
	byID := make(map[string]sourceEntry, len(mf.Sources))
	for _, rs := range mf.Sources {
		entry, err := validateSource(sub, rs)
		if err != nil {
			return nil, err
		}
		if _, dup := byID[entry.id]; dup {
			return nil, fmt.Errorf("corpus: duplicate source id %q", entry.id)
		}
		byID[entry.id] = entry
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })

	return &Corpus{fs: sub, entries: entries, byID: byID}, nil
}

// validateSource checks one manifest entry against sub and, if it passes
// every check, returns the validated sourceEntry.
func validateSource(sub fs.FS, rs rawSource) (sourceEntry, error) {
	if !idPattern.MatchString(rs.ID) {
		return sourceEntry{}, fmt.Errorf("corpus: invalid source id %q (must match %s)", rs.ID, idPattern.String())
	}

	relPath, err := safeRelPath(rs.Path)
	if err != nil {
		return sourceEntry{}, fmt.Errorf("corpus: source %q: %w", rs.ID, err)
	}

	if rs.Bytes < 0 || rs.Bytes > MaxSourceBytes {
		return sourceEntry{}, fmt.Errorf("corpus: source %q declares %d bytes, exceeds max %d", rs.ID, rs.Bytes, MaxSourceBytes)
	}

	data, err := fs.ReadFile(sub, relPath)
	if err != nil {
		return sourceEntry{}, fmt.Errorf("corpus: source %q: read %q: %w", rs.ID, rs.Path, err)
	}
	if len(data) != rs.Bytes {
		return sourceEntry{}, fmt.Errorf("corpus: source %q: manifest declares %d bytes, file is %d bytes", rs.ID, rs.Bytes, len(data))
	}

	sum := sha256.Sum256(data)
	gotSHA := hex.EncodeToString(sum[:])
	if !strings.EqualFold(gotSHA, rs.SHA256) {
		return sourceEntry{}, fmt.Errorf("corpus: source %q: manifest sha256 %s does not match actual %s", rs.ID, rs.SHA256, gotSHA)
	}

	return sourceEntry{
		id:        rs.ID,
		title:     rs.Title,
		relPath:   relPath,
		mediaType: rs.MediaType,
		bytes:     rs.Bytes,
		tags:      append([]string(nil), rs.Tags...),
	}, nil
}

// safeRelPath cleans p and confirms it is a valid, relative path that
// cannot escape the corpus root: no leading slash, no ".." component, not
// empty, and not the root itself. It returns the cleaned, slash-separated
// path suitable for fs.FS lookups (fs.FS always uses "/", regardless of
// GOOS, so the filepath.Clean result — which uses the OS separator — is
// converted back to slash form).
func safeRelPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if !fs.ValidPath(cleaned) {
		return "", fmt.Errorf("unsafe path %q", p)
	}
	if cleaned == "." {
		return "", fmt.Errorf("path %q resolves to the corpus root", p)
	}
	return cleaned, nil
}

// toSourceRef converts a validated sourceEntry into the metadata-only
// core.SourceRef the List API returns. Tags is defensively copied so a
// caller mutating the returned slice cannot corrupt the Corpus's internal
// state.
func toSourceRef(e sourceEntry) core.SourceRef {
	return core.SourceRef{
		ID:        e.id,
		Title:     e.title,
		MediaType: e.mediaType,
		Bytes:     e.bytes,
		Tags:      append([]string(nil), e.tags...),
	}
}

// List returns source metadata (never content), optionally filtered by tag
// (an empty tag matches every source), sorted by id, and paginated.
//
// limit bounds the page size: a non-positive limit uses a sensible default
// (defaultListLimit) and any limit above maxListLimit is capped to it. The
// returned cursor is opaque — callers must not construct or parse it
// themselves — and is "" once there are no more matching sources; passing a
// non-empty cursor back into a later call resumes immediately after the
// last source that call's page returned.
func (c *Corpus) List(tag string, limit int, cursor string) ([]core.SourceRef, string, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	afterID := ""
	if cursor != "" {
		id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("corpus: invalid cursor: %w", err)
		}
		afterID = id
	}

	matched := make([]sourceEntry, 0, len(c.entries))
	for _, e := range c.entries { // c.entries is already sorted by id
		if tag != "" && !hasTag(e.tags, tag) {
			continue
		}
		if afterID != "" && e.id <= afterID {
			continue
		}
		matched = append(matched, e)
	}

	refs := make([]core.SourceRef, 0, min(limit, len(matched)))
	next := ""
	for i, e := range matched {
		if i == limit {
			next = encodeCursor(matched[i-1].id)
			break
		}
		refs = append(refs, toSourceRef(e))
	}

	return refs, next, nil
}

// Read returns one source's content, capped at min(MaxSourceBytes, maxBytes)
// when maxBytes > 0, or at MaxSourceBytes when maxBytes <= 0 ("not given").
// Truncated reports whether the source's actual size exceeds the cap that
// was applied (i.e. whether the returned Content is a prefix of the full
// source).
//
// Read returns ErrSourceNotFound if id does not name a loaded source, and
// ErrSourceTooLarge if the source exceeds MaxSourceBytes while maxBytes was
// not given (callers must explicitly opt in, via maxBytes, to receiving a
// truncated read of an over-size source).
func (c *Corpus) Read(id string, maxBytes int) (core.ReadSourceOutput, error) {
	e, ok := c.byID[id]
	if !ok {
		return core.ReadSourceOutput{}, fmt.Errorf("%w: %q", ErrSourceNotFound, id)
	}

	if maxBytes <= 0 && e.bytes > MaxSourceBytes {
		return core.ReadSourceOutput{}, fmt.Errorf("%w: %q is %d bytes", ErrSourceTooLarge, id, e.bytes)
	}

	capBytes := MaxSourceBytes
	if maxBytes > 0 && maxBytes < capBytes {
		capBytes = maxBytes
	}

	f, err := c.fs.Open(e.relPath)
	if err != nil {
		return core.ReadSourceOutput{}, fmt.Errorf("corpus: open %q: %w", id, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, int64(capBytes)))
	if err != nil {
		return core.ReadSourceOutput{}, fmt.Errorf("corpus: read %q: %w", id, err)
	}

	return core.ReadSourceOutput{
		ID:        e.id,
		Title:     e.title,
		MediaType: e.mediaType,
		Content:   string(data),
		Bytes:     len(data),
		Truncated: e.bytes > capBytes,
	}, nil
}

// hasTag reports whether tags contains tag exactly.
func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// encodeCursor and decodeCursor implement List's pagination cursor. The
// encoding is an implementation detail — callers must treat cursors as
// opaque — but is documented here for maintainers: a cursor is the
// base64 (raw URL, unpadded) encoding of the id of the last source the
// previous page returned. Decoding never fails to be safe: an invalid
// cursor is reported as an error to the caller rather than silently
// resetting pagination.
func encodeCursor(lastID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(lastID))
}

func decodeCursor(cursor string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
