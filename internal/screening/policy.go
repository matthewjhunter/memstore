// Package screening decides whether a write may enter the store.
//
// It combines airlock's two detectors and turns their output into a single
// enforcement decision. Neither detector is trusted alone:
//
//   - [detect] is keyword-anchored regex over normalized text. It is fast, free, and
//     defeated completely by paraphrase. A clean detect result proves nothing.
//   - [screen] asks a model whether the text is addressed to an AI, and requires a
//     verbatim quote as evidence. It catches novel phrasings, costs a model call, and
//     is only available when a generator is configured and reachable.
//
// The structural protection is elsewhere: internal/fence marks stored content as data
// wherever it reaches a model, and holds whether or not anything here fires. This
// package is the second line -- it keeps the worst content out of the store in the
// first place, so the fence is not the only thing standing between a poisoned write
// and a future session.
//
// # Where it runs
//
// Daemon-side, wrapping the store, so every write path is covered: the HTTP API, the
// extraction pipeline that mints facts from transcripts without a human in the loop,
// and anything added later. A control that each new call site has to remember to
// invoke is a control that will eventually be missed.
package screening

import (
	"github.com/matthewjhunter/airlock/detect"
	"github.com/matthewjhunter/airlock/screen"
)

// Outcome is what the policy decided about a write.
type Outcome string

const (
	// OutcomeAllowed means the write proceeds and was fully screened.
	OutcomeAllowed Outcome = "allowed"

	// OutcomeBlocked means the write was rejected.
	OutcomeBlocked Outcome = "blocked"

	// OutcomePending means the write proceeded on regex evidence alone because the
	// model screen could not run, and is queued to be screened again later.
	OutcomePending Outcome = "pending"
)

// Policy is the enforcement configuration.
//
// The thresholds are the only knobs, and they are deliberately few. Every additional
// dial is another way to end up in a state nobody intended and nobody can describe.
type Policy struct {
	// BlockThreat is the lowest screen threat score (0-10) that blocks a write.
	// A verdict below it is recorded and allowed.
	//
	// The model verdict is the only blocking signal. detect's score is recorded as
	// corroboration but never rejects a write on its own -- see the note on
	// [Decision.DetectScore] for why that would be actively harmful here.
	BlockThreat int

	// Enforce turns blocking on. With Enforce false the policy screens and records
	// exactly as it would otherwise but never rejects a write, so a deployment can
	// measure its false-positive rate before anything is lost.
	Enforce bool
}

// DefaultPolicy blocks on a verified model verdict at the midpoint of the threat
// scale.
//
// Threat 6 is a judgment call, not a calibrated figure -- airlock does not claim its
// scale is calibrated and neither does this. It sits above the range where the model
// reports unease it cannot pin to a span (those verdicts fail evidence verification
// and never produce a finding at all) and below the top of the scale, so an
// unambiguous instruction aimed at a model does not need to be maximally florid to be
// caught.
func DefaultPolicy() Policy {
	return Policy{
		BlockThreat: 6,
		Enforce:     true,
	}
}

// Exclusions are memstore's domain-specific "this is NOT an injection" rules, appended
// to screen's generic list.
//
// These exist because memstore's corpus is unusual: it is largely notes about building
// software, including notes about prompt injection, agent instructions, and this very
// package. A screener that flags a fact for describing an attack would make the store
// unable to remember its own security work -- and that failure mode is not
// hypothetical, it is the first thing this corpus will produce.
var Exclusions = []string{
	"Notes, designs, or postmortems ABOUT prompt injection, jailbreaks, or AI security",
	"Stored instructions the user wrote for their own agents, tools, or workflows",
	"Coding conventions and repo rules phrased as imperatives to a developer",
	"Quoted attack strings appearing in test fixtures, security notes, or bug reports",
	"Descriptions of what a tool, agent, or command does when invoked",
}

