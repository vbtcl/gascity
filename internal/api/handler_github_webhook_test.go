package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func TestGitHubWebhookRejectsBadSignature(t *testing.T) {
	state := newFakeState(t)
	configureGitHubWebhookMonitor(t, state)
	h := newTestCityHandler(t, state)

	body := githubPullRequestPayload("partcleda", "partcl", "main", "clean")
	req := newSignedGitHubWebhookRequest(t, state, "pull_request", "wrong-secret", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "signature") {
		t.Fatalf("body = %q, want signature error", rec.Body.String())
	}
	if got := len(state.eventProv.(*events.Fake).Events); got != 0 {
		t.Fatalf("events recorded = %d, want 0", got)
	}
}

func TestGitHubWebhookRejectsUnknownRepo(t *testing.T) {
	state := newFakeState(t)
	configureGitHubWebhookMonitor(t, state)
	h := newTestCityHandler(t, state)

	body := githubPullRequestPayload("unknown", "repo", "main", "clean")
	req := newSignedGitHubWebhookRequest(t, state, "pull_request", "secret", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if got := len(state.eventProv.(*events.Fake).Events); got != 0 {
		t.Fatalf("events recorded = %d, want 0", got)
	}
}

func TestGitHubWebhookRejectsUnconfiguredBaseBranch(t *testing.T) {
	state := newFakeState(t)
	configureGitHubWebhookMonitor(t, state)
	h := newTestCityHandler(t, state)

	body := githubPullRequestPayload("partcleda", "partcl", "release", "clean")
	req := newSignedGitHubWebhookRequest(t, state, "pull_request", "secret", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if got := len(state.eventProv.(*events.Fake).Events); got != 0 {
		t.Fatalf("events recorded = %d, want 0", got)
	}
}

func TestGitHubWebhookRecordsPullRequestUpdatedEvent(t *testing.T) {
	state := newFakeState(t)
	configureGitHubWebhookMonitor(t, state)
	h := newTestCityHandler(t, state)

	body := githubPullRequestPayload("partcleda", "partcl", "main", "clean")
	req := newSignedGitHubWebhookRequest(t, state, "pull_request", "secret", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertGitHubWebhookAccepted(t, rec, events.GitHubPRUpdated)
	recorded := assertOneGitHubEvent(t, state, events.GitHubPRUpdated)
	var payload GitHubPREventPayload
	decodeEventPayload(t, recorded.Payload, &payload)
	if payload.Monitor != "partcl-main" || payload.Owner != "partcleda" || payload.Repo != "partcl" {
		t.Fatalf("payload repo = %+v, want configured repo", payload)
	}
	if payload.PRNumber != 42 || payload.BaseBranch != "main" || payload.HeadSHA != "abc123" {
		t.Fatalf("payload PR = %+v, want PR #42 main abc123", payload)
	}
}

func TestGitHubWebhookRecordsPullRequestConflictedEvent(t *testing.T) {
	state := newFakeState(t)
	configureGitHubWebhookMonitor(t, state)
	h := newTestCityHandler(t, state)

	body := githubPullRequestPayload("partcleda", "partcl", "main", "dirty")
	req := newSignedGitHubWebhookRequest(t, state, "pull_request", "secret", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertGitHubWebhookAccepted(t, rec, events.GitHubPRConflicted)
	recorded := assertOneGitHubEvent(t, state, events.GitHubPRConflicted)
	var payload GitHubPREventPayload
	decodeEventPayload(t, recorded.Payload, &payload)
	if payload.FailureKind != "merge_conflict" || payload.MergeableState != "dirty" {
		t.Fatalf("payload = %+v, want merge conflict dirty", payload)
	}
}

func TestGitHubWebhookRecordsCheckSuiteFailureEvent(t *testing.T) {
	state := newFakeState(t)
	configureGitHubWebhookMonitor(t, state)
	h := newTestCityHandler(t, state)

	body := `{
		"action": "completed",
		"repository": {"name": "partcl", "full_name": "partcleda/partcl", "owner": {"login": "partcleda"}},
		"check_suite": {
			"id": 1001,
			"head_branch": "feature/webhook",
			"head_sha": "def456",
			"status": "completed",
			"conclusion": "failure",
			"html_url": "https://github.com/partcleda/partcl/actions/runs/1001",
			"pull_requests": [{
				"number": 42,
				"url": "https://api.github.com/repos/partcleda/partcl/pulls/42",
				"html_url": "https://github.com/partcleda/partcl/pull/42",
				"head": {"ref": "feature/webhook", "sha": "def456", "repo": {"full_name": "partcleda/partcl"}},
				"base": {"ref": "main", "sha": "base456", "repo": {"full_name": "partcleda/partcl"}}
			}]
		},
		"sender": {"login": "octocat"}
	}`
	req := newSignedGitHubWebhookRequest(t, state, "check_suite", "secret", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertGitHubWebhookAccepted(t, rec, events.GitHubPRCheckFailed)
	recorded := assertOneGitHubEvent(t, state, events.GitHubPRCheckFailed)
	var payload GitHubPREventPayload
	decodeEventPayload(t, recorded.Payload, &payload)
	if payload.CheckName != "check_suite" || payload.CheckConclusion != "failure" {
		t.Fatalf("payload check = %+v, want failed check_suite", payload)
	}
}

func TestGitHubWebhookRecordsCheckRunFailureForMatchingBaseBranch(t *testing.T) {
	state := newFakeState(t)
	configureGitHubWebhookMonitor(t, state)
	h := newTestCityHandler(t, state)

	body := `{
		"action": "completed",
		"repository": {"name": "partcl", "full_name": "partcleda/partcl", "owner": {"login": "partcleda"}},
		"check_run": {
			"id": 1002,
			"name": "unit-tests",
			"head_branch": "feature/webhook",
			"head_sha": "run789",
			"status": "completed",
			"conclusion": "failure",
			"html_url": "https://github.com/partcleda/partcl/actions/runs/1002",
			"pull_requests": [
				{
					"number": 7,
					"html_url": "https://github.com/partcleda/partcl/pull/7",
					"head": {"ref": "feature/release", "sha": "rel789", "repo": {"full_name": "partcleda/partcl"}},
					"base": {"ref": "release", "sha": "base-release", "repo": {"full_name": "partcleda/partcl"}}
				},
				{
					"number": 42,
					"html_url": "https://github.com/partcleda/partcl/pull/42",
					"head": {"ref": "feature/webhook", "sha": "run789", "repo": {"full_name": "partcleda/partcl"}},
					"base": {"ref": "main", "sha": "base-main", "repo": {"full_name": "partcleda/partcl"}}
				}
			]
		},
		"sender": {"login": "octocat"}
	}`
	req := newSignedGitHubWebhookRequest(t, state, "check_run", "secret", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertGitHubWebhookAccepted(t, rec, events.GitHubPRCheckFailed)
	recorded := assertOneGitHubEvent(t, state, events.GitHubPRCheckFailed)
	var payload GitHubPREventPayload
	decodeEventPayload(t, recorded.Payload, &payload)
	if payload.PRNumber != 42 || payload.BaseBranch != "main" || payload.BaseSHA != "base-main" {
		t.Fatalf("payload PR = %+v, want main PR #42", payload)
	}
	if payload.CheckName != "unit-tests" || payload.HeadSHA != "run789" {
		t.Fatalf("payload check = %+v, want unit-tests on run789", payload)
	}
}

func TestGitHubWebhookRecordsMergeGroupFailureEvent(t *testing.T) {
	state := newFakeState(t)
	configureGitHubWebhookMonitor(t, state)
	h := newTestCityHandler(t, state)

	body := `{
		"action": "destroyed",
		"repository": {"name": "partcl", "full_name": "partcleda/partcl", "owner": {"login": "partcleda"}},
		"merge_group": {
			"head_sha": "queue123",
			"head_ref": "gh-readonly-queue/main/pr-42",
			"base_ref": "refs/heads/main",
			"web_url": "https://github.com/partcleda/partcl/pull/42"
		},
		"sender": {"login": "octocat"}
	}`
	req := newSignedGitHubWebhookRequest(t, state, "merge_group", "secret", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertGitHubWebhookAccepted(t, rec, events.GitHubMergeGroupFailed)
	recorded := assertOneGitHubEvent(t, state, events.GitHubMergeGroupFailed)
	var payload GitHubMergeGroupEventPayload
	decodeEventPayload(t, recorded.Payload, &payload)
	if payload.HeadSHA != "queue123" || payload.BaseBranch != "main" || payload.FailureKind != "merge_group_failed" {
		t.Fatalf("payload = %+v, want failed merge group on main", payload)
	}
}

func configureGitHubWebhookMonitor(t *testing.T, state *fakeState) {
	t.Helper()
	t.Setenv("GC_TEST_GITHUB_WEBHOOK_SECRET", "secret")
	state.cfg.GitHub.PRMonitors = []config.GitHubPRMonitor{{
		Name:             "partcl-main",
		Owner:            "partcleda",
		Repo:             "partcl",
		BaseBranches:     []string{"main"},
		Rig:              "myrig",
		RepairRoute:      "partcl/polecat",
		WebhookSecretEnv: "GC_TEST_GITHUB_WEBHOOK_SECRET",
		MergeQueuePolicy: "repair",
	}}
}

func newSignedGitHubWebhookRequest(t *testing.T, state *fakeState, event, secret, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, cityURL(state, "/github/webhook"), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "delivery-123")
	req.Header.Set("X-Hub-Signature-256", "sha256="+githubWebhookSignature(secret, []byte(body)))
	return req
}

func githubWebhookSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func githubPullRequestPayload(owner, repo, base, mergeableState string) string {
	return `{
		"action": "synchronize",
		"repository": {"name": "` + repo + `", "full_name": "` + owner + `/` + repo + `", "owner": {"login": "` + owner + `"}},
		"pull_request": {
			"number": 42,
			"html_url": "https://github.com/` + owner + `/` + repo + `/pull/42",
			"title": "Wire webhook ingestion",
			"state": "open",
			"draft": false,
			"mergeable": true,
			"mergeable_state": "` + mergeableState + `",
			"head": {"ref": "feature/webhook", "sha": "abc123", "repo": {"full_name": "` + owner + `/` + repo + `"}},
			"base": {"ref": "` + base + `", "sha": "base123", "repo": {"full_name": "` + owner + `/` + repo + `"}}
		},
		"sender": {"login": "octocat"}
	}`
}

func assertGitHubWebhookAccepted(t *testing.T, rec *httptest.ResponseRecorder, eventType string) {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp GitHubWebhookResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Recorded || resp.EventType != eventType || resp.Monitor != "partcl-main" {
		t.Fatalf("response = %+v, want recorded %s for partcl-main", resp, eventType)
	}
}

func assertOneGitHubEvent(t *testing.T, state *fakeState, eventType string) events.Event {
	t.Helper()
	recorded := state.eventProv.(*events.Fake).Events
	if len(recorded) != 1 {
		t.Fatalf("events recorded = %d, want 1: %+v", len(recorded), recorded)
	}
	if recorded[0].Type != eventType {
		t.Fatalf("event type = %q, want %q; event=%+v", recorded[0].Type, eventType, recorded[0])
	}
	if recorded[0].Actor != "github" {
		t.Fatalf("event actor = %q, want github", recorded[0].Actor)
	}
	return recorded[0]
}

func decodeEventPayload(t *testing.T, data json.RawMessage, out any) {
	t.Helper()
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode payload: %v; payload=%s", err, string(data))
	}
}
