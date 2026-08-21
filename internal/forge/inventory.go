package forge

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/go-github/v90/github"
	"gopkg.in/yaml.v3"
)

// WorkflowCacheDefaultRef is the workflow_cache.ref value used in Phase 0.
// Only the default branch is fetched, and the row records that fact
// rather than pretending it holds every ref.
const WorkflowCacheDefaultRef = "default"

// Trigger event names that make a workflow externally triggerable.
const (
	TriggerWorkflowDispatch   = "workflow_dispatch"
	TriggerRepositoryDispatch = "repository_dispatch"
)

// perPage is the history-listing page size. A small page size loses
// candidates on a busy repo, so every listing here uses the maximum.
const perPage = 100

// maxPagesPerSlice bounds one day slice. GitHub caps accessible results at
// 1000 per query; ten full pages is that ceiling.
const maxPagesPerSlice = 10

// RepoSurface is the result of surface discovery for one repo.
type RepoSurface struct {
	Repo          Repo
	DefaultBranch string
	Workflows     []WorkflowCacheEntry
	// Cold lists workflows whose file could not be read. They are cached with
	// a nil InputsDef so validation is skipped rather than wrongly rejecting.
	Cold []string
}

// DiscoverSurface lists a repo's workflows, reads each file on the default
// branch, and writes the result to workflow_cache.
//
// It needs contents: read, and nothing more. The gateway never writes a
// workflow file.
func (c *Client) DiscoverSurface(ctx context.Context, repo Repo) (*RepoSurface, error) {
	if c.store == nil {
		return nil, errors.New("forge: no store configured")
	}

	repository, _, err := call(ctx, c, repo, LaneInventory,
		func(ctx context.Context, gh *github.Client) (*github.Repository, *github.Response, error) {
			return gh.Repositories.Get(ctx, repo.Owner, repo.Name)
		})
	if err != nil {
		return nil, err
	}
	defaultBranch := repository.GetDefaultBranch()

	surface := &RepoSurface{Repo: repo, DefaultBranch: defaultBranch}

	opts := &github.ListOptions{PerPage: perPage}
	for {
		page, resp, err := call(ctx, c, repo, LaneInventory,
			func(ctx context.Context, gh *github.Client) (*github.Workflows, *github.Response, error) {
				return gh.Actions.ListWorkflows(ctx, repo.Owner, repo.Name, opts)
			})
		if err != nil {
			return nil, err
		}
		for _, wf := range page.Workflows {
			entry, cold := c.workflowEntry(ctx, repo, wf, defaultBranch)
			if cold {
				surface.Cold = append(surface.Cold, entry.Workflow)
				c.metrics.IncCounter("gateway_workflow_cache_cold_total", map[string]string{
					"repo": repo.String(), "workflow": entry.Workflow,
				})
			}
			if err := c.store.PutWorkflowCache(ctx, entry); err != nil {
				return nil, err
			}
			surface.Workflows = append(surface.Workflows, entry)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return surface, nil
}

// workflowEntry builds one cache row. It returns cold=true when the file could
// not be read, in which case InputsDef stays nil — the "unknown" value, which
// is not the same as "declares no inputs".
func (c *Client) workflowEntry(ctx context.Context, repo Repo, wf *github.Workflow, defaultBranch string) (WorkflowCacheEntry, bool) {
	file := path.Base(wf.GetPath())
	entry := WorkflowCacheEntry{
		Repo:          repo.String(),
		Workflow:      file,
		Ref:           WorkflowCacheDefaultRef,
		WorkflowID:    wf.GetID(),
		DefaultBranch: defaultBranch,
		FetchedAt:     c.now(),
	}

	content, _, err := call(ctx, c, repo, LaneInventory,
		func(ctx context.Context, gh *github.Client) (*github.RepositoryContent, *github.Response, error) {
			fileContent, _, resp, err := gh.Repositories.GetContents(ctx, repo.Owner, repo.Name, wf.GetPath(),
				&github.RepositoryContentGetOptions{Ref: defaultBranch})
			return fileContent, resp, err
		})
	if err != nil {
		return entry, true
	}
	raw, err := content.GetContent()
	if err != nil || raw == "" {
		return entry, true
	}

	triggers, inputs, err := parseWorkflowFile([]byte(raw))
	if err != nil {
		return entry, true
	}
	entry.Triggers = triggers
	entry.InputsDef = inputs
	return entry, false
}

// resolveWorkflowID resolves a workflow file name to its numeric ID and
// caches it. Called only when the cache has no row yet.
func (c *Client) resolveWorkflowID(ctx context.Context, repo Repo, workflow string) (int64, error) {
	opts := &github.ListOptions{PerPage: perPage}
	for {
		page, resp, err := call(ctx, c, repo, LaneDispatch,
			func(ctx context.Context, gh *github.Client) (*github.Workflows, *github.Response, error) {
				return gh.Actions.ListWorkflows(ctx, repo.Owner, repo.Name, opts)
			})
		if err != nil {
			return 0, err
		}
		for _, wf := range page.Workflows {
			if path.Base(wf.GetPath()) == workflow {
				return wf.GetID(), nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	// The workflow is not in the repo's workflow list. This is the same fact
	// an upstream 404 would report, so it maps to the same caller response.
	return 0, &Error{
		Kind:    KindWorkflowNotFound,
		Message: fmt.Sprintf("%s has no workflow %s", repo, workflow),
	}
}

// ---------------------------------------------------------------------------
// History discovery
// ---------------------------------------------------------------------------

// HistoryResult reports one repo's history fetch.
type HistoryResult struct {
	Repo Repo
	// Fetched is how many run records were written.
	Fetched int
	// Truncated is true when GitHub's accessible-result ceiling cut a slice
	// short. This is surfaced, never swallowed — an inventory that
	// silently under-reports history would understate the coverage gap.
	Truncated bool
	// TruncatedDays lists the day slices that hit the ceiling.
	TruncatedDays []string
	// Changed carries runs whose conclusion moved, for the run_observed
	// producer.
	Changed []RunRecord
}

// DiscoverHistory fetches workflow_dispatch and repository_dispatch run
// history for a repo, day-sliced, and upserts it into github_runs.
//
// Day slicing exists because GitHub caps the accessible result set per query.
// One query over 30 days silently returns the newest 1000 runs and calls it
// complete; one query per day keeps each slice under the ceiling for all but
// the busiest repos, and the ceiling that is still hit gets counted.
func (c *Client) DiscoverHistory(ctx context.Context, repo Repo, days int) (*HistoryResult, error) {
	if c.store == nil {
		return nil, errors.New("forge: no store configured")
	}
	if days <= 0 {
		days = c.cfg.InventoryHistoryDays
	}

	out := &HistoryResult{Repo: repo}
	now := c.now().UTC()

	for _, event := range []string{TriggerWorkflowDispatch, TriggerRepositoryDispatch} {
		for i := range days {
			day := now.AddDate(0, 0, -i)
			slice := day.Format("2006-01-02")

			runs, truncated, err := c.fetchDaySlice(ctx, repo, event, slice)
			if err != nil {
				// A paused quota stops background work by design; it
				// is not a bootstrap failure. Everything gathered so far is
				// kept and reported.
				if errors.Is(err, ErrQuotaPaused) {
					return out, err
				}
				return out, err
			}
			if truncated {
				out.Truncated = true
				out.TruncatedDays = append(out.TruncatedDays, slice)
				c.metrics.IncCounter("gateway_bootstrap_truncated_total",
					map[string]string{"repo": repo.String()})
			}
			if len(runs) == 0 {
				continue
			}
			changed, err := c.store.UpsertRuns(ctx, runs)
			if err != nil {
				return out, err
			}
			out.Fetched += len(runs)
			out.Changed = append(out.Changed, changed...)
		}
	}
	return out, nil
}

// fetchDaySlice pages through one day's runs for one event type.
func (c *Client) fetchDaySlice(ctx context.Context, repo Repo, event, day string) ([]RunRecord, bool, error) {
	opts := &github.ListWorkflowRunsOptions{
		Event:       event,
		Created:     day + ".." + day,
		ListOptions: github.ListOptions{PerPage: perPage},
	}

	var (
		records []RunRecord
		total   int
		pages   int
	)
	for {
		page, resp, err := call(ctx, c, repo, LaneInventory,
			func(ctx context.Context, gh *github.Client) (*github.WorkflowRuns, *github.Response, error) {
				return gh.Actions.ListRepositoryWorkflowRuns(ctx, repo.Owner, repo.Name, opts)
			})
		if err != nil {
			return nil, false, err
		}
		total = page.GetTotalCount()
		for _, run := range page.WorkflowRuns {
			records = append(records, c.runRecord(repo, run))
		}
		pages++
		if resp == nil || resp.NextPage == 0 || pages >= maxPagesPerSlice {
			break
		}
		opts.Page = resp.NextPage
	}

	// Ceiling signal: total_count exceeds what we could actually read.
	truncated := total > len(records)
	return records, truncated, nil
}

func (c *Client) runRecord(repo Repo, run *github.WorkflowRun) RunRecord {
	return RunRecord{
		RunID:      run.GetID(),
		Repo:       repo.String(),
		WorkflowID: run.GetWorkflowID(),
		Workflow:   path.Base(run.GetPath()),
		Event:      run.GetEvent(),
		HeadBranch: run.GetHeadBranch(),
		// The coverage-gap report compares github_runs.head_branch against trigger_events
		// .target_ref, which is a full ref. Normalizing at write time keeps
		// the comparison out of the SQL.
		HeadBranchNormalized: BranchRef(run.GetHeadBranch()),
		Actor:                run.GetActor().GetLogin(),
		TriggeringActor:      run.GetTriggeringActor().GetLogin(),
		Conclusion:           run.GetConclusion(),
		CreatedAt:            run.GetCreatedAt().Time,
		FetchedAt:            c.now(),
	}
}

// ---------------------------------------------------------------------------
// Workflow file parsing
// ---------------------------------------------------------------------------

// parseWorkflowFile extracts the on: trigger names and the
// on.workflow_dispatch.inputs definition.
//
// It walks yaml.Node rather than unmarshalling into map[string]any on purpose.
// yaml.v3 keeps YAML 1.1's boolean words, so the key `on:` resolves to the
// boolean true and the block disappears from a generic map — the single most
// common way to misread a GitHub Actions file. Reading mapping keys by their
// literal Value sidesteps it. The failure is silent: not a parse error, but a
// workflow that appears to declare no triggers and no inputs.
func parseWorkflowFile(src []byte) (triggers []string, inputs map[string]InputDef, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse workflow yaml: %w", err)
	}
	root := documentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, nil, errors.New("parse workflow yaml: top level is not a mapping")
	}

	on := mappingValue(root, "on")
	if on == nil {
		// A workflow with no on: block is malformed, but reporting "no
		// triggers" is the honest reading rather than an error that would
		// mark the cache cold.
		return nil, map[string]InputDef{}, nil
	}

	switch on.Kind {
	case yaml.ScalarNode: // on: workflow_dispatch
		triggers = []string{on.Value}
	case yaml.SequenceNode: // on: [push, workflow_dispatch]
		for _, item := range on.Content {
			if item.Kind == yaml.ScalarNode {
				triggers = append(triggers, item.Value)
			}
		}
	case yaml.MappingNode: // on: { workflow_dispatch: { inputs: ... } }
		for i := 0; i+1 < len(on.Content); i += 2 {
			triggers = append(triggers, on.Content[i].Value)
		}
	}

	inputs = map[string]InputDef{}
	if on.Kind == yaml.MappingNode {
		if wd := mappingValue(on, TriggerWorkflowDispatch); wd != nil && wd.Kind == yaml.MappingNode {
			if defs := mappingValue(wd, "inputs"); defs != nil && defs.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(defs.Content); i += 2 {
					inputs[defs.Content[i].Value] = decodeInputDef(defs.Content[i+1])
				}
			}
		}
	}
	return triggers, inputs, nil
}

func documentRoot(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0]
	}
	return n
}

