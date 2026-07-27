// Package cperng hands out per-CPE *rand.Rand instances derived from a
// single root seed. Use it for behavior-engine randomness (scheduler
// jitter, value generators, client fabricators), not for cryptographic
// nonces (those use crypto/rand).
//
// design principle #6: "Determinism is opt-in, randomness is the
// default. But every random source must accept a seed so an operator
// can reproduce a scenario exactly when they need to debug an ACS or
// write a regression test."
//
// Usage:
//
//	src := cperng.New(cfg.Seed)
//	logger.Info("rng initialized", "root_seed", src.RootSeed())
//	rng := src.ForCPE("cpe-1")
//	// pass rng to scheduler.Registration{RNG: rng}, value generators, etc.
package cperng

import (
	"encoding/binary"
	"hash/fnv"
	"math/rand" //nolint:gosec // behavior randomness, not security
	"time"
)

// Source hands out per-CPE *rand.Rand instances derived from one root
// seed. Goroutine-safe: ForCPE may be called concurrently. The returned
// *rand.Rand is NOT goroutine-safe (math/rand's standard guarantee);
// callers that share one across goroutines must provide their own
// synchronization.
type Source struct {
	rootSeed int64
}

// New returns a Source with the given root seed. If rootSeed is 0,
// New derives one from time.Now().UnixNano() so unseeded runs are
// non-deterministic by default (design principle #6). The chosen seed
// is exposed via RootSeed for startup-time logging so an operator can
// record it and replay the run later.
func New(rootSeed int64) *Source {
	if rootSeed == 0 {
		rootSeed = time.Now().UnixNano()
	}
	return &Source{rootSeed: rootSeed}
}

// RootSeed returns the actual root seed in use. When the caller passed
// 0 to New, this returns the time-derived seed so the operator can
// reproduce the run by passing it back via --seed next time.
func (s *Source) RootSeed() int64 {
	return s.rootSeed
}

// ForCPE returns a *rand.Rand seeded deterministically from
// (RootSeed, cpeID). Two Sources with the same root seed produce
// identical streams for the same cpeID. Different cpeIDs under one
// root seed produce effectively-independent streams (FNV-64a
// avalanche on the cpeID byte slice).
//
// Empty cpeID is permitted and produces a stable stream derived from
// the root seed alone, useful for tests.
func (s *Source) ForCPE(cpeID string) *rand.Rand {
	h := fnv.New64a()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(s.rootSeed)) //nolint:gosec // bit-pattern reinterpret, not signed-overflow
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(cpeID))
	return rand.New(rand.NewSource(int64(h.Sum64()))) //nolint:gosec // see file header
}
