// Package pprofcapture takes a live pprof profile of the running process and
// returns it as a value, rather than serving it over net/http/pprof.
//
// That difference is the point. Exposing /debug/pprof means opening a port,
// and on a container behind a load balancer the endpoint is usually
// unreachable exactly when you want it. Returning the profile as a struct lets
// whatever channel you already trust — an admin API, an MCP tool, a CLI — hand
// it back without a new listener.
//
// Nothing here is enabled at rest: block and mutex profiling stay off until a
// caller asks for a window, and every other kind is a snapshot of state the
// runtime already keeps.
package pprofcapture

import (
	"bytes"
	"encoding/base64"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"sync"
	"time"

	commonerrors "github.com/psyb0t/common-go/errors"
	"github.com/psyb0t/ctxerrors"
)

// Profile kinds. The first six are runtime profiles served by pprof.Lookup;
// KindCPU and KindTrace are time-windowed captures.
const (
	KindHeap         = "heap"
	KindGoroutine    = "goroutine"
	KindAllocs       = "allocs"
	KindBlock        = "block"
	KindMutex        = "mutex"
	KindThreadcreate = "threadcreate"
	KindCPU          = "cpu"
	KindTrace        = "trace"
)

const (
	// DebugProto writes the gzipped protobuf `go tool pprof` consumes.
	DebugProto = 0

	// DebugMax is the most verbose text form — full goroutine stacks.
	DebugMax = 2

	// DefaultSeconds is the capture window when a windowed kind is asked for
	// without one.
	DefaultSeconds = 5

	// MaxSeconds bounds the window so a single call cannot hold a request open
	// indefinitely.
	MaxSeconds = 30
)

// Encodings reported on Result.
const (
	EncodingBase64Gzip = "base64+gzip"
	EncodingText       = "text"
)

const (
	// blockProfileRateNS samples roughly one blocking event per 10µs blocked.
	blockProfileRateNS = 10000

	// mutexFraction samples every contention event.
	mutexFraction = 1
)

// Notes stamped onto a Result so whoever reads it — a human or a model — knows
// how to consume the payload instead of guessing.
const (
	//nolint:lll
	noteProto = "Base64 of a gzipped pprof protobuf. Consume with: `echo <profile> | base64 -d > prof.pb.gz && go tool pprof prof.pb.gz` (add `-http=:` for a flame graph). kind=heap answers 'which type or callsite holds the retained memory'; inuse_space is the default heap sample."
	//nolint:lll
	noteText = "Human-readable text dump (debug>=1) — NOT consumable by `go tool pprof`. For goroutine, debug=2 gives full stacks and debug=1 groups identical stacks by count, which is what finds a goroutine leak. Use debug=0 for a proto."
	//nolint:lll
	noteBlockMutexCumulative = " NOTE: block/mutex profiling has no reset API — the buffer accumulates across every enable-window since process start, not just this call's window. Diff two captures to see what happened between them."
)

// profileLock serialises block/mutex enable-capture-disable windows.
// runtime.SetBlockProfileRate and SetMutexProfileFraction are process-global,
// so two concurrent captures must not race each other's enable and disable.
//
//nolint:gochecknoglobals // guards process-global runtime settings
var profileLock sync.Mutex

// blockProfileOwned records whether THIS package currently owns a nonzero block
// profile rate. runtime.SetBlockProfileRate has no getter, so this is the only
// way to know we were the one that turned it on — and therefore the only one
// allowed to turn it back off. Guarded by profileLock.
//
//nolint:gochecknoglobals // paired with profileLock above
var blockProfileOwned bool

// Result is one captured profile plus how to decode it.
//
//nolint:tagliatelle // snake_case: this payload is read by tools and models
type Result struct {
	Kind        string `json:"kind"`
	Debug       int    `json:"debug"`
	Seconds     int    `json:"seconds,omitempty"`
	Encoding    string `json:"encoding"`
	Profile     string `json:"profile"`
	GeneratedAt string `json:"generated_at"`
	Note        string `json:"note"`
}

// Capture takes a profile of the running process.
//
// Snapshot kinds (heap, goroutine, allocs, threadcreate) return immediately.
// KindCPU and KindTrace block for seconds. KindBlock and KindMutex are off by
// default, so with seconds <= 0 they return only whatever has already
// accumulated — usually nothing; pass seconds > 0 to enable profiling for that
// window and disable it afterwards.
//
// debug selects the encoding for snapshot kinds: 0 yields the gzipped protobuf
// (base64-encoded, since it is binary), >= 1 yields text. It is ignored for
// cpu and trace, which have no text form.
//
// An empty kind means KindHeap — the retained-memory profile, which is what a
// caller chasing a leak wants and the least surprising default.
func Capture(kind string, debug int, seconds int) (Result, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		kind = KindHeap
	}

	debug = min(max(debug, DebugProto), DebugMax)

	switch kind {
	case KindCPU:
		return captureCPU(seconds)
	case KindTrace:
		return captureTrace(seconds)
	case KindBlock, KindMutex:
		if seconds > 0 {
			return captureWindowed(kind, debug, seconds)
		}

		return captureLookup(kind, debug)
	case KindHeap, KindGoroutine, KindAllocs, KindThreadcreate:
		return captureLookup(kind, debug)
	default:
		return Result{}, ctxerrors.Wrap(
			commonerrors.ErrInvalidValue, "unknown pprof kind: "+kind,
		)
	}
}

