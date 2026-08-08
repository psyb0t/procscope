package procscope

import (
	"encoding/base64"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	commonerrors "github.com/psyb0t/common-go/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gzipMagic is the first two bytes of a gzip stream. FormatProto claims to be a
// gzipped protobuf, so this proves the claim rather than trusting the Encoding
// field the same code set.
var gzipMagic = "\x1f\x8b" //nolint:gochecknoglobals // test fixture

func TestCaptureSnapshotKinds(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		kind Kind
	}{
		{name: "heap", kind: KindHeap},
		{name: "goroutine", kind: KindGoroutine},
		{name: "allocs", kind: KindAllocs},
		{name: "threadcreate", kind: KindThreadCreate},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Capture(Options{Kind: tc.kind})
			require.NoError(t, err)

			assert.Equal(t, tc.kind, got.Kind)
			assert.Equal(t, FormatProto, got.Format)
			assert.Equal(t, EncodingBase64Gzip, got.Encoding)
			assert.NotEmpty(t, got.GeneratedAt)

			// Detail must NOT be reported for proto — echoing a value that had
			// no effect would suggest it had one.
			assert.Empty(t, got.Detail)

			raw, err := base64.StdEncoding.DecodeString(got.Profile)
			require.NoError(t, err, "FormatProto must be valid base64")
			assert.True(t, strings.HasPrefix(string(raw), gzipMagic),
				"FormatProto must decode to a gzip stream")
		})
	}
}

// The zero Options is the common case: a heap profile in protobuf form.
func TestZeroOptionsIsHeapProto(t *testing.T) {
	t.Parallel()

	got, err := Capture(Options{})
	require.NoError(t, err)

	assert.Equal(t, KindHeap, got.Kind)
	assert.Equal(t, FormatProto, got.Format)
	assert.Equal(t, EncodingBase64Gzip, got.Encoding)
}

// FormatText is a different payload entirely, and Detail selects how much of
// it. Both have to reach the runtime as the right debug level.
func TestFormatTextDetailLevels(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		detail Detail
		want   Detail
	}{
		{name: "grouped", detail: DetailGrouped, want: DetailGrouped},
		{name: "full", detail: DetailFull, want: DetailFull},
		{name: "empty defaults to grouped", detail: "", want: DetailGrouped},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Capture(Options{
				Kind:   KindGoroutine,
				Format: FormatText,
				Detail: tc.detail,
			})
			require.NoError(t, err)

			assert.Equal(t, EncodingText, got.Encoding)
			assert.Equal(t, tc.want, got.Detail)
			assert.Contains(t, got.Profile, "goroutine")
			assert.Contains(t, got.Note, "NOT consumable")
		})
	}
}

// Format and Detail are separate axes now. This pins the mapping onto the
// single runtime/pprof debug integer they replaced — the whole reason that
// integer is no longer in the API.
func TestPprofDebugMapping(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		opts Options
		want int
	}{
		{
			name: "proto ignores detail",
			opts: Options{Format: FormatProto, Detail: DetailFull},
			want: pprofDebugProto,
		},
		{
			name: "text grouped",
			opts: Options{Format: FormatText, Detail: DetailGrouped},
			want: pprofDebugGrouped,
		},
		{
			name: "text full",
			opts: Options{Format: FormatText, Detail: DetailFull},
			want: pprofDebugFull,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tc.opts.pprofDebug())
		})
	}
}

// A typo must be an error, not a quiet heap profile. Every rejection carries
// the offending value so the caller can see what they typed.
func TestUnrecognisedValuesAreRejected(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		opts     Options
		contains string
	}{
		{
			name:     "unknown kind",
			opts:     Options{Kind: "nonsense"},
			contains: "nonsense",
		},
		{
			name:     "unknown format",
			opts:     Options{Format: "yaml"},
			contains: "yaml",
		},
		{
			name:     "unknown detail",
			opts:     Options{Format: FormatText, Detail: "verbose"},
			contains: "verbose",
		},
		{
			name:     "text has no meaning for cpu",
			opts:     Options{Kind: KindCPU, Format: FormatText},
			contains: "no text form",
		},
		{
			name:     "text has no meaning for trace",
			opts:     Options{Kind: KindTrace, Format: FormatText},
			contains: "no text form",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Capture(tc.opts)

			require.Error(t, err)
			require.ErrorIs(t, err, commonerrors.ErrInvalidValue)
			assert.Contains(t, err.Error(), tc.contains)
		})
	}
}

