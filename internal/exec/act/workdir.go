package act

import (
	"bytes"
	"context"
	"fmt"
	osexec "os/exec"
	"strconv"
	"strings"

	"github.com/fatihy101/standpipe/internal/exec"
)

// U-3 — the work directory is asserted to be AT CommitSHA and CLEAN.
//
// act copies the working directory into the container with `docker cp` [S/M],
// so a dirty tree puts files into the run that do not exist on GitHub.
// That is not a cosmetic difference: it is work the GitHub side never saw,
// scored as if both sides ran the same thing.
//
// Not satisfied means REFUSE. There is no repair.

func (e *Executor) inspectWorkdir(ctx context.Context, dir, wantSHA string) (exec.WorkdirProvenance, error) {
	var prov exec.WorkdirProvenance

	head, err := e.git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return prov, exec.Refuse(exec.RefusalWorkdirDirty,
			fmt.Errorf("act: %s: cannot read HEAD: %w", dir, err))
	}
	prov.HeadSHA = head
	if !strings.EqualFold(head, wantSHA) {
		return prov, exec.Refuse(exec.RefusalWorkdirDirty,
			fmt.Errorf("act: %s: HEAD is %s, the request replays %s", dir, head, wantSHA))
	}

	// --porcelain covers modified AND untracked files. Untracked matters as
	// much as modified: act copies the directory, not the index.
	status, err := e.git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return prov, exec.Refuse(exec.RefusalWorkdirDirty,
			fmt.Errorf("act: %s: cannot read status: %w", dir, err))
	}
	if strings.TrimSpace(status) != "" {
		lines := strings.Split(strings.TrimSpace(status), "\n")
		return prov, exec.Refuse(exec.RefusalWorkdirDirty,
			fmt.Errorf("act: %s: %d modified or untracked path(s), first is %q", dir, len(lines), strings.TrimSpace(lines[0])))
	}
	prov.Clean = true

	// The ledger's workdir_commit_count and workdir_shallow. Neither is an
	// assertion: they exist so a run whose checkout fidelity differed (act's
	// checkout is a copy of the working directory, history included, not a
	// real checkout) stays visible afterwards.
	if n, err := e.git(ctx, dir, "rev-list", "--count", "HEAD"); err == nil {
		if c, convErr := strconv.Atoi(strings.TrimSpace(n)); convErr == nil {
			prov.CommitCount = c
		}
	}
	if s, err := e.git(ctx, dir, "rev-parse", "--is-shallow-repository"); err == nil {
		shallow := strings.TrimSpace(s) == "true"
		prov.Shallow = &shallow
	}
	return prov, nil
}

func (e *Executor) git(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := osexec.CommandContext(ctx, e.gitBinary, full...)
	// Same narrow environment discipline as everything else this package
	// starts. GIT_DIR, GIT_WORK_TREE and friends from the operator's shell
	// would silently retarget every one of these reads.
	cmd.Env = []string{"PATH=" + NarrowPath, "HOME=" + e.home, "GIT_TERMINAL_PROMPT=0"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
