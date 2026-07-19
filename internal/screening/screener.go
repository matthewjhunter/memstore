package screening

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/matthewjhunter/airlock/detect"
	"github.com/matthewjhunter/airlock/screen"
)

// Generator is the subset of memstore.Generator this package needs. Declared here
// rather than imported so screening does not depend on the root package, which would
// make the store decorator an import cycle.
type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
	Model() string
}

// verdictMaxTokens caps the screening response.
//
// A verdict is threat, category, a short quote and one sentence -- comfortably under
// 200 tokens, and airlock bounds the free-text fields anyway. The cap is not about
// cost, it is about the tail: an unbounded call lets one rambling generation consume
// the whole timeout and hold a worker slot for a minute, and a scan of a few thousand
// facts hits that often enough to stall. A truncated reply fails to parse and becomes
// a screening failure, which is exactly what the timeout would have produced, sooner.
const verdictMaxTokens = 400

// DefaultTimeout bounds a single screening call.
//
// Screening is not in the synchronous write path. A write lands as
// [OutcomePending] -- durable, and excluded from every read -- and returns
// immediately; an async worker screens it afterwards and flips it to allowed or
// blocked. Nobody is waiting on this call, so it can afford to be patient.
//
// That matters, because patience is what the measurements demanded. At an earlier 5s
// default, four of five screens against a real daemon timed out. Under
// pending-is-invisible semantics that is not a degraded mode, it is an outage: nearly
// every write would sit unreadable waiting for a screen that never finished. Observed
// latency is around 7s per fact, so this leaves substantial headroom for a loaded or
// cold model rather than sitting just above the average.
const DefaultTimeout = 60 * time.Second

// Screener applies a Policy to candidate writes.
//
// The zero Generator is valid: with no generator, every write is screened by regex
// alone and returns OutcomePending, so it is picked up once one is configured.
type Screener struct {
	policy  Policy
	gen     Generator
	timeout time.Duration
	log     *slog.Logger
}

// NewScreener builds a Screener. A nil generator disables the model half; a nil logger
// discards screening logs.
func NewScreener(p Policy, gen Generator, log *slog.Logger) *Screener {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Screener{policy: p, gen: gen, timeout: DefaultTimeout, log: log}
}

// SetTimeout overrides the per-call model timeout. Zero restores the default.
func (s *Screener) SetTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultTimeout
	}
	s.timeout = d
}

// Policy returns the configured policy.
func (s *Screener) Policy() Policy { return s.policy }

// Screen evaluates content and returns the enforcement decision.
//
// It never returns an error. A screening failure is not the caller's problem to
// handle: there is exactly one sensible response to "the model did not answer", and
// it is the fail-open-and-queue behavior encoded here, not something each call site
// should re-decide. Failures are logged and surface as OutcomePending.
func (s *Screener) Screen(ctx context.Context, content string) Decision {
	det := detect.Detect(content)

	// Deliberately no regex short-circuit. An earlier draft skipped the model call
	// when detect scored high, on the theory that regex evidence was already
	// decisive. It is not: detect fires on any text containing a canonical phrasing,
	// so that path rejected a legitimate note about prompt injection with the same
	// confidence as an attack -- and it did so by routing around the exclusions that
	// exist to tell those apart. Every write that reaches a model gets one.
	if s.gen == nil {
		return s.policy.decide(det, screen.Finding{}, false)
	}

	fnd, err := s.modelScreen(ctx, content)
	if err != nil {
		s.log.Warn("screening: model screen unavailable, queuing for re-screen",
			"err", err, "model", s.gen.Model(), "detect_score", det.Score())
		return s.policy.decide(det, screen.Finding{}, false)
	}
	return s.policy.decide(det, fnd, true)
}

// errFabricated marks a verdict whose evidence did not occur in the content.
var errFabricated = errors.New("model cited evidence not present in the content")

// modelScreen renders the screening prompt, calls the generator, and reduces the reply
// to a payload-free finding.
func (s *Screener) modelScreen(ctx context.Context, content string) (screen.Finding, error) {
	p, err := screen.Render(content, screen.Options{Exclusions: Exclusions})
	if err != nil {
		return screen.Finding{}, fmt.Errorf("render prompt: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	var reply string
	if bg, ok := s.gen.(interface {
		GenerateBounded(context.Context, string, int) (string, error)
	}); ok {
		reply, err = bg.GenerateBounded(ctx, p.Text, verdictMaxTokens)
	} else {
		reply, err = s.gen.Generate(ctx, p.Text)
	}
	if err != nil {
		return screen.Finding{}, fmt.Errorf("generate: %w", err)
	}

	// The model must not echo the fence back. If it does, the reply is quoting the
	// content rather than judging it, and parsing it as a verdict is not safe.
	if strings.Contains(reply, p.Nonce) {
		return screen.Finding{}, errors.New("reply echoed the content fence")
	}

	v, err := screen.ParseVerdict(reply)
	if err != nil {
		return screen.Finding{}, fmt.Errorf("parse verdict: %w", err)
	}

	fnd, err := v.Finding(content)
	if err != nil {
		// Finding() refuses to build a record on evidence it could not locate. Treat
		// that as a screening failure rather than a clean result: the model did
		// report something, we just cannot stand behind it. Queuing it means a
		// working screen gets a second look at content that already made one model
		// uneasy.
		//
		// Truncated-but-genuine quotes used to land here too, which meant a real
		// attack quoted at length was written off as fabricated. airlock v0.1.1 fixed
		// that in Locate (airlock#3), so this branch is back to meaning what it says.
		return screen.Finding{}, fmt.Errorf("%w: %v", errFabricated, err)
	}
	return fnd, nil
}