// mappingValue finds a key by its literal text. Matching is case-insensitive
// because YAML keys in the wild are not consistent, and `On:` is legal.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if strings.EqualFold(m.Content[i].Value, key) {
			return m.Content[i+1]
		}
	}
	return nil
}

func decodeInputDef(n *yaml.Node) InputDef {
	var def InputDef
	if n == nil || n.Kind != yaml.MappingNode {
		return def
	}
	if v := mappingValue(n, "description"); v != nil {
		def.Description = v.Value
	}
	if v := mappingValue(n, "required"); v != nil {
		def.Required = strings.EqualFold(v.Value, "true")
	}
	if v := mappingValue(n, "type"); v != nil {
		def.Type = v.Value
	}
	if v := mappingValue(n, "default"); v != nil {
		def.Default = v.Value
	}
	if v := mappingValue(n, "options"); v != nil && v.Kind == yaml.SequenceNode {
		for _, item := range v.Content {
			def.Options = append(def.Options, item.Value)
		}
	}
	return def
}

// StaleWorkflows returns the entries in a surface that are past their TTL and
// should be revalidated in the background (stale-while-revalidate).
func (s *RepoSurface) StaleWorkflows(ttl time.Duration, now time.Time) []WorkflowCacheEntry {
	var out []WorkflowCacheEntry
	for _, e := range s.Workflows {
		if e.Stale(ttl, now) {
			out = append(out, e)
		}
	}
	return out
}

