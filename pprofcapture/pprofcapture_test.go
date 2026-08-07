package pprofcapture

import (
	"encoding/base64"
	"runtime"
	"strings"
	"sync"
	"testing"

	commonerrors "github.com/psyb0t/common-go/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gzipMagic is the first two bytes of a gzip stream. A debug=0 profile claims
// to be a gzipped protobuf, so this is what proves the claim rather than
// trusting the Encoding field the same code set.
var gzipMagic = []byte{0x1f, 0x8b} //nolint:gochecknoglobals // test fixture

func TestCaptureSnapshotKinds(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		kind string
	}{
		{name: "heap", kind: KindHeap},
		{name: "goroutine", kind: KindGoroutine},
		{name: "allocs", kind: KindAllocs},
		{name: "threadcreate", kind: KindThreadcreate},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Capture(tc.kind, DebugProto, 0)
			require.NoError(t, err)

			assert.Equal(t, tc.kind, got.Kind)
			assert.Equal(t, EncodingBase64Gzip, got.Encoding)
			assert.NotEmpty(t, got.GeneratedAt)

			raw, err := base64.StdEncoding.DecodeString(got.Profile)
			require.NoError(t, err, "debug=0 must be valid base64")
			assert.True(t, strings.HasPrefix(string(raw), string(gzipMagic)),
				"debug=0 must decode to a gzip stream")
		})
	}
}

// An empty kind must mean heap — a caller chasing a leak reaches for retained
// memory, and defaulting to anything else silently answers a question nobody
// asked.
func TestEmptyKindDefaultsToHeap(t *testing.T) {
	t.Parallel()

	got, err := Capture("", DebugProto, 0)
	require.NoError(t, err)
	assert.Equal(t, KindHeap, got.Kind)
}

func TestKindIsCaseAndSpaceInsensitive(t *testing.T) {
	t.Parallel()

	got, err := Capture("  HEAP  ", DebugProto, 0)
	require.NoError(t, err)
	assert.Equal(t, KindHeap, got.Kind)
}

// debug>=1 is a different payload entirely — readable text, not a proto. A
// caller that fed it to `go tool pprof` would get a parse error, so the
// Encoding and Note have to change with it.
func TestTextDebugLevelsReturnReadableText(t *testing.T) {
	t.Parallel()

	got, err := Capture(KindGoroutine, DebugMax, 0)
	require.NoError(t, err)

	assert.Equal(t, EncodingText, got.Encoding)
	assert.Contains(t, got.Profile, "goroutine")
	assert.Contains(t, got.Note, "NOT consumable")
}

func TestDebugIsClamped(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		debug int
		want  int
	}{
		{name: "negative clamps to proto", debug: -5, want: DebugProto},
		{name: "in range is kept", debug: 1, want: 1},
		{name: "above max clamps down", debug: 99, want: DebugMax},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Capture(KindGoroutine, tc.debug, 0)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Debug)
		})
	}
}

// cpu and trace are the windowed kinds: they block for the window and have no
// text form, so debug is ignored and the payload is always a proto.
func TestWindowedKinds(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		kind string
	}{
		{name: "cpu", kind: KindCPU},
		{name: "trace", kind: KindTrace},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// debug=2 is passed deliberately: a windowed kind must ignore it
			// rather than claim a text encoding it cannot produce.
			got, err := Capture(tc.kind, DebugMax, 1)
			require.NoError(t, err)

			assert.Equal(t, tc.kind, got.Kind)
			assert.Equal(t, 1, got.Seconds)
			assert.Equal(t, EncodingBase64Gzip, got.Encoding)
			assert.Equal(t, DebugProto, got.Debug)

			raw, err := base64.StdEncoding.DecodeString(got.Profile)
			require.NoError(t, err)
			assert.NotEmpty(t, raw)
		})
	}
}

func TestUnknownKindIsAnInvalidValue(t *testing.T) {
	t.Parallel()

	_, err := Capture("nonsense", DebugProto, 0)

	require.Error(t, err)
	require.ErrorIs(t, err, commonerrors.ErrInvalidValue)
	assert.Contains(t, err.Error(), "nonsense")
}

func TestClampSeconds(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		seconds int
		want    int
	}{
		{name: "zero takes the default", seconds: 0, want: DefaultSeconds},
		{name: "negative takes the default", seconds: -1, want: DefaultSeconds},
		{name: "in range is kept", seconds: 3, want: 3},
		{name: "above max clamps down", seconds: 9999, want: MaxSeconds},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, clampSeconds(tc.seconds))
		})
	}
}

// block/mutex profiling must cost nothing at rest. A zero-second call takes the
// snapshot path, which must NOT switch profiling on — otherwise merely reading
// the profile would leave the process paying for it forever.
func TestZeroSecondBlockCaptureDoesNotEnableProfiling(t *testing.T) {
	testCases := []struct {
		name string
		kind string
	}{
		{name: "block", kind: KindBlock},
		{name: "mutex", kind: KindMutex},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Capture(tc.kind, DebugProto, 0)
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

	_, err := Capture(KindMutex, DebugProto, 1)
	require.NoError(t, err)

	after := runtime.SetMutexProfileFraction(-1)
	assert.Equal(t, before, after, "mutex fraction must be restored")
}

// Both knobs are process-global, so concurrent windowed captures race unless
// they serialise. Run under -race; the assertion is that nothing is left on.
func TestConcurrentWindowedCapturesAreSerialised(t *testing.T) {
	var wg sync.WaitGroup

	for range 4 {

		wg.Go(func() {

			_, err := Capture(KindBlock, DebugProto, 1)
			assert.NoError(t, err)
		})
	}

	wg.Wait()

	profileLock.Lock()
	owned := blockProfileOwned
	profileLock.Unlock()

	// runtime.SetBlockProfileRate has no getter, so the ownership flag is the
	// only observable proof the rate was handed back — which is the same reason
	// the package tracks it in the first place.
	assert.False(t, owned, "every window must have been disabled again")
}
