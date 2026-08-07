package logsearch

import (
	"log/slog"
	"strings"
	"testing"

	commonerrors "github.com/psyb0t/common-go/errors"
	"github.com/psyb0t/slog-configurator/logring"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRing records what it was asked for, so a test can assert on the options
// this package DERIVED rather than only on the result it returned.
type fakeRing struct {
	entries []logring.Entry
	count   int

	searchOpts logring.SearchOptions
	countOpts  logring.SearchOptions

	statEntries int
	statBytes   int
	statDropped uint64
}

func (r *fakeRing) Search(opts logring.SearchOptions) []logring.Entry {
	r.searchOpts = opts

	return r.entries
}

func (r *fakeRing) Count(opts logring.SearchOptions) int {
	r.countOpts = opts

	return r.count
}

func (r *fakeRing) Stats() (int, int, uint64) {
	return r.statEntries, r.statBytes, r.statDropped
}

// A nil ring is a configuration problem, not an empty result. Returning
// (nil, nil) would send the caller hunting for a bug in their filter.
func TestNilRingIsUnavailableNotEmpty(t *testing.T) {
	t.Parallel()

	got, err := Search(nil, Options{})

	require.Error(t, err)
	require.ErrorIs(t, err, commonerrors.ErrUnavailable)
	assert.Nil(t, got)
}

func TestLimitIsClamped(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "zero takes the default", limit: 0, want: DefaultLimit},
		{name: "negative takes the default", limit: -3, want: DefaultLimit},
		{name: "in range is kept", limit: 25, want: 25},
		{name: "above max clamps down", limit: 999999, want: MaxLimit},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ring := &fakeRing{}

			_, err := Search(ring, Options{Limit: tc.limit})
			require.NoError(t, err)

			assert.Equal(t, tc.want, ring.searchOpts.Limit)
		})
	}
}

// The count must run the same FILTERS without the PAGING, or total_matches
// would just restate how many the page returned and paging would be blind.
func TestCountDropsPagingButKeepsFilters(t *testing.T) {
	t.Parallel()

	ring := &fakeRing{count: 4312}

	got, err := Search(ring, Options{
		Contains: "timeout",
		MinLevel: slog.LevelWarn,
		Limit:    10,
		Offset:   30,
	})
	require.NoError(t, err)

	assert.Zero(t, ring.countOpts.Limit, "the count must not be paged")
	assert.Zero(t, ring.countOpts.Offset, "the count must not be offset")
	assert.Equal(t, "timeout", ring.countOpts.Contains, "filters must survive")
	assert.Equal(t, slog.LevelWarn, ring.countOpts.MinLevel)

	assert.Equal(t, 4312, got.TotalMatches)
	assert.Equal(t, 30, got.Offset)
}

func TestEveryFilterReachesTheRing(t *testing.T) {
	t.Parallel()

	ring := &fakeRing{}

	_, err := Search(ring, Options{
		Contains:  "a",
		Exclude:   "b",
		Attrs:     map[string]string{"request_id": "abc"},
		Levels:    []slog.Level{slog.LevelError},
		Offset:    5,
		Ascending: true,
	})
	require.NoError(t, err)

	assert.Equal(t, "a", ring.searchOpts.Contains)
	assert.Equal(t, "b", ring.searchOpts.Exclude)
	assert.Equal(t, map[string]string{"request_id": "abc"}, ring.searchOpts.Attrs)
	assert.Equal(t, []slog.Level{slog.LevelError}, ring.searchOpts.Levels)
	assert.Equal(t, 5, ring.searchOpts.Offset)
	assert.True(t, ring.searchOpts.Ascending)
}

func TestEntryConversion(t *testing.T) {
	t.Parallel()

	ring := &fakeRing{
		entries: []logring.Entry{{
			Level: slog.LevelWarn,
			Msg:   "handled",
			Line:  `{"msg":"handled"}`,
			Attrs: []logring.Attr{
				{Key: "request_id", Value: "abc"},
				{Key: "http.status", Value: "500"},
			},
		}},
		statEntries: 7,
		statBytes:   99,
		statDropped: 2,
	}

	got, err := Search(ring, Options{})
	require.NoError(t, err)
	require.Len(t, got.Entries, 1)

	entry := got.Entries[0]
	assert.Equal(t, "WARN", entry.Level)
	assert.Equal(t, "handled", entry.Msg)
	assert.False(t, entry.Truncated)
	assert.Equal(t, map[string]string{
		"request_id":  "abc",
		"http.status": "500",
	}, entry.Attrs)

	assert.Equal(t, 1, got.Returned)
	assert.Equal(t, 7, got.RingEntries)
	assert.Equal(t, 99, got.RingBytes)
	assert.Equal(t, uint64(2), got.RingDropped)
}

// One pathological record must not consume the whole response.
func TestOversizedLineIsTruncatedAndFlagged(t *testing.T) {
	t.Parallel()

	ring := &fakeRing{
		entries: []logring.Entry{{Line: strings.Repeat("x", MaxLineRunes+500)}},
	}

	got, err := Search(ring, Options{})
	require.NoError(t, err)
	require.Len(t, got.Entries, 1)

	assert.True(t, got.Entries[0].Truncated)
	assert.Len(t, []rune(got.Entries[0].Line), MaxLineRunes)
}

// Truncation cuts on a RUNE boundary. Cutting on a byte boundary would split a
// multi-byte character and put invalid UTF-8 into the JSON response.
func TestTruncationDoesNotSplitAMultiByteCharacter(t *testing.T) {
	t.Parallel()

	ring := &fakeRing{
		entries: []logring.Entry{{Line: strings.Repeat("é", MaxLineRunes+10)}},
	}

	got, err := Search(ring, Options{})
	require.NoError(t, err)

	line := got.Entries[0].Line
	assert.True(t, got.Entries[0].Truncated)
	assert.Len(t, []rune(line), MaxLineRunes)
	assert.Equal(t, strings.Repeat("é", MaxLineRunes), line,
		"every retained rune must still be intact")
}

// An entry with no attrs must omit the map rather than emit an empty object.
func TestEntryWithoutAttrsOmitsTheMap(t *testing.T) {
	t.Parallel()

	ring := &fakeRing{entries: []logring.Entry{{Line: "plain"}}}

	got, err := Search(ring, Options{})
	require.NoError(t, err)

	assert.Nil(t, got.Entries[0].Attrs)
}
