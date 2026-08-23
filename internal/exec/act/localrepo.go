package act

import (
	"context"
	"fmt"
	"strings"

	"github.com/fatihy101/standpipe/internal/exec"
)

// U-8 — the resolved revision of every local source mapping is recorded by the
// executor, BEFORE the run.
//
// WHY THIS EXISTS AT ALL [M]. A mirror that is MISSING makes the run fall
// over loudly. A mirror that is STALE produces a clean `success`: a mirror
// rewound about 25 commits ran a four-year-old action and not one warning
// appeared. act writes the matched PATH at debug level and the RESOLVED
// REVISION at no level at all, and GITHUB_ACTION_REF carries `v3`, not a SHA.
// So what `@v4` resolved to cannot be read out of act's output — if it is not
// written here it exists nowhere.
//
// WHAT THIS DOES NOT DO, said plainly so the line is not read as closing a
// silent degradation: it makes staleness AUDITABLE, not NOISY. A drill against
// a stale mirror still produces `success` and still enters the gate as a match.
// All U-8 buys is that afterwards someone can ask which tree it ran against.
// Deciding what counts as stale, and warning once an outage has already begun,
// belong to the open question of outage-critical host state.
//
// Unreadable is a REFUSAL: `refused` / `local_repository_unresolved`. There is
// no such thing as a NULL resolved SHA in the ledger.

func (e *Executor) resolveLocalRepositories(ctx context.Context) ([]exec.LocalRepositoryProvenance, error) {
	mappings := e.cfg.Act.LocalRepository
	out := make([]exec.LocalRepositoryProvenance, 0, len(mappings))
	for _, url := range sortedKeys(mappings) {
		path := mappings[url]
		sha, err := e.git(ctx, path, "rev-parse", "HEAD")
		if err != nil {
			return nil, exec.Refuse(exec.RefusalLocalRepositoryUnresolved,
				fmt.Errorf("act: local_repository[%s] -> %s: %w", url, path, err))
		}
		sha = strings.TrimSpace(sha)
		if !isHex40(sha) {
			return nil, exec.Refuse(exec.RefusalLocalRepositoryUnresolved,
				fmt.Errorf("act: local_repository[%s] -> %s: HEAD resolved to %q, not a 40-hex SHA", url, path, sha))
		}
		out = append(out, exec.LocalRepositoryProvenance{URL: url, LocalPath: path, ResolvedSHA: sha})
	}
	return out, nil
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
