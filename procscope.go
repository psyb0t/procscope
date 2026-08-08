// Package procscope takes a live pprof profile of the running process and
// returns it as a value, rather than serving it over net/http/pprof.
//
// That difference is the point. Exposing /debug/pprof means opening a port, and
// on a container behind a load balancer the endpoint is usually unreachable
// exactly when you want it. Returning the profile as a struct lets whatever
// channel you already trust — an admin route, an MCP tool, a CLI — hand it back
// without a new listener.
//
//	result, err := procscope.Capture(procscope.Options{Kind: procscope.KindHeap})
//
// Nothing here is enabled at rest: block and mutex profiling stay off until a
// caller asks for a window, and every other kind is a snapshot of state the
// runtime already keeps.
package procscope

import (
	"bytes"
	"encoding/base64"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"sync"
	"time"

	commonerrors "github.com/psyb0t/common-go/errors"
	"github.com/psyb0t/ctxerrors"
)

// Kind selects which profile to take.
//
// The values match the names the Go runtime registers its profiles under, so a
// reader who already knows pprof recognises them — but the named type keeps a
// typo from reaching the runtime as a silent miss.
type Kind string

const (
	// KindHeap is retained memory: which types and callsites are holding it
	// right now. The profile to reach for when chasing a leak.
	KindHeap Kind = "heap"

	// KindAllocs is cumulative allocation since process start — churn rather
	// than retention.
	KindAllocs Kind = "allocs"

	// KindGoroutine is what every goroutine is currently doing. With
	// FormatText and DetailGrouped it is the goroutine-leak finder.
	KindGoroutine Kind = "goroutine"

	// KindThreadCreate is what created OS threads.
	KindThreadCreate Kind = "threadcreate"

	// KindBlock is where goroutines blocked. Off by default — see Options.Window.
	KindBlock Kind = "block"

	// KindMutex is where lock contention happened. Off by default — see
	// Options.Window.
	KindMutex Kind = "mutex"

	// KindCPU samples what burned CPU across a window. Blocks for that window.
	KindCPU Kind = "cpu"

	// KindTrace is a full execution trace for `go tool trace`. Blocks for the
	// window.
	KindTrace Kind = "trace"
)

// Format selects how the profile comes back.
//
// This and Detail used to be a single `debug int` inherited from
// runtime/pprof.Profile.WriteTo, where 0 meant protobuf and anything higher
// meant text AND picked a verbosity. Those are two unrelated decisions, so they
// are two fields.
type Format string

const (
	// FormatProto returns the gzipped protobuf `go tool pprof` reads,
	// base64-encoded because it is binary. The default, and what you want
	// unless a human is reading the bytes directly.
	FormatProto Format = "proto"

	// FormatText returns a human-readable dump. NOT consumable by
	// `go tool pprof`. Only the snapshot kinds support it — cpu and trace have
	// no text form.
	FormatText Format = "text"
)

// Detail selects how much text FormatText produces. FormatProto ignores it,
// having only one representation.
type Detail string

const (
	// DetailGrouped collapses identical stacks and reports a count for each.
	// For KindGoroutine this is what finds a leak — one stack with 40,000
	// goroutines behind it. The default for FormatText.
	DetailGrouped Detail = "grouped"

	// DetailFull prints every stack individually, ungrouped. Large, and worth
	// it only when you need one specific goroutine rather than the shape of
	// the pile.
	DetailFull Detail = "full"
)

// Encoding describes how to read Result.Profile.
type Encoding string

const (
	// EncodingBase64Gzip is base64 of a gzipped pprof protobuf.
	EncodingBase64Gzip Encoding = "base64+gzip"

	// EncodingText is the payload as-is, already human-readable.
	EncodingText Encoding = "text"
)

const (
	// DefaultWindow is the capture window when a windowed kind is asked for
	// without one.
	DefaultWindow = 5 * time.Second

	// MaxWindow bounds the window so a single call cannot hold a request open
	// indefinitely.
	MaxWindow = 30 * time.Second
)

