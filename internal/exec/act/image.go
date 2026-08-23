package act

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fatihy101/standpipe/internal/exec"
)

// Image provisioning is a term of the executor contract.
//
// This is the single item worth EIGHT POINTS of agreement on its own (88.0% ->
// 96.0%), and nothing was looking at it before the design was written.
//
// Three steps, in order: resolve, pull BY DIGEST, tag locally as a
// single-platform image. Four assertions are then made on
// `docker image inspect` BEFORE every run. They are ASSERTIONS and not a
// repair because the repair works today and can come back silently, and when it
// does the symptom is not "wrong answer" but a rain of exit code 127 — one
// `echo ... >> $GITHUB_PATH` drops node off PATH and every subsequent JS action
// dies. That is invisible at run level until someone looks at which STEP fell
// over.
//
// The two halves are split deliberately:
//
//	Ensure  — steps 1-3. Touches the network. NOT called during a run.
//	Verify  — the four assertions. Touches nothing but the local daemon,
//	          and runs on EVERY invocation.
//
// All three steps belong to the executor, and the assertion is made every run
// while the repair comes from configuration. Preflight therefore
// calls Verify only: a run that had to pull would be a run whose ground changed
// underneath it, and in SupplyOffline it would also be a rehearsal reaching for
// the network it is pretending is gone.

// dockerImage is the subset of `docker image inspect` the assertions read.
//
// Config is a POINTER on purpose. Under the containerd image store a tag can
// stand for an OCI INDEX, and inspect then answers `"Config": {}` with
// `"Architecture": ""` [M]. That empty object is the whole reason
// assertion one exists, so it must be distinguishable from a populated one.
type dockerImage struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
	Config      *struct {
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Architecture string `json:"Architecture"`
	Os           string `json:"Os"`
	Descriptor   *struct {
		Digest string `json:"digest"`
	} `json:"Descriptor"`
}

// sourceDigestLabel records which source digest a create_commit image was
// materialised from.
//
// WHY IT EXISTS. The fourth assertion is "the digest equals the one in
// configuration", and the configuration defines that digest as the SOURCE's
// platform-resolved manifest digest. Measured on 2026-08-22: `docker create
// --platform` + `docker commit` produces an image whose every digest — Id,
// Descriptor.digest, RepoDigests — is a NEW, locally computed one. So for
// materialisation `create_commit` the fourth assertion cannot be satisfied by
// any digest the image carries, and without something like this label the
// measured repair path is unverifiable by its own contract. The label is
// stamped by Ensure and checked by Verify, so the identity claim stays
// checkable on every run instead of being dropped.
const sourceDigestLabel = "dev.standpipe.source-digest"

// Ensure performs the three provisioning steps for every configured image, idempotently.
//
// It is an operator / drill-setup action and reaches the network. A run never
// calls it.
func (e *Executor) Ensure(ctx context.Context) error {
	for _, img := range e.cfg.Act.Images {
		if _, err := e.verifyImage(ctx, img); err == nil {
			continue // already the right ground; do not touch the network
		}
		if err := e.materialise(ctx, img); err != nil {
			return fmt.Errorf("act: materialising %s: %w", img.LocalTag, err)
		}
		if _, err := e.verifyImage(ctx, img); err != nil {
			return fmt.Errorf("act: %s was materialised but does not verify: %w", img.LocalTag, err)
		}
	}
	return nil
}

func (e *Executor) materialise(ctx context.Context, img ImageConfig) error {
	ref := img.Source + "@" + img.Digest
	switch img.Materialisation {
	case MaterialisePullByDigest:
		// Step 2: pull BY DIGEST, never by tag. A tag moves, and an image
		// changing under :act-latest during a drill's 14 days is changing the
		// ground in the middle of the score.
		if _, err := e.docker(ctx, "pull", "--platform", img.Platform, ref); err != nil {
			return err
		}
		// Step 3: bind the resolved image to a local, single-platform tag. act
		// is only ever given that tag (via -P) and never pulls.
		_, err := e.docker(ctx, "tag", ref, img.LocalTag)
		return err

	case MaterialiseCreateCommit:
		// The first measurement's repair. `create` is given source@digest rather than
		// the tag so that the pin holds here too.
		out, err := e.docker(ctx, "create", "--platform", img.Platform, ref)
		if err != nil {
			return err
		}
		cid := lastLine(out)
		if cid == "" {
			return fmt.Errorf("docker create printed no container id")
		}
		defer func() { _, _ = e.docker(context.WithoutCancel(ctx), "rm", "-f", cid) }()
		_, err = e.docker(ctx, "commit",
			"-c", "LABEL "+sourceDigestLabel+"="+img.Digest,
			cid, img.LocalTag)
		return err

	default:
		return fmt.Errorf("materialisation %q is not recognised", img.Materialisation)
	}
}