func TestClampWindow(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		window time.Duration
		want   time.Duration
	}{
		{name: "zero takes the default", window: 0, want: DefaultWindow},
		{
			name:   "negative takes the default",
			window: -time.Second,
			want:   DefaultWindow,
		},
		{name: "in range is kept", window: 3 * time.Second, want: 3 * time.Second},
		{name: "above max clamps down", window: time.Hour, want: MaxWindow},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, clampWindow(tc.window))
		})
	}
}

// cpu and trace block for the window and have no text form, so Format is always
// proto and the resolved Window comes back on the Result.
func TestWindowedKinds(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		kind Kind
	}{
		{name: "cpu", kind: KindCPU},
		{name: "trace", kind: KindTrace},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Capture(Options{Kind: tc.kind, Window: time.Second})
			require.NoError(t, err)

			assert.Equal(t, tc.kind, got.Kind)
			assert.Equal(t, time.Second, got.Window)
			assert.Equal(t, FormatProto, got.Format)
			assert.Equal(t, EncodingBase64Gzip, got.Encoding)

			raw, err := base64.StdEncoding.DecodeString(got.Profile)
			require.NoError(t, err)
			assert.NotEmpty(t, raw)
		})
	}
}

// block/mutex profiling must cost nothing at rest. With no Window the snapshot
// path runs, which must NOT switch profiling on — otherwise merely reading the
// profile would leave the process paying for it forever.
func TestZeroWindowDoesNotEnableProfiling(t *testing.T) {
	testCases := []struct {
		name string
		kind Kind
	}{
		{name: "block", kind: KindBlock},
		{name: "mutex", kind: KindMutex},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Capture(Options{Kind: tc.kind})
			require.NoError(t, err)

			profileLock.Lock()
			owned := blockProfileOwned
			profileLock.Unlock()

			assert.False(t, owned,
				"a snapshot read must not leave profiling enabled")
		})
	}
}

// Enabling and restoring mutex profiling must be symmetric: a capture that
// leaked a nonzero fraction would keep sampling every contention event for the
// life of the process.
func TestMutexFractionIsRestored(t *testing.T) {
	before := runtime.SetMutexProfileFraction(-1)

	_, err := Capture(Options{Kind: KindMutex, Window: time.Second})
	require.NoError(t, err)

	after := runtime.SetMutexProfileFraction(-1)
	assert.Equal(t, before, after, "mutex fraction must be restored")
}

// A windowed block/mutex capture reports the window it actually used, and warns
// that the buffer is cumulative rather than scoped to that window.
func TestWindowedBlockCaptureReportsItsWindow(t *testing.T) {
	got, err := Capture(Options{Kind: KindBlock, Window: time.Second})
	require.NoError(t, err)

	assert.Equal(t, time.Second, got.Window)
	assert.Contains(t, got.Note, "no reset API")
}

// Both knobs are process-global, so concurrent windowed captures race unless
// they serialise. Run under -race; the assertion is that nothing is left on.
func TestConcurrentWindowedCapturesAreSerialised(t *testing.T) {
	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			_, err := Capture(Options{Kind: KindBlock, Window: time.Second})
			assert.NoError(t, err)
		})
	}

	wg.Wait()

	profileLock.Lock()
	owned := blockProfileOwned
	profileLock.Unlock()

	// runtime.SetBlockProfileRate has no getter, so the ownership flag is the
	// only observable proof the rate was handed back — the same reason the
	// package tracks it at all.
	assert.False(t, owned, "every window must have been disabled again")
}