// The debug levels runtime/pprof.Profile.WriteTo takes. Unexported on purpose:
// this integer is the thing the package exists to stop leaking to callers.
const (
	pprofDebugProto   = 0
	pprofDebugGrouped = 1
	pprofDebugFull    = 2
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
	noteProto = "Base64 of a gzipped pprof protobuf. Consume with: `echo <profile> | base64 -d > prof.pb.gz && go tool pprof prof.pb.gz` (add `-http=:` for a flame graph). Kind=heap answers 'which type or callsite holds the retained memory'; inuse_space is the default heap sample."
	//nolint:lll
	noteText = "Human-readable text dump — NOT consumable by `go tool pprof`. For Kind=goroutine, DetailGrouped collapses identical stacks with a count (this is what finds a leak) and DetailFull prints every stack. Use FormatProto for something go tool pprof can read."
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

// Options is a capture request. The zero value is a heap profile in protobuf
// form, which is the most common thing to want.
type Options struct {
	// Kind selects the profile. Empty means KindHeap.
	Kind Kind

	// Format selects protobuf or text. Empty means FormatProto.
	Format Format

	// Detail selects how verbose FormatText is. Empty means DetailGrouped.
	// Ignored by FormatProto.
	Detail Detail

	// Window is how long to capture for.
	//
	// KindCPU and KindTrace always need one and block for it; <= 0 uses
	// DefaultWindow, and anything above MaxWindow is clamped to it.
	//
	// For KindBlock and KindMutex it does something different and important:
	// both profilers are OFF in every Go process, so a Window > 0 ENABLES the
	// profiler for that long and disables it afterwards. With no Window they
	// return only whatever already accumulated, which on a process that never
	// enabled them is nothing.
	//
	// The snapshot kinds ignore it — they read state the runtime already has.
	Window time.Duration
}

// Result is one captured profile plus how to decode it.
//
//nolint:tagliatelle // snake_case: this payload is read by tools and models
type Result struct {
	Kind        Kind          `json:"kind"`
	Format      Format        `json:"format"`
	Detail      Detail        `json:"detail,omitempty"`
	Window      time.Duration `json:"window,omitempty"`
	Encoding    Encoding      `json:"encoding"`
	Profile     string        `json:"profile"`
	GeneratedAt string        `json:"generated_at"`
	Note        string        `json:"note"`
}

// Capture takes a profile of the running process.
//
// Snapshot kinds (heap, allocs, goroutine, threadcreate) return immediately.
// KindCPU and KindTrace block for Options.Window. KindBlock and KindMutex are
// off by default — see Options.Window for how they arm themselves.
//
// An unrecognised Kind, Format or Detail is an error rather than a silent
// fallback: a typo that quietly returns a heap profile is worse than one that
// says so.
func Capture(opts Options) (Result, error) {
	opts, err := opts.resolve()
	if err != nil {
		return Result{}, err
	}

	switch opts.Kind {
	case KindCPU:
		return captureCPU(opts)
	case KindTrace:
		return captureTrace(opts)
	case KindBlock, KindMutex:
		if opts.Window > 0 {
			return captureWindowed(opts)
		}

		return captureLookup(opts)
	case KindHeap, KindGoroutine, KindAllocs, KindThreadCreate:
		return captureLookup(opts)
	default:
		return Result{}, ctxerrors.Wrapf(
			commonerrors.ErrInvalidValue, "unknown profile kind %q", opts.Kind,
		)
	}
}

// resolve fills the defaults and rejects anything unrecognised, so every path
// below works with a fully specified request.
func (o Options) resolve() (Options, error) {
	o.applyDefaults()

	if err := o.validate(); err != nil {
		return o, err
	}

	return o, nil
}

// applyDefaults fills the zero value into the common case: a heap profile in
// protobuf form.
func (o *Options) applyDefaults() {
	if o.Kind == "" {
		o.Kind = KindHeap
	}

	if o.Format == "" {
		o.Format = FormatProto
	}

	if o.Detail == "" {
		o.Detail = DetailGrouped
	}
}

// validate rejects anything unrecognised. A typo that quietly returns a heap
// profile is worse than one that says so.
func (o Options) validate() error {
	if o.Format != FormatProto && o.Format != FormatText {
		return ctxerrors.Wrapf(
			commonerrors.ErrInvalidValue, "unknown format %q", o.Format,
		)
	}

	if o.Detail != DetailGrouped && o.Detail != DetailFull {
		return ctxerrors.Wrapf(
			commonerrors.ErrInvalidValue, "unknown detail %q", o.Detail,
		)
	}

	// cpu and trace have no text form, so honouring the request would produce a
	// Result whose Format contradicted its Encoding.
	if o.Format == FormatText && (o.Kind == KindCPU || o.Kind == KindTrace) {
		return ctxerrors.Wrapf(
			commonerrors.ErrInvalidValue,
			"kind %q has no text form, use FormatProto", o.Kind,
		)
	}

	return nil
}

// pprofDebug maps the resolved Format and Detail onto the single integer
// runtime/pprof takes. This function is the entire reason that integer is not
// part of the package's API.
func (o Options) pprofDebug() int {
	if o.Format == FormatProto {
		return pprofDebugProto
	}

	if o.Detail == DetailFull {
		return pprofDebugFull
	}

	return pprofDebugGrouped
}

// captureLookup writes a runtime profile snapshot.
func captureLookup(opts Options) (Result, error) {
	profile := pprof.Lookup(string(opts.Kind))
	if profile == nil {
		return Result{}, ctxerrors.Wrapf(
			commonerrors.ErrNotFound,
			"pprof profile %q is not registered", opts.Kind,
		)
	}

	var buf bytes.Buffer
	if err := profile.WriteTo(&buf, opts.pprofDebug()); err != nil {
		return Result{}, ctxerrors.Wrap(err, "write pprof profile")
	}

	return encode(opts, buf.Bytes()), nil
}

// captureWindowed temporarily enables block or mutex profiling, waits for the
// window to accumulate events, captures, then restores the prior state.
func captureWindowed(opts Options) (Result, error) {
	opts.Window = clampWindow(opts.Window)

	weEnabledBlock, previousMutexFraction := enableProfiling(opts.Kind)
	defer disableProfiling(opts.Kind, weEnabledBlock, previousMutexFraction)

	time.Sleep(opts.Window)

	result, err := captureLookup(opts)
	if err != nil {
		return Result{}, err
	}

	result.Window = opts.Window
	result.Note += noteBlockMutexCumulative

	return result, nil
}

// enableProfiling turns the requested profiler on and returns how to undo it:
// the bool is true only when THIS call set a nonzero block rate, and the int is
// the mutex fraction that preceded this call, restored verbatim so an
// already-enabled mutex profile is left alone.
func enableProfiling(kind Kind) (bool, int) {
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
	// Every other kind reads state the runtime already keeps, so there is
	// nothing to switch on for it.
	case KindHeap, KindAllocs, KindGoroutine, KindThreadCreate,
		KindCPU, KindTrace:
		return false, 0
	default:
		return false, 0
	}
}

