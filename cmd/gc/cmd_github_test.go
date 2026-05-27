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
	oldDispatch := dispatchGitHubPRRepairWorkflow
	var dispatchCalls []githubPRRepairDispatchRequest
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
	dispatchGitHubPRRepairWorkflow = func(_ context.Context, req githubPRRepairDispatchRequest) (githubPRRepairDispatchResult, error) {
		dispatchCalls = append(dispatchCalls, req)
		if req.Workflow != "mol-polecat-work" {
			t.Fatalf("dispatch workflow = %q, want mol-polecat-work", req.Workflow)
		}
		if req.Route != "partcl/polecat" {
			t.Fatalf("dispatch route = %q, want partcl/polecat", req.Route)
		}
		wispID := "mol-" + req.Bead.ID
		if err := req.Store.SetMetadata(req.Bead.ID, "gc.routed_to", req.Route); err != nil {
			return githubPRRepairDispatchResult{}, err
		}
		if err := req.Store.SetMetadata(req.Bead.ID, "molecule_id", wispID); err != nil {
			return githubPRRepairDispatchResult{}, err
		}
		return githubPRRepairDispatchResult{Dispatched: true, Workflow: req.Workflow, WispRootID: wispID}, nil
	}
	t.Cleanup(func() {
		resolveGitHubTokenForBackfill = oldToken
		newGitHubPRBackfillClient = oldClient
		openGitHubPRRepairStore = oldStore
		dispatchGitHubPRRepairWorkflow = oldDispatch
	})

	var firstPayload struct {
		OK                bool                 `json:"ok"`
		ExistingRepairs   int                  `json:"existing_repairs"`
		CreatedRepairs    int                  `json:"created_repairs"`
		UpdatedRepairs    int                  `json:"updated_repairs"`
		DispatchedRepairs int                  `json:"dispatched_repairs"`
		RepairBeads       []githubPRRepairBead `json:"repair_beads"`
	}
	for i := 0; i < 2; i++ {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--city", cityPath, "github", "pr", "backfill", "partcl-main", "--create-repair-beads", "--json"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run %d code = %d, stdout = %q, stderr = %q", i, code, stdout.String(), stderr.String())
		}
		if i == 0 {
			if err := json.Unmarshal(stdout.Bytes(), &firstPayload); err != nil {
				t.Fatalf("decode first stdout %q: %v", stdout.String(), err)
			}
		}
	}
	if !firstPayload.OK {
		t.Fatal("ok = false, want true")
	}
	if firstPayload.CreatedRepairs != 1 || firstPayload.ExistingRepairs != 0 || firstPayload.DispatchedRepairs != 1 || firstPayload.UpdatedRepairs != 0 {
		t.Fatalf("first repair counts = created %d existing %d updated %d dispatched %d, want 1/0/0/1",
			firstPayload.CreatedRepairs, firstPayload.ExistingRepairs, firstPayload.UpdatedRepairs, firstPayload.DispatchedRepairs)
	}
	if len(firstPayload.RepairBeads) != 1 {
		t.Fatalf("first repair beads = %#v, want one", firstPayload.RepairBeads)
	}
	if got := firstPayload.RepairBeads[0]; !got.Created || got.Updated || !got.Dispatched || got.Workflow != "mol-polecat-work" || got.WispRootID == "" {
		t.Fatalf("first repair bead = %#v, want created dispatch with workflow", got)
	}
	if len(dispatchCalls) != 1 {
		t.Fatalf("dispatch calls = %d, want one idempotent dispatch across repeated runs", len(dispatchCalls))
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
	if got := created[0].Metadata["molecule_id"]; got == "" {
		t.Fatalf("molecule_id = empty, want attached workflow metadata")
	}
}

func TestGitHubPRBackfillCommandUpdatesAndDispatchesExistingRepairBead(t *testing.T) {
	cityPath := writeGitHubMonitorTestCity(t)
	store := beads.NewMemStore()
	existing, err := store.Create(beads.Bead{
		Title: "Existing repair",
		Type:  "task",
		Metadata: map[string]string{
			"source":              "github-pr-monitor",
			"github.owner":        "partcleda",
			"github.repo":         "partcl",
			"github.pr":           "2560",
			"github.head_sha":     "abc123",
			"github.failure_kind": "checks_failed",
		},
	})
	if err != nil {
		t.Fatalf("Create existing repair: %v", err)
	}
	oldToken := resolveGitHubTokenForBackfill
	oldClient := newGitHubPRBackfillClient
	oldStore := openGitHubPRRepairStore
	oldDispatch := dispatchGitHubPRRepairWorkflow
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
	dispatchGitHubPRRepairWorkflow = func(_ context.Context, req githubPRRepairDispatchRequest) (githubPRRepairDispatchResult, error) {
		if req.Bead.ID != existing.ID {
			t.Fatalf("dispatch bead = %q, want existing %q", req.Bead.ID, existing.ID)
		}
		wispID := "mol-" + req.Bead.ID
		if err := req.Store.SetMetadata(req.Bead.ID, "gc.routed_to", req.Route); err != nil {
			return githubPRRepairDispatchResult{}, err
		}
		if err := req.Store.SetMetadata(req.Bead.ID, "molecule_id", wispID); err != nil {
			return githubPRRepairDispatchResult{}, err
		}
		return githubPRRepairDispatchResult{Dispatched: true, Workflow: req.Workflow, WispRootID: wispID}, nil
	}
	t.Cleanup(func() {
		resolveGitHubTokenForBackfill = oldToken
		newGitHubPRBackfillClient = oldClient
		openGitHubPRRepairStore = oldStore
		dispatchGitHubPRRepairWorkflow = oldDispatch
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "github", "pr", "backfill", "partcl-main", "--create-repair-beads", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var payload struct {
		ExistingRepairs   int                  `json:"existing_repairs"`
		CreatedRepairs    int                  `json:"created_repairs"`
		UpdatedRepairs    int                  `json:"updated_repairs"`
		DispatchedRepairs int                  `json:"dispatched_repairs"`
		RepairBeads       []githubPRRepairBead `json:"repair_beads"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode stdout %q: %v", stdout.String(), err)
	}
	if payload.CreatedRepairs != 0 || payload.ExistingRepairs != 1 || payload.UpdatedRepairs != 1 || payload.DispatchedRepairs != 1 {
		t.Fatalf("repair counts = created %d existing %d updated %d dispatched %d, want 0/1/1/1",
			payload.CreatedRepairs, payload.ExistingRepairs, payload.UpdatedRepairs, payload.DispatchedRepairs)
	}
	if len(payload.RepairBeads) != 1 {
		t.Fatalf("repair beads = %#v, want one", payload.RepairBeads)
	}
	if got := payload.RepairBeads[0]; got.ID != existing.ID || got.Created || !got.Updated || !got.Dispatched || got.Workflow != "mol-polecat-work" {
		t.Fatalf("repair bead = %#v, want existing updated dispatch", got)
	}
	updated, err := store.Get(existing.ID)
	if err != nil {
		t.Fatalf("Get existing: %v", err)
	}
	if got := updated.Metadata["github.monitor"]; got != "partcl-main" {
		t.Fatalf("github.monitor = %q, want partcl-main", got)
	}
	if got := updated.Metadata["gc.routed_to"]; got != "partcl/polecat" {
		t.Fatalf("gc.routed_to = %q, want partcl/polecat", got)
	}
	if got := updated.Metadata["molecule_id"]; got == "" {
		t.Fatalf("molecule_id = empty, want dispatch attachment")
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

[[agent]]
name = "polecat"
dir = "partcl"
default_sling_formula = "mol-polecat-work"

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
