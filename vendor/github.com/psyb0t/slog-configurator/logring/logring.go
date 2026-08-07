// Package logring keeps the most recent log records in a bounded in-memory
// ring so a process can search its own logs without leaving the process.
//
// It is an slog.Handler, so it composes with this repo's fan-out:
//
//	slogconfigurator.AddHandler(logring.New(logring.Options{}))
//
// The ring is a debugging aid, not a log store. It is bounded, it is per
// process, and it dies with the process — the moment you most want logs (a
// crash) is the moment it is gone. Ship logs somewhere durable as well.
package logring

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/psyb0t/ctxerrors"
)

const (
	// DefaultMaxBytes bounds the ring by the size of the records it holds
	// rather than a record count: one pathological 100 KB line must not be
	// able to evict a hundred useful ones.
	DefaultMaxBytes = 100 << 20 // 100 MiB

	// DefaultMaxRecordBytes drops any single record larger than this instead
	// of letting it evict a large share of the ring on its own.
	DefaultMaxRecordBytes = 1 << 20 // 1 MiB

	// DefaultLevel skips DEBUG by default. A service logging every SQL
	// statement at DEBUG fills any ring with query traces in seconds and
	// buries the records worth searching for.
	DefaultLevel = slog.LevelInfo

	// DefaultSearchLimit bounds an unbounded Search so a caller cannot
	// accidentally pull the whole ring into a response.
	DefaultSearchLimit = 200

	// groupSeparator joins nested group names into one flat attribute key, so
	// a logger with WithGroup("http") logging "status" is searchable as
	// "http.status".
	groupSeparator = "."
)

// Attr is one captured attribute, rendered to its string form at capture time.
//
// The value is a string rather than a slog.Value because a stored Value can
// retain arbitrary caller objects, and a ring holding the last 100 MiB of
// records would pin every one of them for as long as the record survives.
// Capture time is also the only moment a LogValuer still resolves to what was
// actually logged.
type Attr struct {
	Key   string
	Value string
}

// Entry is one captured record: the formatted line exactly as the configured
// handler wrote it, minus its trailing newline, plus the fields needed to
// filter without re-parsing it.
//
// Attrs carries the record's attributes INCLUDING those bound earlier through
// slog.Logger.With, flattened to dotted keys. It is captured from the
// slog.Record rather than parsed back out of Line, which is what makes
// attribute search work identically whether the ring stores JSON or text.
type Entry struct {
	Time  time.Time
	Level slog.Level
	Msg   string
	Line  string
	Attrs []Attr
}

// Attr returns the value bound to key, and whether it was present at all.
func (e Entry) Attr(key string) (string, bool) {
	for _, attr := range e.Attrs {
		if attr.Key == key {
			return attr.Value, true
		}
	}

	return "", false
}

// size is the entry's contribution to the byte budget. The budget exists to
// bound memory, so everything the entry retains counts — not just the line.
func (e Entry) size() int {
	size := len(e.Line) + len(e.Msg)
	for _, attr := range e.Attrs {
		size += len(attr.Key) + len(attr.Value)
	}

	return size
}

// Options configures a ring. The zero value is usable — every field falls back
// to its Default* constant.
type Options struct {
	// MaxBytes caps the total size of retained records. <= 0 uses
	// DefaultMaxBytes.
	MaxBytes int

	// MaxRecordBytes drops any single record larger than this. <= 0 uses
	// DefaultMaxRecordBytes. It is clamped to MaxBytes — see New.
	MaxRecordBytes int

	// Level is the minimum level retained. nil uses DefaultLevel (INFO).
	Level slog.Leveler

	// Text stores human-readable lines instead of JSON. Default is JSON, so
	// the retained line matches what a JSON-configured process ships.
	//
	// This chooses the format of Line and nothing else. Attribute search reads
	// Entry.Attrs, captured from the record itself, so it behaves identically
	// either way.
	Text bool
}

// store is the shared bounded buffer. Handlers derived via WithAttrs /
// WithGroup point at the SAME store, so records reach one ring no matter which
// derived logger emitted them.
type store struct {
	mu             sync.RWMutex
	entries        []Entry
	curBytes       int
	maxBytes       int
	maxRecordBytes int
	dropped        uint64
}

// Handler captures records into a bounded ring. Safe for concurrent use.
type Handler struct {
	store *store
	level slog.Leveler

	// attrs and groups mirror what WithAttrs / WithGroup bound onto the inner
	// handler. They have to be tracked here as well because slog gives a
	// Handler no way to read them back, and a slog.Record carries only the
	// per-call attrs — so without this, the most useful thing to search by (a
	// request id bound once through With) would be missing from every Entry.
	//
	// Both are treated as immutable: derivation copies rather than appending
	// in place, so sibling handlers cannot scribble on each other.
	attrs  []Attr
	groups []string

	// fmtMu guards fmtBuf and inner together: the inner handler writes its
	// formatted output into fmtBuf, so the pair is only usable by one
	// goroutine at a time.
	fmtMu  *sync.Mutex
	fmtBuf *bytes.Buffer
	inner  slog.Handler
}