// captureLookup writes a runtime profile snapshot at the given debug level.
func captureLookup(kind string, debug int) (Result, error) {
	profile := pprof.Lookup(kind)
	if profile == nil {
		return Result{}, ctxerrors.Wrap(
			commonerrors.ErrNotFound, "pprof profile not registered: "+kind,
		)
	}

	var buf bytes.Buffer
	if err := profile.WriteTo(&buf, debug); err != nil {
		return Result{}, ctxerrors.Wrap(err, "write pprof profile")
	}

	return encode(kind, debug, 0, buf.Bytes()), nil
}

// captureWindowed temporarily enables block or mutex profiling, waits for the
// window to accumulate events, captures, then restores the prior state.
func captureWindowed(kind string, debug int, seconds int) (Result, error) {
	seconds = clampSeconds(seconds)

	weEnabledBlock, previousMutexFraction := enableProfiling(kind)
	defer disableProfiling(kind, weEnabledBlock, previousMutexFraction)

	time.Sleep(time.Duration(seconds) * time.Second)

	result, err := captureLookup(kind, debug)
	if err != nil {
		return Result{}, err
	}

	result.Seconds = seconds
	result.Note += noteBlockMutexCumulative

	return result, nil
}

// enableProfiling turns the requested profiler on and returns how to undo it:
// the bool is true only when THIS call set a nonzero block rate, and the int is
// the mutex fraction that preceded this call, restored verbatim so an
// already-enabled mutex profile is left alone.
func enableProfiling(kind string) (bool, int) {
	profileLock.Lock()
	defer profileLock.Unlock()

	switch kind {
	case KindBlock:
		if blockProfileOwned {
			return false, 0
		}

		blockProfileOwned = true

		runtime.SetBlockProfileRate(blockProfileRateNS)

		return true, 0
	case KindMutex:
		return false, runtime.SetMutexProfileFraction(mutexFraction)
	default:
		return false, 0
	}
}

// disableProfiling restores the prior state. Block resets to 0 only if this
// call was the one that enabled it; mutex always restores what preceded it.
func disableProfiling(kind string, weEnabledBlock bool, previousMutexFraction int) {
	profileLock.Lock()
	defer profileLock.Unlock()

	switch kind {
	case KindBlock:
		if !weEnabledBlock {
			return
		}

		runtime.SetBlockProfileRate(0)

		blockProfileOwned = false
	case KindMutex:
		runtime.SetMutexProfileFraction(previousMutexFraction)
	}
}

// captureCPU runs a CPU profile for the clamped window.
func captureCPU(seconds int) (Result, error) {
	seconds = clampSeconds(seconds)

	var buf bytes.Buffer
	if err := pprof.StartCPUProfile(&buf); err != nil {
		return Result{}, ctxerrors.Wrap(err, "start cpu profile")
	}

	time.Sleep(time.Duration(seconds) * time.Second)
	pprof.StopCPUProfile()

	return encode(KindCPU, DebugProto, seconds, buf.Bytes()), nil
}

// captureTrace runs an execution trace for the clamped window.
func captureTrace(seconds int) (Result, error) {
	seconds = clampSeconds(seconds)

	var buf bytes.Buffer
	if err := trace.Start(&buf); err != nil {
		return Result{}, ctxerrors.Wrap(err, "start trace")
	}

	time.Sleep(time.Duration(seconds) * time.Second)
	trace.Stop()

	return encode(KindTrace, DebugProto, seconds, buf.Bytes()), nil
}

// encode wraps raw profile bytes: text passes through, proto is base64-encoded
// because it is gzipped binary and would not survive a JSON string otherwise.
func encode(kind string, debug int, seconds int, raw []byte) Result {
	result := Result{
		Kind:        kind,
		Debug:       debug,
		Seconds:     seconds,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if debug > DebugProto {
		result.Encoding = EncodingText
		result.Profile = string(raw)
		result.Note = noteText

		return result
	}

	result.Encoding = EncodingBase64Gzip
	result.Profile = base64.StdEncoding.EncodeToString(raw)
	result.Note = noteProto

	return result
}

// clampSeconds bounds the capture window, defaulting when unset.
func clampSeconds(seconds int) int {
	if seconds <= 0 {
		return DefaultSeconds
	}

	return min(seconds, MaxSeconds)
}
