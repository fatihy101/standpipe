// Package act implements exec.Executor over nektos/act: image provisioning,
// the invocation contract and `defaults.run` normalisation.
//
// Everything that must not leak through the executor interface lives here and nowhere else: platform
// mapping, container images and digests, --local-repository, --pull, the
// artifact and cache server addresses, act's exit code, act's log format, the
// defaults.run merge rule, shell selection and ACT_DISABLE_VERSION_CHECK.
// `drill --exec` knows none of these words.
//
// Which implementation obligations (see package exec) this discharges, and
// where:
//
//	U-1  ground verified before running   image.go     (the four image assertions)
//	U-2  GitHub's semantics, not act's    normalise.go (the replace rule)
//	U-3  workdir at SHA and clean         workdir.go
//	U-4  structured job and step set      parse.go     (act --json --verbose)
//	U-5  wall clock enforced here         executor.go  (act ignores timeout-minutes)
//	U-6  provenance filled always         executor.go
//	U-7  never skip silently              parse.go + executor.go
//	U-8  mirror revisions read pre-run    localrepo.go
//
// EVIDENCE DISCIPLINE. Claims about act carry an evidence marker: [M] measured
// by running it, [S] read in act's source, [V] verified from its documentation
// or --help output. A marker is not promoted by being read again. In
// particular: an argv element marked [V — act v0.2.89 --help] has a proven
// SPELLING and an UNMEASURED behaviour. This package does not branch on any
// behaviour whose only evidence is [V] or [S].
package act