// disableProfiling restores the prior state. Block resets to 0 only if this
// call was the one that enabled it; mutex always restores what preceded it.
func disableProfiling(kind Kind, weEnabledBlock bool, previousMutexFraction int) {
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
	// Nothing was enabled for the other kinds, so there is nothing to restore.
	case KindHeap, KindAllocs, KindGoroutine, KindThreadCreate,
		KindCPU, KindTrace:
	}
}

// captureCPU runs a CPU profile for the clamped window.
func captureCPU(opts Options) (Result, error) {
	opts.Window = clampWindow(opts.Window)

	var buf bytes.Buffer
	if err := pprof.StartCPUProfile(&buf); err != nil {
		return Result{}, ctxerrors.Wrap(err, "start cpu profile")
	}

	time.Sleep(opts.Window)
	pprof.StopCPUProfile()

	return encode(opts, buf.Bytes()), nil
}

// captureTrace runs an execution trace for the clamped window.
func captureTrace(opts Options) (Result, error) {
	opts.Window = clampWindow(opts.Window)

	var buf bytes.Buffer
	if err := trace.Start(&buf); err != nil {
		return Result{}, ctxerrors.Wrap(err, "start trace")
	}

	time.Sleep(opts.Window)
	trace.Stop()

	return encode(opts, buf.Bytes()), nil
}

// encode wraps raw profile bytes: text passes through, proto is base64-encoded
// because it is gzipped binary and would not survive a JSON string otherwise.
func encode(opts Options, raw []byte) Result {
	result := Result{
		Kind:        opts.Kind,
		Format:      opts.Format,
		Window:      opts.Window,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if opts.Format == FormatText {
		result.Detail = opts.Detail
		result.Encoding = EncodingText
		result.Profile = string(raw)
		result.Note = noteText

		return result
	}

	// Detail is left empty for proto: reporting a value that had no effect
	// would suggest it had one.
	result.Encoding = EncodingBase64Gzip
	result.Profile = base64.StdEncoding.EncodeToString(raw)
	result.Note = noteProto

	return result
}

// clampWindow bounds the capture window, defaulting when unset.
func clampWindow(window time.Duration) time.Duration {
	if window <= 0 {
		return DefaultWindow
	}

	return min(window, MaxWindow)
}