// New builds a ring handler. Add it to a fan-out rather than installing it as
// the only handler — on its own it captures logs and emits nothing.
func New(opts Options) *Handler {
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	maxRecordBytes := opts.MaxRecordBytes
	if maxRecordBytes <= 0 {
		maxRecordBytes = DefaultMaxRecordBytes
	}

	// Clamping makes maxRecordBytes <= maxBytes true by construction. A record
	// bigger than the whole ring used to pass the per-record check, get
	// appended, and then be evicted by the very loop meant to bound the ring —
	// wiping every older entry on the way through, and counting as neither
	// stored nor dropped. Rejecting it up front makes it a visible drop.
	maxRecordBytes = min(maxRecordBytes, maxBytes)

	var level slog.Leveler = DefaultLevel
	if opts.Level != nil {
		level = opts.Level
	}

	buf := &bytes.Buffer{}

	// The inner handler is deliberately given LevelDebug: this Handler's own
	// Enabled already gates on opts.Level, and a second threshold here would
	// silently win whenever it was the stricter of the two.
	handlerOpts := &slog.HandlerOptions{Level: slog.LevelDebug}

	var inner slog.Handler
	if opts.Text {
		inner = slog.NewTextHandler(buf, handlerOpts)
	} else {
		inner = slog.NewJSONHandler(buf, handlerOpts)
	}

	return &Handler{
		store: &store{
			maxBytes:       maxBytes,
			maxRecordBytes: maxRecordBytes,
		},
		level:  level,
		fmtMu:  &sync.Mutex{},
		fmtBuf: buf,
		inner:  inner,
	}
}

// Enabled reports whether the ring retains records at this level.
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// Handle formats the record and appends it, evicting oldest-first to stay
// under the byte cap.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	line, err := h.format(ctx, r)
	if err != nil {
		return err
	}

	h.store.push(Entry{
		Time:  r.Time,
		Level: r.Level,
		Msg:   r.Message,
		Line:  line,
		Attrs: h.captureAttrs(r),
	})

	return nil
}

// format runs the record through the inner handler and returns what it wrote.
func (h *Handler) format(ctx context.Context, r slog.Record) (string, error) {
	h.fmtMu.Lock()
	defer h.fmtMu.Unlock()

	h.fmtBuf.Reset()

	if err := h.inner.Handle(ctx, r); err != nil {
		return "", ctxerrors.Wrap(err, "format record for ring")
	}

	// Both stdlib handlers terminate a record with a newline. Nothing here
	// concatenates lines — an Entry IS the record boundary — so it delimits
	// nothing: it would only be a byte of the budget spent per record and a
	// trailing character every caller has to strip before printing or parsing.
	return strings.TrimRight(h.fmtBuf.String(), "\n"), nil
}

// captureAttrs flattens the handler's bound attrs plus the record's own into
// one dotted-key slice.
func (h *Handler) captureAttrs(r slog.Record) []Attr {
	if len(h.attrs) == 0 && r.NumAttrs() == 0 {
		return nil
	}

	prefix := strings.Join(h.groups, groupSeparator)

	// Copied rather than shared: an Entry outlives the call, and handing out a
	// slice that aliases h.attrs would let a caller mutating one entry's Attrs
	// change every other entry from the same logger.
	captured := make([]Attr, len(h.attrs), len(h.attrs)+r.NumAttrs())
	copy(captured, h.attrs)

	r.Attrs(func(attr slog.Attr) bool {
		captured = appendAttr(captured, prefix, attr)

		return true
	})

	return captured
}

// appendAttr renders one slog.Attr into dst, recursing through groups so a
// nested value lands under a dotted key instead of being lost.
func appendAttr(dst []Attr, prefix string, attr slog.Attr) []Attr {
	// Resolve first: a LogValuer's meaning is whatever it returns at log time,
	// and what it returns may itself be a group.
	attr.Value = attr.Value.Resolve()

	key := attr.Key
	if prefix != "" && key != "" {
		key = prefix + groupSeparator + key
	}

	if attr.Value.Kind() != slog.KindGroup {
		// slog discards an attr with no key at all; mirroring that keeps the
		// captured attrs consistent with the formatted line.
		if attr.Key == "" {
			return dst
		}

		return append(dst, Attr{Key: key, Value: attr.Value.String()})
	}

	// An empty group contributes nothing, and a group with an empty key is
	// inlined by slog into its parent rather than adding a level — which the
	// empty key above already produces, since key stays at prefix.
	if attr.Key == "" {
		key = prefix
	}

	for _, member := range attr.Value.Group() {
		dst = appendAttr(dst, key, member)
	}

	return dst
}