// verifyImages makes the image assertions for every runs-on label the plan
// uses, and returns one provenance row per label (the ledger's exec_image).
//
// Any assertion that does not hold is a refusal: `refused` /
// `image_unverified`, and act is not started.
func (e *Executor) verifyImages(ctx context.Context, runsOn []string) ([]exec.ImageProvenance, error) {
	seen := map[string]bool{}
	out := make([]exec.ImageProvenance, 0, len(runsOn))
	for _, label := range runsOn {
		if seen[label] {
			continue
		}
		seen[label] = true
		img, ok := e.cfg.Act.ImageFor(label)
		if !ok {
			// Reached only if the platform check was skipped; kept so this
			// function is safe on its own.
			return nil, exec.Refuse(exec.RefusalPlatformUnmapped,
				fmt.Errorf("act: runs-on %q has no platforms[] mapping", label))
		}
		prov, err := e.verifyImage(ctx, img)
		if err != nil {
			return nil, exec.Refuse(exec.RefusalImageUnverified,
				fmt.Errorf("act: runs-on %q -> %s: %w", label, img.LocalTag, err))
		}
		prov.RunsOnLabel = label
		out = append(out, prov)
	}
	return out, nil
}

// verifyImage is the image claim, literally: four assertions on the LOCAL tag.
func (e *Executor) verifyImage(ctx context.Context, img ImageConfig) (exec.ImageProvenance, error) {
	prov := exec.ImageProvenance{LocalTag: img.LocalTag, Digest: img.Digest}

	raw, err := e.docker(ctx, "image", "inspect", img.LocalTag)
	if err != nil {
		return prov, fmt.Errorf("docker image inspect: %w", err)
	}
	var images []dockerImage
	if err := json.Unmarshal([]byte(raw), &images); err != nil {
		return prov, fmt.Errorf("parsing docker image inspect: %w", err)
	}
	if len(images) != 1 {
		return prov, fmt.Errorf("docker image inspect returned %d images", len(images))
	}
	return assertImage(images[0], img)
}

// assertImage is the four image assertions, as a pure function over what
// `docker image inspect` said.
//
// It is separated from the daemon call on purpose: the assertions are the part
// worth eight points of agreement, and they should be checkable against
// captured inspect output without a running daemon.
func assertImage(in dockerImage, img ImageConfig) (exec.ImageProvenance, error) {
	prov := exec.ImageProvenance{LocalTag: img.LocalTag, Digest: img.Digest}

	// Assertion 1 — Config is not empty. Under the containerd image store the
	// tag stands for an OCI index and inspect answers "Config": {} [M].
	if in.Config == nil || (len(in.Config.Env) == 0 && len(in.Config.Labels) == 0) {
		return prov, fmt.Errorf("Config is empty — the tag is an unresolved index, not a single-platform image")
	}
	prov.ConfigNonEmpty = true

	// Assertion 2 — Config.Env carries a non-empty PATH. act reads the
	// container PATH from the image's env and falls back to
	// DefaultPathVariable() on an empty read [S run_context.go:489-515]; that
	// fallback is what turns one $GITHUB_PATH append into an exit-code-127
	// rain.
	pathOK := false
	for _, kv := range in.Config.Env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok && strings.TrimSpace(v) != "" {
			pathOK = true
			break
		}
	}
	if !pathOK {
		return prov, fmt.Errorf("Config.Env carries no non-empty PATH")
	}
	prov.PathPresent = true

	// Assertion 3 — Architecture is not empty and matches the expected
	// platform. An empty architecture is the signature of an unresolved index
	// [M].
	if in.Architecture == "" {
		return prov, fmt.Errorf("Architecture is empty — the signature of an unresolved index")
	}
	prov.Architecture = in.Architecture
	wantOS, wantArch, _ := strings.Cut(img.Platform, "/")
	if in.Architecture != wantArch {
		return prov, fmt.Errorf("Architecture is %q, expected %q from images[].platform", in.Architecture, wantArch)
	}
	if in.Os != "" && in.Os != wantOS {
		return prov, fmt.Errorf("Os is %q, expected %q from images[].platform", in.Os, wantOS)
	}

	// Assertion 4 — digest identity. The only evidence that steps 1 and 2
	// actually happened.
	if !digestMatches(in, img) {
		return prov, fmt.Errorf("no identity digest matches images[].digest %s (Id=%s, RepoDigests=%v)",
			img.Digest, in.ID, in.RepoDigests)
	}
	return prov, nil
}

// digestMatches is assertion 4.
//
// Which field carries the identity depends on the store and on how the tag was
// materialised, so all of them are accepted and each one is a genuine identity
// claim rather than a loosening:
//
//   - Id and Descriptor.digest are the manifest digest under the containerd
//     store (measured: a pull-by-digest tag has Id == the platform-resolved
//     manifest digest);
//   - RepoDigests carries `<repo>@<digest>` for anything that came from a
//     registry;
//   - the source-digest LABEL is the only one available after create_commit,
//     which recomputes every other digest locally. See sourceDigestLabel.
func digestMatches(in dockerImage, img ImageConfig) bool {
	if in.ID == img.Digest {
		return true
	}
	if in.Descriptor != nil && in.Descriptor.Digest == img.Digest {
		return true
	}
	for _, rd := range in.RepoDigests {
		if _, d, ok := strings.Cut(rd, "@"); ok && d == img.Digest {
			return true
		}
	}
	if in.Config != nil && in.Config.Labels[sourceDigestLabel] == img.Digest {
		return true
	}
	return false
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
