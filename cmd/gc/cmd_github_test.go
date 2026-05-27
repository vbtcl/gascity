package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/githubmonitor"
)

type fakeGitHubPRLister struct {
	prs []githubmonitor.PullRequest
	err error
}

func (f fakeGitHubPRLister) ListOpenPullRequests(context.Context, string, string) ([]githubmonitor.PullRequest, error) {
	return f.prs, f.err
}

func TestGitHubPRBackfillCommandReportsActionableResults(t *testing.T) {
	cityPath := writeGitHubMonitorTestCity(t)
	oldToken := resolveGitHubTokenForBackfill
	oldClient := newGitHubPRBackfillClient
	resolveGitHubTokenForBackfill = func(context.Context) (string, error) { return "token", nil }
	newGitHubPRBackfillClient = func(token string) githubPRLister {
		if token != "token" {
			t.Fatalf("token = %q, want test token", token)
		}
		return fakeGitHubPRLister{prs: []githubmonitor.PullRequest{
			{
				Number:           2560,
				Title:            "Deploy",
				URL:              "https://github.com/partcleda/partcl/pull/2560",
				BaseRefName:      "main",
				HeadRefName:      "fix",
				HeadSHA:          "abc123",
				MergeStateStatus: "BLOCKED",
				Checks:           []githubmonitor.Check{{Name: "deploy", Status: "COMPLETED", Conclusion: "FAILURE"}},
			},
		}}
	}
	t.Cleanup(func() {
		resolveGitHubTokenForBackfill = oldToken
		newGitHubPRBackfillClient = oldClient
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "github", "pr", "backfill", "partcl-main", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var payload struct {
		MonitorCount    int `json:"monitor_count"`
		ResultCount     int `json:"result_count"`
		ActionableCount int `json:"actionable_count"`
		Results         []githubmonitor.Result
		OK              bool `json:"ok"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode stdout %q: %v", stdout.String(), err)
	}
	if !payload.OK {
		t.Fatal("ok = false, want true")
	}
	if payload.MonitorCount != 1 || payload.ResultCount != 1 || payload.ActionableCount != 1 {
		t.Fatalf("counts = monitors %d results %d actionable %d, want 1/1/1", payload.MonitorCount, payload.ResultCount, payload.ActionableCount)
	}
	if got := payload.Results[0]; got.Number != 2560 || got.State != githubmonitor.StateFailed || got.RepairRoute != "partcl/polecat" {
		t.Fatalf("result = %#v, want failing PR routed to partcl/polecat", got)
	}
}

func TestGitHubPRBackfillCommandCreatesDedupedRepairBeads(t *testing.T) {
	cityPath := writeGitHubMonitorTestCity(t)
	store := beads.NewMemStore()
	oldToken := resolveGitHubTokenForBackfill
	oldClient := newGitHubPRBackfillClient
	oldStore := openGitHubPRRepairStore
	resolveGitHubTokenForBackfill = func(context.Context) (string, error) { return "token", nil }
	newGitHubPRBackfillClient = func(string) githubPRLister {
		return fakeGitHubPRLister{prs: []githubmonitor.PullRequest{
			{
				Number:           2560,
				Title:            "Deploy",
				URL:              "https://github.com/partcleda/partcl/pull/2560",
				BaseRefName:      "main",
				HeadRefName:      "fix",
				HeadSHA:          "abc123",
				MergeStateStatus: "BLOCKED",
				Checks:           []githubmonitor.Check{{Name: "deploy", Status: "COMPLETED", Conclusion: "FAILURE"}},
			},
		}}
	}
	openGitHubPRRepairStore = func(string, string) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() {
		resolveGitHubTokenForBackfill = oldToken
		newGitHubPRBackfillClient = oldClient
		openGitHubPRRepairStore = oldStore
	})

	for i := 0; i < 2; i++ {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--city", cityPath, "github", "pr", "backfill", "partcl-main", "--create-repair-beads", "--json"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run %d code = %d, stdout = %q, stderr = %q", i, code, stdout.String(), stderr.String())
		}
	}

	created, err := store.ListByMetadata(map[string]string{
		"source":          "github-pr-monitor",
		"github.owner":    "partcleda",
		"github.repo":     "partcl",
		"github.pr":       "2560",
		"github.head_sha": "abc123",
	}, 0)
	if err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created repair beads = %#v, want one deduped bead", created)
	}
	if got := created[0].Metadata["gc.routed_to"]; got != "partcl/polecat" {
		t.Fatalf("gc.routed_to = %q, want partcl/polecat", got)
	}
	if !strings.Contains(created[0].Description, "deploy") {
		t.Fatalf("description = %q, want failed check detail", created[0].Description)
	}
}

func TestGitHubPRBackfillCommandCoalescesFailureKindTransition(t *testing.T) {
	cityPath := writeGitHubMonitorTestCity(t)
	store := beads.NewMemStore()
	sequences := [][]githubmonitor.PullRequest{
		{
			{
				Number:           2560,
				Title:            "Deploy",
				URL:              "https://github.com/partcleda/partcl/pull/2560",
				BaseRefName:      "main",
				HeadRefName:      "fix",
				HeadSHA:          "abc123",
				MergeStateStatus: "BLOCKED",
			},
		},
		{
			{
				Number:           2560,
				Title:            "Deploy",
				URL:              "https://github.com/partcleda/partcl/pull/2560",
				BaseRefName:      "main",
				HeadRefName:      "fix",
				HeadSHA:          "abc123",
				MergeStateStatus: "UNSTABLE",
				Checks:           []githubmonitor.Check{{Name: "unit", Status: "COMPLETED", Conclusion: "FAILURE"}},
			},
		},
	}
	calls := 0
	oldToken := resolveGitHubTokenForBackfill
	oldClient := newGitHubPRBackfillClient
	oldStore := openGitHubPRRepairStore
	resolveGitHubTokenForBackfill = func(context.Context) (string, error) { return "token", nil }
	newGitHubPRBackfillClient = func(string) githubPRLister {
		if calls >= len(sequences) {
			t.Fatalf("unexpected GitHub client call %d", calls)
		}
		prs := sequences[calls]
		calls++
		return fakeGitHubPRLister{prs: prs}
	}
	openGitHubPRRepairStore = func(string, string) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() {
		resolveGitHubTokenForBackfill = oldToken
		newGitHubPRBackfillClient = oldClient
		openGitHubPRRepairStore = oldStore
	})

	for i := 0; i < len(sequences); i++ {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--city", cityPath, "github", "pr", "backfill", "partcl-main", "--create-repair-beads", "--json"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run %d code = %d, stdout = %q, stderr = %q", i, code, stdout.String(), stderr.String())
		}
	}

	created, err := store.ListByMetadata(map[string]string{
		"source":          "github-pr-monitor",
		"github.owner":    "partcleda",
		"github.repo":     "partcl",
		"github.pr":       "2560",
		"github.head_sha": "abc123",
	}, 0)
	if err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created repair beads = %#v, want one bead across failure_kind transition", created)
	}
	got := created[0]
	if got.Metadata["github.failure_kind"] != githubmonitor.FailureKindChecksFailed {
		t.Fatalf("github.failure_kind = %q, want %q", got.Metadata["github.failure_kind"], githubmonitor.FailureKindChecksFailed)
	}
	if got.Metadata["github.failed_checks"] != "unit" {
		t.Fatalf("github.failed_checks = %q, want unit", got.Metadata["github.failed_checks"])
	}
	if !strings.Contains(got.Description, "unit") || !strings.Contains(got.Description, githubmonitor.FailureKindChecksFailed) {
		t.Fatalf("description = %q, want latest failed-check details", got.Description)
	}
}

func TestGitHubPRBackfillCommandNudgesInProgressRepairWorker(t *testing.T) {
	cityPath := writeGitHubMonitorTestCity(t)
	store := beads.NewMemStore()
	existing, err := store.Create(beads.Bead{
		Title: "Existing repair",
		Metadata: map[string]string{
			"source":              "github-pr-monitor",
			"github.owner":        "partcleda",
			"github.repo":         "partcl",
			"github.pr":           "2560",
			"github.head_sha":     "abc123",
			"github.failure_kind": githubmonitor.FailureKindBlocked,
			"gc.routed_to":        "partcl/polecat",
		},
	})
	if err != nil {
		t.Fatalf("Create existing: %v", err)
	}
	status := "in_progress"
	assignee := "partcl/furiosa"
	if err := store.Update(existing.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("mark existing in progress: %v", err)
	}

	oldToken := resolveGitHubTokenForBackfill
	oldClient := newGitHubPRBackfillClient
	oldStore := openGitHubPRRepairStore
	oldNudge := nudgeGitHubPRRepairWorker
	var nudges []struct {
		target  string
		message string
	}
	resolveGitHubTokenForBackfill = func(context.Context) (string, error) { return "token", nil }
	newGitHubPRBackfillClient = func(string) githubPRLister {
		return fakeGitHubPRLister{prs: []githubmonitor.PullRequest{
			{
				Number:           2560,
				Title:            "Deploy",
				URL:              "https://github.com/partcleda/partcl/pull/2560",
				BaseRefName:      "main",
				HeadRefName:      "fix",
				HeadSHA:          "abc123",
				MergeStateStatus: "UNSTABLE",
				Checks:           []githubmonitor.Check{{Name: "unit", Status: "COMPLETED", Conclusion: "FAILURE"}},
			},
		}}
	}
	openGitHubPRRepairStore = func(string, string) (beads.Store, error) {
		return store, nil
	}
	nudgeGitHubPRRepairWorker = func(target, message string) error {
		nudges = append(nudges, struct {
			target  string
			message string
		}{target: target, message: message})
		return nil
	}
	t.Cleanup(func() {
		resolveGitHubTokenForBackfill = oldToken
		newGitHubPRBackfillClient = oldClient
		openGitHubPRRepairStore = oldStore
		nudgeGitHubPRRepairWorker = oldNudge
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "github", "pr", "backfill", "partcl-main", "--create-repair-beads", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}

	created, err := store.ListByMetadata(map[string]string{
		"source":          "github-pr-monitor",
		"github.owner":    "partcleda",
		"github.repo":     "partcl",
		"github.pr":       "2560",
		"github.head_sha": "abc123",
	}, 0)
	if err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	if len(created) != 1 || created[0].ID != existing.ID {
		t.Fatalf("repair beads = %#v, want existing bead only", created)
	}
	if created[0].Metadata["github.failure_kind"] != githubmonitor.FailureKindChecksFailed {
		t.Fatalf("github.failure_kind = %q, want %q", created[0].Metadata["github.failure_kind"], githubmonitor.FailureKindChecksFailed)
	}
	if len(nudges) != 1 {
		t.Fatalf("nudges = %#v, want one worker nudge", nudges)
	}
	if nudges[0].target != assignee {
		t.Fatalf("nudge target = %q, want %q", nudges[0].target, assignee)
	}
	for _, want := range []string{existing.ID, "partcleda/partcl#2560", githubmonitor.FailureKindChecksFailed, "unit"} {
		if !strings.Contains(nudges[0].message, want) {
			t.Fatalf("nudge message = %q, want %q", nudges[0].message, want)
		}
	}
}

func TestGitHubPRBackfillCommandFiltersCleanResultsByDefault(t *testing.T) {
	cityPath := writeGitHubMonitorTestCity(t)
	oldToken := resolveGitHubTokenForBackfill
	oldClient := newGitHubPRBackfillClient
	resolveGitHubTokenForBackfill = func(context.Context) (string, error) { return "token", nil }
	newGitHubPRBackfillClient = func(string) githubPRLister {
		return fakeGitHubPRLister{prs: []githubmonitor.PullRequest{
			{Number: 1, BaseRefName: "main", MergeStateStatus: "CLEAN"},
		}}
	}
	t.Cleanup(func() {
		resolveGitHubTokenForBackfill = oldToken
		newGitHubPRBackfillClient = oldClient
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "github", "pr", "backfill", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), `"number":1`) {
		t.Fatalf("stdout = %s, clean PR should be filtered by default", stdout.String())
	}
}

func writeGitHubMonitorTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir .gc: %v", err)
	}
	body := `[workspace]
name = "test-city"

[[rigs]]
name = "partcl"
path = "partcl"
prefix = "pa"

[[github.pr_monitor]]
name = "partcl-main"
owner = "partcleda"
repo = "partcl"
base_branches = ["main"]
rig = "partcl"
repair_route = "partcl/polecat"
notify = ["gastown.mayor"]
poll_interval = "2m"
merge_queue = "repair"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, "partcl"), 0o755); err != nil {
		t.Fatalf("mkdir partcl: %v", err)
	}
	return cityPath
}

func TestGitHubPRBackfillCommandPropagatesRepairStoreError(t *testing.T) {
	cityPath := writeGitHubMonitorTestCity(t)
	oldToken := resolveGitHubTokenForBackfill
	oldClient := newGitHubPRBackfillClient
	oldStore := openGitHubPRRepairStore
	resolveGitHubTokenForBackfill = func(context.Context) (string, error) { return "token", nil }
	newGitHubPRBackfillClient = func(string) githubPRLister {
		return fakeGitHubPRLister{prs: []githubmonitor.PullRequest{{Number: 1, BaseRefName: "main", HeadSHA: "abc", MergeStateStatus: "DIRTY"}}}
	}
	openGitHubPRRepairStore = func(string, string) (beads.Store, error) {
		return nil, fmt.Errorf("store unavailable")
	}
	t.Cleanup(func() {
		resolveGitHubTokenForBackfill = oldToken
		newGitHubPRBackfillClient = oldClient
		openGitHubPRRepairStore = oldStore
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "github", "pr", "backfill", "--create-repair-beads"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run code = 0, want failure")
	}
	if !strings.Contains(stderr.String(), "store unavailable") {
		t.Fatalf("stderr = %q, want store error", stderr.String())
	}
}
