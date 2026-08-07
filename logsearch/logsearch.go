// Package logsearch reads a process's own in-memory log ring, so "what just
// happened" can be answered without leaving the process.
//
// It wraps github.com/psyb0t/slog-configurator/logring with the parts a
// caller-facing surface needs and the ring deliberately does not have: a page
// size it will not exceed, a per-line cap so one enormous record cannot crowd
// out every other match, and a total-matches count so a reader can tell "100 of
// 4312" from "100 of 100".
//
// It is a debugging aid, not a log store. The ring is per process and dies with
// it, so a crashed or restarted task shows nothing, and in a multi-replica
// deployment it reflects whichever replica served the call. Ship logs somewhere
// durable as well.
package logsearch

import (
	"log/slog"
	"regexp"
	"time"

	commonerrors "github.com/psyb0t/common-go/errors"
	"github.com/psyb0t/ctxerrors"
	"github.com/psyb0t/slog-configurator/logring"
)

const (
	// DefaultLimit keeps an unfiltered read from returning the whole ring to a
	// caller that just asked for "the logs".
	DefaultLimit = 100

	// MaxLimit bounds the response no matter what is asked for. Entries are
	// whole formatted lines, so a large page is a large payload.
	MaxLimit = 1000

	// MaxLineRunes truncates a single stored line in the response. One
	// pathological record — a dumped payload, a stack trace — must not consume
	// the entire result and crowd out the other matches.
	MaxLineRunes = 4000
)

// Ring is the slice of the log ring this package needs.
//
// It is an interface rather than *logring.Handler so a caller can substitute a
// fake in tests, and so this package does not depend on the ring's whole
// surface.
type Ring interface {
	Search(opts logring.SearchOptions) []logring.Entry
	Count(opts logring.SearchOptions) int
	Stats() (entries, bytes int, dropped uint64)
}

// Options is a search request with every value already resolved. Parsing
// relative times ("15m") and level names into these belongs to whatever layer
// accepts them from a user.
type Options struct {
	Contains  string
	Exclude   string
	Match     *regexp.Regexp
	Attrs     map[string]string
	MinLevel  slog.Leveler
	Levels    []slog.Level
	Since     time.Time
	Until     time.Time
	Limit     int
	Offset    int
	Ascending bool
}

// Entry is one returned record.
//
// Attrs is a map because that is what a JSON payload wants. The ring keeps
// attributes ordered, but a reader filtering on request_id does not care about
// order.
type Entry struct {
	Time      time.Time         `json:"time"`
	Level     string            `json:"level"`
	Msg       string            `json:"msg"`
	Line      string            `json:"line"`
	Attrs     map[string]string `json:"attrs,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
}

// Result is the search payload.
//
// TotalMatches is the count BEFORE limit and offset, which is the field that
// makes paging deliberate instead of guesswork: without it a caller cannot
// distinguish a full page from the last page.
//
//nolint:tagliatelle // snake_case: this payload is read by tools and models
type Result struct {
	Entries      []Entry `json:"entries"`
	Returned     int     `json:"returned"`
	TotalMatches int     `json:"total_matches"`
	Offset       int     `json:"offset"`
	RingEntries  int     `json:"ring_entries"`
	RingBytes    int     `json:"ring_bytes"`
	RingDropped  uint64  `json:"ring_dropped_oversized"`
}

// Search reads the ring.
//
// A nil ring returns commonerrors.ErrUnavailable rather than an empty result:
// "the ring was never wired" and "nothing matched" are different answers, and
// collapsing them sends the caller looking for a bug in their filter.
func Search(ring Ring, opts Options) (*Result, error) {
	if ring == nil {
		return nil, ctxerrors.Wrap(
			commonerrors.ErrUnavailable,
			"log ring: process started without an in-memory ring",
		)
	}

	searchOpts := logring.SearchOptions{
		Contains:  opts.Contains,
		Exclude:   opts.Exclude,
		Match:     opts.Match,
		Attrs:     opts.Attrs,
		MinLevel:  opts.MinLevel,
		Levels:    opts.Levels,
		Since:     opts.Since,
		Until:     opts.Until,
		Limit:     clampLimit(opts.Limit),
		Offset:    opts.Offset,
		Ascending: opts.Ascending,
	}

	// The count runs the same filters without paging, so it answers "how many
	// match" rather than "how many did this page return".
	countOpts := searchOpts
	countOpts.Limit = 0
	countOpts.Offset = 0

	total := ring.Count(countOpts)
	found := ring.Search(searchOpts)
	entries, ringBytes, dropped := ring.Stats()

	out := make([]Entry, 0, len(found))
	for _, entry := range found {
		out = append(out, toEntry(entry))
	}

	return &Result{
		Entries:      out,
		Returned:     len(out),
		TotalMatches: total,
		Offset:       opts.Offset,
		RingEntries:  entries,
		RingBytes:    ringBytes,
		RingDropped:  dropped,
	}, nil
}

// toEntry converts a ring entry, truncating an oversized line.
func toEntry(entry logring.Entry) Entry {
	line, truncated := truncateRunes(entry.Line, MaxLineRunes)

	out := Entry{
		Time:      entry.Time,
		Level:     entry.Level.String(),
		Msg:       entry.Msg,
		Line:      line,
		Truncated: truncated,
	}

	if len(entry.Attrs) == 0 {
		return out
	}

	out.Attrs = make(map[string]string, len(entry.Attrs))
	for _, attr := range entry.Attrs {
		out.Attrs[attr.Key] = attr.Value
	}

	return out
}

// truncateRunes cuts on a rune boundary, so a multi-byte character is never
// split into invalid UTF-8 in the response.
func truncateRunes(value string, limit int) (string, bool) {
	runes := []rune(value)
	if len(runes) <= limit {
		return value, false
	}

	return string(runes[:limit]), true
}

// clampLimit bounds the page size, defaulting when unset.
func clampLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}

	return min(limit, MaxLimit)
}
