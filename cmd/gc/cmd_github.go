package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/githubmonitor"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/spf13/cobra"
)

type githubPRLister interface {
	ListOpenPullRequests(context.Context, string, string) ([]githubmonitor.PullRequest, error)
}

var (
	newGitHubPRBackfillClient = func(token string) githubPRLister {
		return githubmonitor.NewGraphQLClient(token)
	}
	resolveGitHubTokenForBackfill = resolveGitHubToken
	openGitHubPRRepairStore       = func(cityPath, scopeRoot string) (beads.Store, error) {
		return openStoreAtForCity(scopeRoot, cityPath)
	}
	dispatchGitHubPRRepairWorkflow = dispatchGitHubPRRepairWorkflowDefault
)

type githubPRBackfillOptions struct {
	monitorName    string
	jsonOutput     bool
	includeClean   bool
	timeout        time.Duration
	actionableOnly bool
	createRepairs  bool
}

type githubPRBackfillResult struct {
	SchemaVersion     string                 `json:"schema_version"`
	CityPath          string                 `json:"city_path"`
	MonitorCount      int                    `json:"monitor_count"`
	ResultCount       int                    `json:"result_count"`
	ActionableCount   int                    `json:"actionable_count"`
	Results           []githubmonitor.Result `json:"results"`
	RepairBeads       []githubPRRepairBead   `json:"repair_beads,omitempty"`
	ExistingRepairs   int                    `json:"existing_repairs,omitempty"`
	CreatedRepairs    int                    `json:"created_repairs,omitempty"`
	UpdatedRepairs    int                    `json:"updated_repairs,omitempty"`
	DispatchedRepairs int                    `json:"dispatched_repairs,omitempty"`
}

type githubPRRepairBead struct {
	ID         string `json:"id"`
	PR         int    `json:"pr"`
	URL        string `json:"url,omitempty"`
	Created    bool   `json:"created"`
	Updated    bool   `json:"updated"`
	Dispatched bool   `json:"dispatched"`
	Route      string `json:"route,omitempty"`
	Workflow   string `json:"workflow,omitempty"`
	WispRootID string `json:"wisp_root_id,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
}

type githubPRRepairEnsureResult struct {
	Bead       beads.Bead
	Created    bool
	Updated    bool
	Dispatched bool
	Route      string
	Workflow   string
	WispRootID string
	WorkflowID string
}

type githubPRRepairDispatchRequest struct {
	CityPath string
	Cfg      *config.City
	Store    beads.Store
	StoreDir string
	Bead     beads.Bead
	Target   config.Agent
	Route    string
	Workflow string
	Stderr   io.Writer
}

type githubPRRepairDispatchResult struct {
	Dispatched bool
	Workflow   string
	WispRootID string
	WorkflowID string
}

func newGitHubCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github",
		Short: "GitHub integration commands",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newGitHubPRCmd(stdout, stderr))
	return cmd
}

func newGitHubPRCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "GitHub pull-request monitor commands",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newGitHubPRBackfillCmd(stdout, stderr))
	return cmd
}

func newGitHubPRBackfillCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := githubPRBackfillOptions{
		timeout:        45 * time.Second,
		actionableOnly: true,
	}
	cmd := &cobra.Command{
		Use:   "backfill [monitor-name]",
		Short: "Query configured GitHub PR readiness monitors",
		Long: `Query configured GitHub PR readiness monitors.

The command reads [[github.pr_monitor]] entries from the resolved city
configuration, queries open pull requests from GitHub, and reports PRs that
need repair: failed checks, merge conflicts, blocked mergeability, or branches
behind their base. By default clean and pending-only PRs are omitted; pass
--all to include every observed PR.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.monitorName = args[0]
			}
			if opts.includeClean {
				opts.actionableOnly = false
			}
			if doGitHubPRBackfill(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.jsonOutput, "json", false, "emit JSON")
	cmd.Flags().BoolVar(&opts.includeClean, "all", false, "include clean and pending-only PRs")
	cmd.Flags().BoolVar(&opts.createRepairs, "create-repair-beads", false, "create deduped repair beads for actionable PRs and sling them to their repair workflow")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "GitHub query timeout")
	return cmd
}

