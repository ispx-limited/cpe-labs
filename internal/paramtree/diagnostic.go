package paramtree

import (
	"fmt"
	"time"
)

// DiagnosticConfig describes a TR-069 triggered diagnostic: a parameter
// an ACS writes to start work, which the CPE later moves to a terminal
// value once the work is done.
//
// WHY THIS IS NOT A GENERATOR.
//
// Every other animation in a profile is time-driven: a generator ticks
// on an interval and writes a value whether anyone asked or not. A
// diagnostic is the opposite. Nothing happens until an ACS writes the
// trigger, and the observable difference between "never asked" and
// "asked and found nothing" is the entire point of the parameter. A
// neighbour scan is the clearest case: an empty result table means one
// thing before a sweep and something completely different after one,
// and a fleet that reported results without being asked would let an
// ACS believe it had measured air it never touched.
//
// WHAT IT DELIBERATELY DOES NOT SIMULATE.
//
// The result rows themselves are ordinary profile parameters, drawn
// per CPE like any other value. The diagnostic does not invent
// neighbours on each run: a given home hears roughly the same
// neighbours every sweep, so redrawing them per run would be less
// realistic, not more. What the diagnostic owns is the state machine
// and the count, which are the two things an ACS actually keys on.
type DiagnosticConfig struct {
	// StatePath is the leaf an ACS writes to start the diagnostic and
	// reads to learn it finished.
	StatePath string

	// Trigger is the value that starts a run. TR-069 spells this
	// "Requested" across every standard diagnostic; vendors that use
	// another word set it here rather than patching the simulator.
	Trigger string

	// Complete is the terminal value written when the run finishes.
	// Vendor spellings differ, which is why this is config: the ACS
	// side waits for "not Requested and not None" precisely because it
	// cannot assume this string.
	Complete string

	// Duration is how long the run takes before Complete lands. A real
	// neighbour sweep takes seconds because the radio goes off channel
	// per channel, and an ACS that polls will see the intermediate
	// state, so completing instantly would hide a state an operator's
	// tooling has to handle.
	Duration time.Duration

	// CountPath is the leaf holding the number of result rows. Written
	// to ResultCount when the run completes and left alone otherwise,
	// so a device that has never been asked reports zero.
	CountPath string

	// ResultCount is the value written to CountPath on completion. It
	// must match the number of result rows the profile declares, which
	// the loader checks rather than trusting.
	ResultCount int
}

// rawDiagnostic is the YAML shape of one diagnostic.
type rawDiagnostic struct {
	StatePath   string `yaml:"statePath"`
	Trigger     string `yaml:"trigger"`
	Complete    string `yaml:"complete"`
	Duration    string `yaml:"duration"`
	CountPath   string `yaml:"countPath"`
	ResultCount int    `yaml:"resultCount"`
}

// parseDiagnostic validates one diagnostic against the built tree.
//
// Every path is checked for existence here rather than at run time: a
// diagnostic whose state path is misspelled would otherwise look like a
// device that simply never completes, which is indistinguishable from a
// genuine firmware fault and is exactly the confusion the simulator
// exists to keep out of a test.
func parseDiagnostic(tree *Tree, raw rawDiagnostic, where string) (DiagnosticConfig, error) {
	if raw.StatePath == "" {
		return DiagnosticConfig{}, fmt.Errorf("%s: statePath is required", where)
	}
	if _, err := tree.Get(raw.StatePath); err != nil {
		return DiagnosticConfig{}, fmt.Errorf("%s: statePath %q: %w", where, raw.StatePath, err)
	}
	trigger := raw.Trigger
	if trigger == "" {
		trigger = "Requested"
	}
	complete := raw.Complete
	if complete == "" {
		complete = "Complete"
	}
	if trigger == complete {
		return DiagnosticConfig{}, fmt.Errorf(
			"%s: trigger and complete are both %q, so a run could never be observed to finish",
			where, trigger)
	}

	// Default rather than required: a sweep that reports instantly is
	// the one shape a real device never has, so the zero value has to
	// be a realistic duration rather than zero.
	duration := 5 * time.Second
	if raw.Duration != "" {
		d, err := time.ParseDuration(raw.Duration)
		if err != nil {
			return DiagnosticConfig{}, fmt.Errorf("%s: duration %q: %w", where, raw.Duration, err)
		}
		if d < 0 {
			return DiagnosticConfig{}, fmt.Errorf("%s: duration must not be negative, got %s", where, d)
		}
		duration = d
	}

	if raw.CountPath != "" {
		if _, err := tree.Get(raw.CountPath); err != nil {
			return DiagnosticConfig{}, fmt.Errorf("%s: countPath %q: %w", where, raw.CountPath, err)
		}
		if raw.ResultCount < 0 {
			return DiagnosticConfig{}, fmt.Errorf("%s: resultCount must not be negative, got %d", where, raw.ResultCount)
		}
	}

	return DiagnosticConfig{
		StatePath:   raw.StatePath,
		Trigger:     trigger,
		Complete:    complete,
		Duration:    duration,
		CountPath:   raw.CountPath,
		ResultCount: raw.ResultCount,
	}, nil
}