// BootstrapResult is one repo's bootstrap outcome.
type BootstrapResult struct {
	Repo    Repo
	Surface *RepoSurface
	History *HistoryResult
	// Err is non-nil when this repo could not be bootstrapped. A
	// bootstrap failure never blocks startup — the gateway keeps serving with
	// whatever the persistent cache already holds, which is the whole point
	// of a product that has to work while GitHub is broken.
	Err error
}

// Bootstrap runs surface then history discovery for each target repo. It is background work: every call runs in LaneInventory and
// pauses per installation below the quota threshold.
//
// It never returns early on a per-repo failure; the caller gets one result per
// repo and decides what to surface.
func (c *Client) Bootstrap(ctx context.Context, repos []Repo) []BootstrapResult {
	out := make([]BootstrapResult, 0, len(repos))
	for _, repo := range repos {
		res := BootstrapResult{Repo: repo}
		surface, err := c.DiscoverSurface(ctx, repo)
		if err != nil {
			res.Err = err
			out = append(out, res)
			if ctx.Err() != nil {
				return out
			}
			continue
		}
		res.Surface = surface

		history, err := c.DiscoverHistory(ctx, repo, c.cfg.InventoryHistoryDays)
		res.History = history
		res.Err = err
		out = append(out, res)
		if ctx.Err() != nil {
			return out
		}
	}
	return out
}