func doGitHubPRBackfill(opts githubPRBackfillOptions, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc github pr backfill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, prov, err := loadConfigCommandCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc github pr backfill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !opts.jsonOutput {
		for _, warning := range prov.Warnings {
			fmt.Fprintf(stderr, "gc github pr backfill: warning: %s\n", warning) //nolint:errcheck // best-effort stderr
		}
	}

	monitors, err := selectGitHubPRMonitors(cfg, opts.monitorName)
	if err != nil {
		fmt.Fprintf(stderr, "gc github pr backfill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	token, err := resolveGitHubTokenForBackfill(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "gc github pr backfill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	client := newGitHubPRBackfillClient(token)

	result := githubPRBackfillResult{
		SchemaVersion: "1",
		CityPath:      cityPath,
		MonitorCount:  len(monitors),
	}
	for _, monitor := range monitors {
		prs, err := client.ListOpenPullRequests(ctx, strings.TrimSpace(monitor.Owner), strings.TrimSpace(monitor.Repo))
		if err != nil {
			fmt.Fprintf(stderr, "gc github pr backfill: monitor %q: %v\n", monitor.Name, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		evaluated := githubmonitor.EvaluatePullRequests(monitor, prs)
		for _, prResult := range evaluated {
			if prResult.Actionable {
				result.ActionableCount++
				if opts.createRepairs {
					repair, err := ensureGitHubPRRepairBead(ctx, cityPath, cfg, monitor, prResult, stderr)
					if err != nil {
						fmt.Fprintf(stderr, "gc github pr backfill: repair bead for %s/%s#%d: %v\n", prResult.Owner, prResult.Repo, prResult.Number, err) //nolint:errcheck // best-effort stderr
						return 1
					}
					result.RepairBeads = append(result.RepairBeads, githubPRRepairBead{
						ID:         repair.Bead.ID,
						PR:         prResult.Number,
						URL:        prResult.URL,
						Created:    repair.Created,
						Updated:    repair.Updated,
						Dispatched: repair.Dispatched,
						Route:      repair.Route,
						Workflow:   repair.Workflow,
						WispRootID: repair.WispRootID,
						WorkflowID: repair.WorkflowID,
					})
					if repair.Created {
						result.CreatedRepairs++
					} else {
						result.ExistingRepairs++
					}
					if repair.Updated {
						result.UpdatedRepairs++
					}
					if repair.Dispatched {
						result.DispatchedRepairs++
					}
				}
			}
			if opts.actionableOnly && !prResult.Actionable {
				continue
			}
			result.Results = append(result.Results, prResult)
		}
	}
	result.ResultCount = len(result.Results)
	sortGitHubPRBackfillResults(result.Results)

	if opts.jsonOutput {
		if writeCLIJSONLineOrExit(stdout, stderr, "gc github pr backfill", result) != 0 {
			return 1
		}
		return 0
	}
	writeGitHubPRBackfillText(stdout, result)
	return 0
}

func ensureGitHubPRRepairBead(ctx context.Context, cityPath string, cfg *config.City, monitor config.GitHubPRMonitor, result githubmonitor.Result, stderr io.Writer) (githubPRRepairEnsureResult, error) {
	if !result.Actionable {
		return githubPRRepairEnsureResult{}, errors.New("result is not actionable")
	}
	rig, ok := rigByName(cfg, strings.TrimSpace(monitor.Rig))
	if !ok {
		return githubPRRepairEnsureResult{}, fmt.Errorf("rig %q not found", monitor.Rig)
	}
	target, ok := resolveAgentIdentity(cfg, strings.TrimSpace(result.RepairRoute), strings.TrimSpace(monitor.Rig))
	if !ok {
		return githubPRRepairEnsureResult{}, fmt.Errorf("repair route %q not found in config", result.RepairRoute)
	}
	route := agentutil.NormalizePoolRouteTarget(cfg, target.QualifiedName())
	workflow := strings.TrimSpace(target.EffectiveDefaultSlingFormula())

	scopeRoot := resolveStoreScopeRoot(cityPath, rig.Path)
	store, err := openGitHubPRRepairStore(cityPath, scopeRoot)
	if err != nil {
		return githubPRRepairEnsureResult{}, err
	}

	filters := githubPRRepairDedupeMetadata(result)
	existing, err := store.ListByMetadata(filters, 1)
	if err != nil {
		return githubPRRepairEnsureResult{}, fmt.Errorf("checking existing repair beads: %w", err)
	}
	var (
		repair  beads.Bead
		created bool
		updated bool
	)
	if len(existing) > 0 {
		repair = existing[0]
		updated, err = refreshGitHubPRRepairMetadata(store, &repair, githubPRRepairMetadata(result))
		if err != nil {
			return githubPRRepairEnsureResult{}, err
		}
	} else {
		priority := 1
		repair, err = store.Create(beads.Bead{
			Title:       githubPRRepairTitle(result),
			Type:        "task",
			Priority:    &priority,
			Description: githubPRRepairDescription(result),
			Labels:      []string{"github", "ci", "repair", "pr-monitor"},
			Metadata:    githubPRRepairMetadata(result),
		})
		if err != nil {
			return githubPRRepairEnsureResult{}, fmt.Errorf("creating repair bead: %w", err)
		}
		created = true
	}

	if githubPRRepairAlreadyDispatched(repair) {
		routeUpdated, err := ensureGitHubPRRepairRoute(store, &repair, route)
		if err != nil {
			return githubPRRepairEnsureResult{}, err
		}
		return githubPRRepairEnsureResult{
			Bead:     repair,
			Created:  created,
			Updated:  updated || routeUpdated,
			Route:    route,
			Workflow: workflow,
		}, nil
	}

	dispatch, err := dispatchGitHubPRRepairWorkflow(ctx, githubPRRepairDispatchRequest{
		CityPath: cityPath,
		Cfg:      cfg,
		Store:    store,
		StoreDir: scopeRoot,
		Bead:     repair,
		Target:   target,
		Route:    route,
		Workflow: workflow,
		Stderr:   stderr,
	})
	if err != nil {
		return githubPRRepairEnsureResult{}, fmt.Errorf("slinging repair bead %s to %s: %w", repair.ID, route, err)
	}
	if dispatch.Workflow == "" {
		dispatch.Workflow = workflow
	}
	return githubPRRepairEnsureResult{
		Bead:       repair,
		Created:    created,
		Updated:    updated,
		Dispatched: dispatch.Dispatched,
		Route:      route,
		Workflow:   dispatch.Workflow,
		WispRootID: dispatch.WispRootID,
		WorkflowID: dispatch.WorkflowID,
	}, nil
}

func githubPRRepairDedupeMetadata(result githubmonitor.Result) map[string]string {
	return map[string]string{
		"source":              "github-pr-monitor",
		"github.owner":        result.Owner,
		"github.repo":         result.Repo,
		"github.pr":           strconv.Itoa(result.Number),
		"github.head_sha":     result.HeadSHA,
		"github.failure_kind": result.FailureKind,
	}
}

func githubPRRepairMetadata(result githubmonitor.Result) map[string]string {
	metadata := githubPRRepairDedupeMetadata(result)
	metadata["github.monitor"] = result.Monitor
	metadata["github.url"] = result.URL
	metadata["github.base"] = result.BaseRefName
	metadata["github.head"] = result.HeadRefName
	metadata["github.merge_state_status"] = result.MergeStateStatus
	metadata["github.state"] = result.State
	metadata["github.failed_checks"] = strings.Join(result.FailedChecks, "\n")
	metadata["github.pending_checks"] = strings.Join(result.PendingChecks, "\n")
	return metadata
}

func refreshGitHubPRRepairMetadata(store beads.Store, repair *beads.Bead, desired map[string]string) (bool, error) {
	if repair.Metadata == nil {
		repair.Metadata = make(map[string]string, len(desired))
	}
	changes := make(map[string]string)
	for key, value := range desired {
		if repair.Metadata[key] != value {
			changes[key] = value
		}
	}
	if len(changes) == 0 {
		return false, nil
	}
	if err := store.SetMetadataBatch(repair.ID, changes); err != nil {
		return false, fmt.Errorf("updating repair bead metadata: %w", err)
	}
	for key, value := range changes {
		repair.Metadata[key] = value
	}
	return true, nil
}

func githubPRRepairAlreadyDispatched(repair beads.Bead) bool {
	if repair.Metadata == nil {
		return false
	}
	return strings.TrimSpace(repair.Metadata["molecule_id"]) != "" || strings.TrimSpace(repair.Metadata["workflow_id"]) != ""
}

func ensureGitHubPRRepairRoute(store beads.Store, repair *beads.Bead, route string) (bool, error) {
	route = strings.TrimSpace(route)
	if route == "" {
		return false, nil
	}
	if repair.Metadata == nil {
		repair.Metadata = map[string]string{}
	}
	if strings.TrimSpace(repair.Metadata["gc.routed_to"]) == route {
		return false, nil
	}
	if err := store.SetMetadata(repair.ID, "gc.routed_to", route); err != nil {
		return false, fmt.Errorf("setting repair bead route: %w", err)
	}
	repair.Metadata["gc.routed_to"] = route
	return true, nil
}

func dispatchGitHubPRRepairWorkflowDefault(ctx context.Context, req githubPRRepairDispatchRequest) (githubPRRepairDispatchResult, error) {
	if err := ctx.Err(); err != nil {
		return githubPRRepairDispatchResult{}, err
	}
	if req.Cfg == nil {
		return githubPRRepairDispatchResult{}, errors.New("city config is required")
	}
	if req.Store == nil {
		return githubPRRepairDispatchResult{}, errors.New("repair bead store is required")
	}
	if strings.TrimSpace(req.Bead.ID) == "" {
		return githubPRRepairDispatchResult{}, errors.New("repair bead id is required")
	}
	stderr := req.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	cityName := loadedCityName(req.Cfg, req.CityPath)
	storeRef := workflowStoreRefForDir(req.StoreDir, req.CityPath, cityName, req.Cfg)
	storeEnv, err := slingStoreEnvWithError(req.Cfg, req.CityPath, req.StoreDir)
	if err != nil {
		return githubPRRepairDispatchResult{}, err
	}
	runner := SlingRunner(shellSlingRunner)
	if len(storeEnv) > 0 {
		runner = func(dir, command string, env map[string]string) (string, error) {
			return shellSlingRunner(dir, command, mergeGitHubPRRepairEnv(storeEnv, env))
		}
	}
	deps := slingDeps{
		CityName: cityName,
		CityPath: req.CityPath,
		Cfg:      req.Cfg,
		SP:       newSessionProvider(),
		Runner:   runner,
		Store:    req.Store,
		StoreRef: storeRef,
		SourceWorkflowStores: func() ([]sling.SourceWorkflowStore, error) {
			stores, skips, err := openSourceWorkflowStores(req.Cfg, req.CityPath, "")
			if len(skips) > 0 {
				fmt.Fprintln(stderr, "warning:", formatSourceWorkflowStoreSkips(skips)) //nolint:errcheck
			}
			if err != nil {
				return nil, err
			}
			out := make([]sling.SourceWorkflowStore, 0, len(stores))
			for _, storeView := range stores {
				out = append(out, sling.SourceWorkflowStore{
					Store:    storeView.store,
					StoreRef: workflowStoreRefForDir(storeView.path, req.CityPath, cityName, req.Cfg),
				})
			}
			return out, nil
		},
	}
	populateSlingDepsCallbacks(&deps)

	workflow := strings.TrimSpace(req.Workflow)
	result, err := sling.DoSling(slingOpts{
		Target:        req.Target,
		BeadOrFormula: req.Bead.ID,
		OnFormula:     workflow,
		NoFormula:     workflow == "",
		Nudge:         true,
		Force:         true,
		NoConvoy:      true,
	}, deps, req.Store)
	if err != nil {
		return githubPRRepairDispatchResult{}, err
	}
	if result.NudgeAgent != nil {
		doSlingNudge(result.NudgeAgent, cityName, req.CityPath, req.Cfg, deps.SP, req.Store, io.Discard, stderr)
	}
	if result.FormulaName != "" {
		workflow = result.FormulaName
	}
	return githubPRRepairDispatchResult{
		Dispatched: !result.Idempotent,
		Workflow:   workflow,
		WispRootID: result.WispRootID,
		WorkflowID: result.WorkflowID,
	}, nil
}

func mergeGitHubPRRepairEnv(left, right map[string]string) map[string]string {
	if len(left) == 0 {
		return right
	}
	merged := make(map[string]string, len(left)+len(right))
	for key, value := range left {
		merged[key] = value
	}
	for key, value := range right {
		merged[key] = value
	}
	return merged
}

func githubPRRepairTitle(result githubmonitor.Result) string {
	title := strings.TrimSpace(result.Title)
	if title == "" {
		title = result.URL
	}
	if title == "" {
		title = result.FailureKind
	}
	return fmt.Sprintf("Repair GitHub PR %s/%s#%d readiness: %s", result.Owner, result.Repo, result.Number, title)
}

func githubPRRepairDescription(result githubmonitor.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub PR readiness monitor %q found actionable work.\n\n", result.Monitor)
	fmt.Fprintf(&b, "Repository: %s/%s\n", result.Owner, result.Repo)
	fmt.Fprintf(&b, "PR: #%d", result.Number)
	if result.URL != "" {
		fmt.Fprintf(&b, " %s", result.URL)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Base: %s\n", result.BaseRefName)
	if result.HeadRefName != "" {
		fmt.Fprintf(&b, "Head: %s\n", result.HeadRefName)
	}
	if result.HeadSHA != "" {
		fmt.Fprintf(&b, "Head SHA: %s\n", result.HeadSHA)
	}
	fmt.Fprintf(&b, "State: %s\n", result.State)
	if result.FailureKind != "" {
		fmt.Fprintf(&b, "Failure kind: %s\n", result.FailureKind)
	}
	if result.MergeStateStatus != "" {
		fmt.Fprintf(&b, "GitHub merge state: %s\n", result.MergeStateStatus)
	}
	if len(result.FailedChecks) > 0 {
		b.WriteString("\nFailed checks:\n")
		for _, check := range result.FailedChecks {
			fmt.Fprintf(&b, "- %s\n", check)
		}
	}
	if len(result.PendingChecks) > 0 {
		b.WriteString("\nPending checks:\n")
		for _, check := range result.PendingChecks {
			fmt.Fprintf(&b, "- %s\n", check)
		}
	}
	if result.RepairRoute != "" {
		fmt.Fprintf(&b, "\nRoute: %s\n", result.RepairRoute)
	}
	return b.String()
}

func selectGitHubPRMonitors(cfg *config.City, name string) ([]config.GitHubPRMonitor, error) {
	if cfg == nil || len(cfg.GitHub.PRMonitors) == 0 {
		return nil, errors.New("no github.pr_monitor entries are configured")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return append([]config.GitHubPRMonitor(nil), cfg.GitHub.PRMonitors...), nil
	}
	for _, monitor := range cfg.GitHub.PRMonitors {
		if strings.TrimSpace(monitor.Name) == name {
			return []config.GitHubPRMonitor{monitor}, nil
		}
	}
	return nil, fmt.Errorf("github.pr_monitor %q not found", name)
}

func sortGitHubPRBackfillResults(results []githubmonitor.Result) {
	slices.SortFunc(results, func(a, b githubmonitor.Result) int {
		if c := strings.Compare(a.Monitor, b.Monitor); c != 0 {
			return c
		}
		if c := strings.Compare(a.Repo, b.Repo); c != 0 {
			return c
		}
		return a.Number - b.Number
	})
}

func writeGitHubPRBackfillText(stdout io.Writer, result githubPRBackfillResult) {
	if len(result.Results) == 0 {
		fmt.Fprintf(stdout, "No actionable GitHub PR readiness problems found across %d monitor(s).\n", result.MonitorCount) //nolint:errcheck
		return
	}
	for _, pr := range result.Results {
		fmt.Fprintf(stdout, "%s %s/%s#%d %s", pr.Monitor, pr.Owner, pr.Repo, pr.Number, pr.State) //nolint:errcheck
		if pr.FailureKind != "" {
			fmt.Fprintf(stdout, " %s", pr.FailureKind) //nolint:errcheck
		}
		if pr.MergeStateStatus != "" {
			fmt.Fprintf(stdout, " merge=%s", pr.MergeStateStatus) //nolint:errcheck
		}
		if len(pr.FailedChecks) > 0 {
			fmt.Fprintf(stdout, " failed=%s", strings.Join(pr.FailedChecks, ",")) //nolint:errcheck
		}
		if len(pr.PendingChecks) > 0 {
			fmt.Fprintf(stdout, " pending=%s", strings.Join(pr.PendingChecks, ",")) //nolint:errcheck
		}
		if pr.RepairRoute != "" {
			fmt.Fprintf(stdout, " route=%s", pr.RepairRoute) //nolint:errcheck
		}
		if pr.URL != "" {
			fmt.Fprintf(stdout, " url=%s", pr.URL) //nolint:errcheck
		}
		fmt.Fprintln(stdout) //nolint:errcheck
	}
}

func resolveGitHubToken(ctx context.Context) (string, error) {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token, nil
		}
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("GitHub token not found in GITHUB_TOKEN/GH_TOKEN and `gh auth token` failed: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("GitHub token not found: `gh auth token` returned empty output")
	}
	return token, nil
}