// WithAttrs returns a handler writing to the SAME ring with the attrs applied.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	next := h.derive(h.inner.WithAttrs(attrs))

	prefix := strings.Join(h.groups, groupSeparator)

	merged := make([]Attr, len(h.attrs), len(h.attrs)+len(attrs))
	copy(merged, h.attrs)

	for _, attr := range attrs {
		merged = appendAttr(merged, prefix, attr)
	}

	next.attrs = merged

	return next
}

// WithGroup returns a handler writing to the SAME ring with the group applied.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	next := h.derive(h.inner.WithGroup(name))
	// Clip forces the append to allocate rather than write into a tail this
	// handler's siblings may also be appending to.
	next.groups = append(slices.Clip(h.groups), name)

	return next
}

// derive builds a sibling handler around an already-derived inner handler.
//
// The buffer and its mutex are shared with the parent deliberately, not
// carelessly: slog's WithAttrs/WithGroup keep the handler's original io.Writer,
// so the derived inner handler still writes into the parent's buffer and there
// is no way to retarget it. Sharing the mutex is therefore what makes
// concurrent Handle calls across derived loggers safe. The store is shared for
// the obvious reason — every derived logger feeds one ring.
func (h *Handler) derive(inner slog.Handler) *Handler {
	return &Handler{
		store:  h.store,
		level:  h.level,
		attrs:  h.attrs,
		groups: h.groups,
		fmtMu:  h.fmtMu,
		fmtBuf: h.fmtBuf,
		inner:  inner,
	}
}

// Search returns matching entries, newest first unless opts.Ascending.
func (h *Handler) Search(opts SearchOptions) []Entry {
	return h.store.search(opts)
}

// Count reports how many entries match, without materialising them. Limit and
// Offset are ignored — the point is the total a paged Search would walk.
func (h *Handler) Count(opts SearchOptions) int {
	return h.store.count(opts)
}

// Tail returns the newest n entries in chronological order, unfiltered. It is
// the "show me what just happened" read, where Search is the "find me that one
// record" read.
func (h *Handler) Tail(n int) []Entry {
	return h.store.tail(n)
}

// Clear discards every retained entry. The dropped counter is a lifetime total
// and survives, so Stats keeps reporting records the ring refused as oversized.
func (h *Handler) Clear() {
	h.store.clear()
}

// Stats reports, in order, how many entries the ring holds, how many bytes
// they occupy, and how many records it has dropped for being oversized.
func (h *Handler) Stats() (int, int, uint64) {
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()

	return len(h.store.entries), h.store.curBytes, h.store.dropped
}

// SearchOptions filters a ring read. The zero value returns everything, newest
// first, capped at DefaultSearchLimit.
type SearchOptions struct {
	// Contains keeps only entries whose line contains this substring
	// (case-insensitive).
	Contains string

	// Exclude drops any entry whose line contains this substring
	// (case-insensitive). Applied alongside Contains, so the two compose into
	// "this but not that".
	Exclude string

	// Match keeps only entries whose line matches this expression.
	//
	// It is a compiled *regexp.Regexp rather than a pattern string so a bad
	// pattern is a caller-side error at compile time, instead of forcing
	// Search to grow an error return or to swallow it silently.
	Match *regexp.Regexp

	// Attrs keeps only entries carrying every one of these key/value pairs
	// exactly. Keys are dotted for grouped attrs — a logger with
	// WithGroup("http") logging "status" matches under "http.status".
	//
	// This reads the captured attributes, NOT the formatted line, so it works
	// the same whether the ring stores JSON or text.
	Attrs map[string]string

	// MinLevel keeps only records at or above this level. nil applies no
	// level filter at all.
	//
	// It is a Leveler rather than a slog.Level on purpose: slog.Level(0) is
	// LevelInfo, so a plain slog.Level field would make the zero value of
	// SearchOptions silently hide every DEBUG record the ring retained.
	MinLevel slog.Leveler

	// Levels keeps only records at exactly these levels, for the "warnings and
	// errors but not info" read a floor cannot express. Empty applies no
	// filter. Combined with MinLevel, both have to pass.
	Levels []slog.Level

	// Since keeps only records at or after this instant.
	Since time.Time

	// Until keeps only records at or before this instant.
	Until time.Time

	// Limit caps the results. <= 0 uses DefaultSearchLimit.
	Limit int

	// Offset skips this many matches before collecting, for paging through a
	// result larger than Limit. Skipping happens after filtering, in the same
	// order the results come back.
	Offset int

	// Ascending returns oldest first instead of newest first.
	Ascending bool
}

