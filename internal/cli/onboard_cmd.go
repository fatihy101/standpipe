package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/fatihy101/standpipe/internal/adapter"
	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/store"
)

// runOnboardCmd implements `standpipe onboard`.
//
// The command adds a target repo to `allowed_targets` and reports what the
// operator still has to do. The six onboarding steps map to the six blocks below.
//
// `--workflows` is OPTIONAL: the gateway already extracts
// the workflow list in its surface discovery, and asking an operator to
// type a list we can read produces typos and omissions. Discovery SUGGESTS,
// it does not replace — the write still happens after confirmation.
//
// When discovery fails, the command STOPS and asks for `--workflows`. It never
// writes an empty list quietly: an empty `workflows` fails config.Validate,
// and a target that exists with nothing allowed under it is a gateway that
// answers 403 to everything with no indication why.
func runOnboardCmd(args []string) error {
	fs := newFlagSet("onboard")
	cfgPath := fs.String("config", "standpipe.yaml", "path to standpipe.yaml")
	repoFlag := fs.String("repo", "", "owner/name of the repo to onboard")
	workflowsFlag := fs.String("workflows", "", "comma-separated workflow files; discovered when omitted")
	assumeYes := fs.Bool("yes", false, "skip the confirmation prompt")
	dryRun := fs.Bool("dry-run", false, "report what would be written and change nothing")
	signalPID := fs.Int("signal", 0, "pid of the running gateway to SIGHUP after writing")
	pidFile := fs.String("pid-file", "", "read the gateway's pid from this file instead of --signal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repoFlag == "" {
		return errors.New("onboard: --repo owner/name is required")
	}
	repo, err := forge.ParseRepo(*repoFlag)
	if err != nil {
		return fmt.Errorf("onboard --repo: %w", err)
	}

	ctx := context.Background()
	cfg, db, err := openStore(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := cfg.CheckFiles(); err != nil {
		// Unlike `config validate`, onboard actually calls GitHub, so an
		// unreadable App key is fatal here rather than a warning.
		return err
	}

	gh, err := forge.New(forge.Config{
		AppID:          cfg.GitHub.AppID,
		PrivateKeyPath: cfg.GitHub.PrivateKeyPath,
		MaxInflight:    cfg.GitHub.MaxInflight,
		MaxQueueWait:   cfg.GitHub.MaxQueueWait.D(),
		// The RESOLVED value, never a zero.
		ReturnRunDetails:     cfg.GitHub.ReturnRunDetails,
		InventoryHistoryDays: cfg.Inventory.HistoryDays,
		// github.base_url, the same value the gateway uses. `onboard` calls
		// GitHub for real, so a deployment whose API root is not github.com
		// would otherwise have a CLI that talks to a different server than the
		// daemon it is configuring.
		BaseURL:   cfg.GitHub.BaseURL,
		UserAgent: "standpipe-onboard/" + version,
	}, forge.WithStore(&adapter.ForgeStore{DB: db}))
	if err != nil {
		return err
	}

	// ---- Step 1: installation check, and step 1a: surface discovery.
	//
	// Both come from one call. internal/forge exposes no installation lookup
	// of its own (installationID is unexported), but DiscoverSurface goes
	// through it and answers KindInstallationNotFound or KindAppNotFound —
	// distinct operator-visible states. The installation ID that the documented
	// example output prints cannot be shown from here; the need for an exported
	// lookup is recorded as an internal/forge interface request rather than worked around by
	// reaching into the package.
	surface, err := gh.DiscoverSurface(ctx, repo)
	if err != nil {
		switch forge.KindOf(err) {
		case forge.KindAppNotFound:
			// github.app_id names no App at all (measured 2026-08-22: GitHub
			// answers 404 "Integration not found"). This branch exists so the
			// case does not fall through to the generic handler by luck: the
			// generic one ends by suggesting --workflows, which cannot help,
			// and the installation branch below would send the operator to
			// install an App that does not exist.
			fmt.Printf("✗ %s — %v\n", repo, err)
			fmt.Printf("  → fix github.app_id in %s; no installation can succeed while the id names no App\n", *cfgPath)
			fmt.Printf("  → nothing was written to %s\n", *cfgPath)
			return silentExit(1)
		case forge.KindInstallationNotFound:
			// forge's own message is printed instead of a sentence of ours.
			// This used to assert "the App is NOT installed on this repo",
			// which is one of THREE causes a bare 404 cannot separate — the
			// repo may also not exist, or the App may not be able to see it.
			// Naming one of them is the flattening this slice exists to
			// remove; forge names all three and picks none.
			fmt.Printf("✗ %s — %v\n", repo, err)
			fmt.Printf("  → if the App is simply not installed: https://github.com/settings/installations\n")
			fmt.Printf("  → nothing was written to %s\n", *cfgPath)
			return silentExit(1)
		}
		fmt.Printf("✗ %s — workflow discovery failed: %v\n", repo, err)
		fmt.Printf("  → onboard does NOT continue with an empty list.\n")
		fmt.Printf("  → re-run with an explicit list: standpipe onboard --repo %s --workflows a.yml,b.yml\n", repo)
		return silentExit(1)
	}
	fmt.Printf("✓ %s — installation found, default branch %s\n", repo, surface.DefaultBranch)

	// ---- Step 2: which workflows are dispatchable.
	dispatchable := map[string]forge.WorkflowCacheEntry{}
	var discovered []string
	for _, e := range surface.Workflows {
		if e.HasTrigger(forge.TriggerWorkflowDispatch) {
			dispatchable[e.Workflow] = e
			discovered = append(discovered, e.Workflow)
		}
	}
	sort.Strings(discovered)

	var chosen []string
	if *workflowsFlag != "" {
		for _, w := range strings.Split(*workflowsFlag, ",") {
			if w = strings.TrimSpace(w); w != "" {
				chosen = append(chosen, w)
			}
		}
	} else {
		if len(discovered) == 0 {
			fmt.Printf("✗ %s — no workflow declares `on: workflow_dispatch`\n", repo)
			if len(surface.Cold) > 0 {
				fmt.Printf("  → %d workflow file(s) could not be read: %s\n", len(surface.Cold), strings.Join(surface.Cold, ", "))
				fmt.Printf("  → a cold definition is not an empty one; onboard will not guess\n")
			}
			fmt.Printf("  → nothing was written. Pass --workflows to add targets explicitly.\n")
			return silentExit(1)
		}
		fmt.Printf("✓ discovered %d workflow_dispatch workflow(s): %s\n", len(discovered), strings.Join(discovered, ", "))
		if !*assumeYes && !*dryRun && !confirm(fmt.Sprintf("  add all %d to allowed_targets?", len(discovered))) {
			fmt.Printf("  → declined; nothing was written\n")
			return silentExit(1)
		}
		chosen = discovered
	}

	// ---- Steps 2 and 3: per-workflow validation and the hardening report.
	var writable []string
	for _, wf := range chosen {
		if !isWorkflowFileName(wf) {
			// config.Validate would reject it anyway; catching it here means
			// the operator gets one clear message instead of a validation dump
			// after a GitHub round trip.
			fmt.Printf("✗ %s — not a workflow file name (expected something like release.yml); skipped\n", wf)
			continue
		}
		entry, ok := dispatchable[wf]
		if !ok {
			// Reported as a warning, not a refusal. A workflow on
			// a branch that has not merged yet is a legitimate reason to
			// allowlist a file that discovery cannot see.
			fmt.Printf("! %s — no `on: workflow_dispatch` found in the default branch's copy; adding anyway\n", wf)
			fmt.Printf("  → the gateway will answer 403 workflow_not_allowed until it exists\n")
			writable = append(writable, wf)
			continue
		}
		if _, hasID := entry.InputsDef[forge.ReservedInputKey]; hasID {
			fmt.Printf("✓ %s — workflow_dispatch found, %s: present\n", wf, forge.ReservedInputKey)
		} else {
			// `!`, not `✗`. Tier 1 already gives
			// this workflow `resolution: exact` in normal operation. What is
			// missing is the RECOVERY path — and it is missing exactly when the
			// dispatch response is lost, which is during an outage, which is
			// the event that produced this product.
			fmt.Printf("! %s — workflow_dispatch found, %s: MISSING\n", wf, forge.ReservedInputKey)
			fmt.Printf("  → tier 1 correlation works; tier 2 (outage recovery) is not armed\n")
			fmt.Printf("  → optional hardening: add inputs.%s + run-name (tier 2)\n", forge.ReservedInputKey)
		}
		if entry.InputsDef == nil {
			fmt.Printf("  → its definition is UNKNOWN (the file could not be read): input validation\n")
			fmt.Printf("    will be SKIPPED and events will record inputs_validation `%s`\n", store.ValidationSkippedColdCache)
		}
		writable = append(writable, wf)
	}
	if len(writable) == 0 {
		fmt.Printf("✗ nothing to write\n")
		return silentExit(1)
	}

	// ---- Steps 4 and 6: write, idempotently.
	original, err := os.ReadFile(*cfgPath)
	if err != nil {
		return err
	}
	next, changed, err := upsertAllowedTarget(original, repo.String(), writable)
	if err != nil {
		return fmt.Errorf("onboard: editing %s: %w", *cfgPath, err)
	}
	if !changed {
		fmt.Printf("✓ %s already lists %s with %s — nothing to do\n",
			*cfgPath, repo, strings.Join(writable, ", "))
		return nil
	}
	if *dryRun {
		fmt.Printf("\ndry-run: %s would become:\n\n%s\n", *cfgPath, next)
		if _, err := config.Parse(next); err != nil {
			return fmt.Errorf("and it would NOT validate:\n%w", err)
		}
		fmt.Printf("(it validates; nothing was written)\n")
		return nil
	}
	if err := writeConfigAtomically(*cfgPath, original, next); err != nil {
		return fmt.Errorf("onboard: %w", err)
	}
	fmt.Printf("✓ allowed_targets entry written to %s (%s: %s)\n", *cfgPath, repo, strings.Join(writable, ", "))
	fmt.Printf("  YAML comments were preserved; blank lines were not (yaml.v3 does not model them).\n")

	// ---- Step 5: tell the running gateway.
	pid, err := gatewayPID(*signalPID, *pidFile)
	switch {
	case err != nil:
		fmt.Printf("! SIGHUP NOT sent: %v\n", err)
		fmt.Printf("  → the file is written but the running gateway is still on the old allowlist.\n")
		fmt.Printf("  → send it yourself: kill -HUP <pid>   (SIGHUP reloads allowed_targets)\n")
	case pid == 0:
		fmt.Printf("! SIGHUP NOT sent: no --signal <pid> and no --pid-file.\n")
		fmt.Printf("  → the file is written but the running gateway is still on the old allowlist.\n")
		fmt.Printf("  → send it yourself: kill -HUP <pid>   (SIGHUP reloads allowed_targets)\n")
	default:
		if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
			fmt.Printf("! SIGHUP to pid %d failed: %v — send it yourself with `kill -HUP %d`\n", pid, err, pid)
		} else {
			fmt.Printf("✓ SIGHUP sent to gateway (pid %d)\n", pid)
			// A reload that fails validation leaves the OLD
			// configuration live and logs an error. The file was validated
			// before it was written, so this should not happen — but the
			// gateway's log is where it would show, not here.
			fmt.Printf("  the gateway logs the reload result; a failed reload keeps the previous config\n")
		}
	}

	fmt.Printf("\nNext: `standpipe status`, then a smoke trigger and `standpipe trace <event-id>`\n")
	fmt.Printf("to confirm resolution `exact` from source `dispatch_response`.\n")
	return nil
}

// gatewayPID resolves the target process for step 5 (SIGHUP).
func gatewayPID(flagPID int, pidFile string) (int, error) {
	if flagPID > 0 {
		return flagPID, nil
	}
	if pidFile == "" {
		return 0, nil
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", pidFile, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("%s does not contain a pid", pidFile)
	}
	return pid, nil
}

// isWorkflowFileName mirrors config.Validate's rule for allowed_targets
// workflow entries: a file name, no path, no wildcard.
func isWorkflowFileName(s string) bool {
	if strings.ContainsAny(s, "/\\*?") {
		return false
	}
	return strings.HasSuffix(s, ".yml") || strings.HasSuffix(s, ".yaml")
}