// Decision is the outcome of screening one write, plus the payload-free evidence
// behind it.
//
// It carries no attacker-authored bytes: not the quoted evidence, not the model's
// prose. Those would be a second delivery path into logs, dashboards, and error
// strings, none of which are fenced. See [screen.Finding] for the full argument. The
// content itself is held separately, in quarantine, where reads are fenced.
type Decision struct {
	// Outcome is the enforcement result.
	Outcome Outcome

	// Threat is the model's 0-10 score, or 0 when no model screen ran.
	Threat int

	// Category is the screen category, [screen.CategoryNone] when clean, or
	// [screen.CategoryUnclassified] when the model returned one outside the
	// vocabulary.
	Category string

	// Verified reports that the model's cited evidence was located in the content.
	// An unverified threat is not blockable evidence -- see Screen.
	Verified bool

	// DetectScore is detect's 0-100 aggregate. It is recorded as corroboration and
	// as a triage signal, and it never blocks a write on its own.
	//
	// That is not timidity about false positives in general -- it is specific to this
	// corpus. detect is keyword-anchored regex, so it fires on any text that CONTAINS
	// a canonical phrasing, including a note quoting one. Memstore holds the user's
	// notes about building software, prompt injection and this package among them: a
	// fact reading "memstore fences content so a fact containing 'ignore all previous
	// instructions' cannot pose as a directive" scores 80, the same as the attack it
	// describes. Enforcing on that score would make the store unable to remember its
	// own security work, and would do it silently.
	//
	// Distinguishing a description of an attack from an attack is exactly what the
	// model screen and [Exclusions] are for, so the decision waits for them. When no
	// model is reachable the write goes to OutcomePending, where it is durable but
	// unreadable -- so nothing is lost and nothing hostile is served in the meantime.
	DetectScore int

	// DetectRules lists the rule IDs that fired, for triage. Rule IDs come from
	// airlock's corpus, not from the content, so they are safe to store and log.
	DetectRules []string

	// Obfuscated reports that the raw content carried stacked combining marks
	// suggesting deliberate obfuscation. Independent of the rules: normalization
	// strips the marks before matching, so obfuscated text can trip nothing at all.
	Obfuscated bool

	// ModelScreened reports that a model verdict contributed. False means regex
	// evidence only, either because no generator is configured or because the call
	// failed -- in which case Outcome is OutcomePending and the write is queued.
	ModelScreened bool

	// Reason names which signal drove a block, in fixed vocabulary. Empty unless
	// Outcome is OutcomeBlocked.
	Reason string
}

// Blocked reports whether the write was rejected.
func (d Decision) Blocked() bool { return d.Outcome == OutcomeBlocked }

// ReasonModelThreat is the only block reason: a verified model verdict at or above
// the threat threshold. Fixed string, never derived from content.
const ReasonModelThreat = "model-threat"

// decide applies the policy to a detect result and an optional model finding.
//
// The model finding is only consulted when it verified: [screen.Verdict.Finding]
// refuses to build a Finding on evidence it could not locate in the content, so an
// unverified threat means the model produced a quote-shaped string rather than a
// citation. Blocking a user's memory on fabricated evidence is worse than missing the
// write, so unverified verdicts never block.
func (p Policy) decide(det detect.Result, fnd screen.Finding, modelScreened bool) Decision {
	d := Decision{
		Outcome:       OutcomeAllowed,
		DetectScore:   det.Score(),
		DetectRules:   ruleIDs(det),
		Obfuscated:    det.Obfuscated,
		ModelScreened: modelScreened,
		Category:      screen.CategoryNone,
	}
	if modelScreened {
		d.Threat = fnd.Threat
		d.Category = fnd.Category
		d.Verified = fnd.Verified
	} else {
		// No model verdict: the write is allowed on regex evidence but must be
		// screened again once a generator is reachable.
		d.Outcome = OutcomePending
	}

	if !p.Enforce {
		return d
	}

	if modelScreened && d.Verified && p.BlockThreat > 0 && d.Threat >= p.BlockThreat {
		d.Outcome = OutcomeBlocked
		d.Reason = ReasonModelThreat
	}
	return d
}

func ruleIDs(r detect.Result) []string {
	if len(r.Matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.Matches))
	for _, m := range r.Matches {
		out = append(out, m.Rule)
	}
	return out
}