func (s *store) push(entry Entry) {
	size := entry.size()

	s.mu.Lock()
	defer s.mu.Unlock()

	if size > s.maxRecordBytes {
		s.dropped++

		return
	}

	s.entries = append(s.entries, entry)
	s.curBytes += size

	for s.curBytes > s.maxBytes && len(s.entries) > 0 {
		s.curBytes -= s.entries[0].size()
		// Zeroing before reslicing releases the evicted line and attrs. The
		// backing array outlives the reslice, so without this their memory
		// stays reachable and the ring holds more than curBytes claims.
		s.entries[0] = Entry{}
		s.entries = s.entries[1:]
	}
}

func (s *store) search(opts SearchOptions) []Entry {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}

	skip := max(opts.Offset, 0)
	filter := newEntryFilter(opts)

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Entry, 0, min(limit, len(s.entries)))

	for _, i := range s.walkOrder(opts.Ascending) {
		if len(out) >= limit {
			break
		}

		if !filter.keep(s.entries[i]) {
			continue
		}

		if skip > 0 {
			skip--

			continue
		}

		out = append(out, s.entries[i])
	}

	return out
}

// walkOrder yields the indices to visit in the requested direction. The caller
// holds the lock.
func (s *store) walkOrder(ascending bool) []int {
	order := make([]int, len(s.entries))
	for i := range order {
		order[i] = i
	}

	if !ascending {
		slices.Reverse(order)
	}

	return order
}

func (s *store) count(opts SearchOptions) int {
	filter := newEntryFilter(opts)

	s.mu.RLock()
	defer s.mu.RUnlock()

	matched := 0

	for _, entry := range s.entries {
		if filter.keep(entry) {
			matched++
		}
	}

	return matched
}

func (s *store) tail(n int) []Entry {
	if n <= 0 {
		return nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	start := max(len(s.entries)-n, 0)

	out := make([]Entry, len(s.entries)-start)
	copy(out, s.entries[start:])

	return out
}

func (s *store) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries = nil
	s.curBytes = 0
}

// entryFilter is SearchOptions reduced to the checks search actually runs,
// resolved once up front rather than re-derived for every entry.
type entryFilter struct {
	needle   string
	exclude  string
	match    *regexp.Regexp
	attrs    map[string]string
	minLevel slog.Level
	byLevel  bool
	levels   []slog.Level
	since    time.Time
	until    time.Time
}

func newEntryFilter(opts SearchOptions) entryFilter {
	filter := entryFilter{
		needle:  strings.ToLower(opts.Contains),
		exclude: strings.ToLower(opts.Exclude),
		match:   opts.Match,
		attrs:   opts.Attrs,
		levels:  opts.Levels,
		since:   opts.Since,
		until:   opts.Until,
	}

	// byLevel keeps "no filter" distinct from "floor at DEBUG", so a custom
	// level below DEBUG still comes back from an unfiltered Search.
	if opts.MinLevel != nil {
		filter.byLevel = true
		filter.minLevel = opts.MinLevel.Level()
	}

	return filter
}

// keep runs the cheap checks first — an integer compare, two time compares and
// a short attr scan eliminate most entries before anything has to walk a whole
// line, and the regexp runs last because it is by far the most expensive.
func (f entryFilter) keep(entry Entry) bool {
	if !f.keepLevel(entry) || !f.keepTime(entry) || !f.keepAttrs(entry) {
		return false
	}

	return f.keepLine(entry)
}

func (f entryFilter) keepLevel(entry Entry) bool {
	if f.byLevel && entry.Level < f.minLevel {
		return false
	}

	return len(f.levels) == 0 || slices.Contains(f.levels, entry.Level)
}

func (f entryFilter) keepTime(entry Entry) bool {
	if !f.since.IsZero() && entry.Time.Before(f.since) {
		return false
	}

	return f.until.IsZero() || !entry.Time.After(f.until)
}

func (f entryFilter) keepAttrs(entry Entry) bool {
	for key, want := range f.attrs {
		got, ok := entry.Attr(key)
		if !ok || got != want {
			return false
		}
	}

	return true
}

func (f entryFilter) keepLine(entry Entry) bool {
	if f.needle != "" || f.exclude != "" {
		lowered := strings.ToLower(entry.Line)

		if f.needle != "" && !strings.Contains(lowered, f.needle) {
			return false
		}

		if f.exclude != "" && strings.Contains(lowered, f.exclude) {
			return false
		}
	}

	return f.match == nil || f.match.MatchString(entry.Line)
}
