package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

type githubRepositoryIdentity struct {
	Owner    string
	Repo     string
	FullName string
}

type githubWebhookResult struct {
	EventType     string
	Payload       events.Payload
	Subject       string
	Message       string
	Monitor       string
	IgnoredReason string
}

func (s *Server) humaHandleGitHubWebhook(_ context.Context, input *GitHubWebhookInput) (*GitHubWebhookOutput, error) {
	ep := s.state.EventProvider()
	if ep == nil {
		return nil, huma.Error503ServiceUnavailable("events not enabled")
	}
	eventName := strings.TrimSpace(input.Event)
	if !supportedGitHubWebhookEvent(eventName) {
		return nil, huma.Error400BadRequest("unsupported GitHub webhook event: " + eventName)
	}

	repo, err := githubRepositoryFromPayload(input.Body.Repository)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	baseBranches := githubWebhookBaseBranches(eventName, input.Body)
	monitor, baseBranch, ok := findGitHubWebhookMonitor(s.state.Config(), repo, baseBranches)
	if !ok {
		return nil, huma.Error404NotFound(githubMonitorNotFoundMessage(repo, baseBranches))
	}
	secret, err := githubWebhookSecret(monitor)
	if err != nil {
		return nil, err
	}
	if !validGitHubWebhookSignature(input.Signature256, secret, input.RawBody) {
		return nil, huma.Error401Unauthorized("github webhook signature invalid")
	}

	result, err := normalizeGitHubWebhook(eventName, input.DeliveryID, input.Body, repo, monitor, baseBranch)
	if err != nil {
		return nil, err
	}
	if result.IgnoredReason != "" {
		return &GitHubWebhookOutput{Body: GitHubWebhookResponse{
			Recorded: false,
			Status:   "ignored",
			Monitor:  result.Monitor,
			Reason:   result.IgnoredReason,
		}}, nil
	}

	payload, err := json.Marshal(result.Payload)
	if err != nil {
		return nil, huma.Error500InternalServerError("encoding GitHub event payload: " + err.Error())
	}
	ep.Record(events.Event{
		Type:    result.EventType,
		Actor:   "github",
		Subject: result.Subject,
		Message: result.Message,
		Payload: payload,
	})
	return &GitHubWebhookOutput{Body: GitHubWebhookResponse{
		Recorded:  true,
		Status:    "recorded",
		EventType: result.EventType,
		Monitor:   result.Monitor,
	}}, nil
}

func supportedGitHubWebhookEvent(eventName string) bool {
	switch strings.TrimSpace(eventName) {
	case "pull_request", "check_run", "check_suite", "merge_group":
		return true
	default:
		return false
	}
}

func githubRepositoryFromPayload(repo GitHubRepository) (githubRepositoryIdentity, error) {
	owner := strings.TrimSpace(repo.Owner.Login)
	if owner == "" {
		owner = strings.TrimSpace(repo.Owner.Name)
	}
	name := strings.TrimSpace(repo.Name)
	fullName := strings.TrimSpace(repo.FullName)
	if fullName != "" {
		if before, after, ok := strings.Cut(fullName, "/"); ok {
			if owner == "" {
				owner = before
			}
			if name == "" {
				name = after
			}
		}
	}
	if owner == "" || name == "" {
		return githubRepositoryIdentity{}, fmt.Errorf("github webhook repository owner and name are required")
	}
	if fullName == "" {
		fullName = owner + "/" + name
	}
	return githubRepositoryIdentity{Owner: owner, Repo: name, FullName: fullName}, nil
}

func githubWebhookBaseBranches(eventName string, payload GitHubWebhookPayload) []string {
	var branches []string
	switch eventName {
	case "pull_request":
		if payload.PullRequest != nil {
			branches = append(branches, payload.PullRequest.Base.Ref)
		}
	case "check_run":
		if payload.CheckRun != nil {
			for _, pr := range payload.CheckRun.PullRequests {
				branches = append(branches, pr.Base.Ref)
			}
			branches = append(branches, githubBaseFromMergeQueueRef(payload.CheckRun.HeadBranch))
		}
	case "check_suite":
		if payload.CheckSuite != nil {
			for _, pr := range payload.CheckSuite.PullRequests {
				branches = append(branches, pr.Base.Ref)
			}
			branches = append(branches, githubBaseFromMergeQueueRef(payload.CheckSuite.HeadBranch))
		}
	case "merge_group":
		if payload.MergeGroup != nil {
			branches = append(branches, payload.MergeGroup.BaseRef)
			branches = append(branches, githubBaseFromMergeQueueRef(payload.MergeGroup.HeadRef))
		}
	}
	return uniqueNonEmptyBranches(branches)
}

func githubBaseFromMergeQueueRef(ref string) string {
	ref = normalizeGitHubBranch(ref)
	const prefix = "gh-readonly-queue/"
	if !strings.HasPrefix(ref, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(ref, prefix)
	base, _, _ := strings.Cut(rest, "/")
	return base
}

func normalizeGitHubBranch(branch string) string {
	branch = strings.TrimSpace(branch)
	branch = strings.TrimPrefix(branch, "refs/heads/")
	return branch
}

func uniqueNonEmptyBranches(values []string) []string {
	seen := make(map[string]bool, len(values))
	branches := make([]string, 0, len(values))
	for _, value := range values {
		branch := normalizeGitHubBranch(value)
		if branch == "" {
			continue
		}
		key := strings.ToLower(branch)
		if seen[key] {
			continue
		}
		seen[key] = true
		branches = append(branches, branch)
	}
	return branches
}

func findGitHubWebhookMonitor(cfg *config.City, repo githubRepositoryIdentity, baseBranches []string) (config.GitHubPRMonitor, string, bool) {
	if cfg == nil {
		return config.GitHubPRMonitor{}, "", false
	}
	for _, monitor := range cfg.GitHub.PRMonitors {
		if !strings.EqualFold(strings.TrimSpace(monitor.Owner), repo.Owner) ||
			!strings.EqualFold(strings.TrimSpace(monitor.Repo), repo.Repo) {
			continue
		}
		for _, configuredBase := range monitor.BaseBranches {
			for _, base := range baseBranches {
				if strings.EqualFold(strings.TrimSpace(configuredBase), base) {
					return monitor, normalizeGitHubBranch(base), true
				}
			}
		}
	}
	return config.GitHubPRMonitor{}, "", false
}

func githubMonitorNotFoundMessage(repo githubRepositoryIdentity, baseBranches []string) string {
	if len(baseBranches) == 0 {
		return fmt.Sprintf("no GitHub PR monitor configured for %s", repo.FullName)
	}
	return fmt.Sprintf("no GitHub PR monitor configured for %s on base branch %s", repo.FullName, strings.Join(baseBranches, ","))
}

func githubWebhookSecret(monitor config.GitHubPRMonitor) (string, error) {
	envName := strings.TrimSpace(monitor.WebhookSecretEnv)
	if envName == "" {
		return "", huma.Error503ServiceUnavailable("github webhook monitor has no webhook_secret_env configured")
	}
	secret := os.Getenv(envName)
	if secret == "" {
		return "", huma.Error503ServiceUnavailable("github webhook secret environment variable is empty: " + envName)
	}
	return secret, nil
}

func validGitHubWebhookSignature(signatureHeader, secret string, body []byte) bool {
	signatureHeader = strings.TrimSpace(signatureHeader)
	const prefix = "sha256="
	if !strings.HasPrefix(signatureHeader, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signatureHeader, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	return hmac.Equal(got, want)
}

func normalizeGitHubWebhook(eventName, deliveryID string, payload GitHubWebhookPayload, repo githubRepositoryIdentity, monitor config.GitHubPRMonitor, baseBranch string) (githubWebhookResult, error) {
	switch eventName {
	case "pull_request":
		if payload.PullRequest == nil {
			return githubWebhookResult{}, huma.Error400BadRequest("pull_request payload is missing pull_request")
		}
		prPayload := githubPREventPayload(deliveryID, eventName, payload.Action, payload, repo, monitor, baseBranch, *payload.PullRequest)
		if githubPullRequestIsConflicted(*payload.PullRequest) {
			prPayload.FailureKind = "merge_conflict"
			return githubWebhookResult{
				EventType: events.GitHubPRConflicted,
				Payload:   prPayload,
				Subject:   githubPRSubject(repo, prPayload.PRNumber),
				Message:   fmt.Sprintf("%s PR #%d has merge conflicts", repo.FullName, prPayload.PRNumber),
				Monitor:   monitor.Name,
			}, nil
		}
		return githubWebhookResult{
			EventType: events.GitHubPRUpdated,
			Payload:   prPayload,
			Subject:   githubPRSubject(repo, prPayload.PRNumber),
			Message:   fmt.Sprintf("%s PR #%d updated", repo.FullName, prPayload.PRNumber),
			Monitor:   monitor.Name,
		}, nil
	case "check_run":
		if payload.CheckRun == nil {
			return githubWebhookResult{}, huma.Error400BadRequest("check_run payload is missing check_run")
		}
		return normalizeGitHubCheckRun(deliveryID, eventName, payload, repo, monitor, baseBranch, *payload.CheckRun)
	case "check_suite":
		if payload.CheckSuite == nil {
			return githubWebhookResult{}, huma.Error400BadRequest("check_suite payload is missing check_suite")
		}
		return normalizeGitHubCheckSuite(deliveryID, eventName, payload, repo, monitor, baseBranch, *payload.CheckSuite)
	case "merge_group":
		if payload.MergeGroup == nil {
			return githubWebhookResult{}, huma.Error400BadRequest("merge_group payload is missing merge_group")
		}
		return normalizeGitHubMergeGroup(deliveryID, eventName, payload, repo, monitor, baseBranch, *payload.MergeGroup)
	default:
		return githubWebhookResult{}, huma.Error400BadRequest("unsupported GitHub webhook event: " + eventName)
	}
}

func githubPREventPayload(deliveryID, eventName, action string, payload GitHubWebhookPayload, repo githubRepositoryIdentity, monitor config.GitHubPRMonitor, baseBranch string, pr GitHubPullRequest) GitHubPREventPayload {
	return GitHubPREventPayload{
		DeliveryID:     deliveryID,
		SourceEvent:    eventName,
		Action:         action,
		Monitor:        monitor.Name,
		Rig:            monitor.Rig,
		RepairRoute:    monitor.RepairRoute,
		Owner:          repo.Owner,
		Repo:           repo.Repo,
		FullName:       repo.FullName,
		PRNumber:       pr.Number,
		PRURL:          pr.HTMLURL,
		PRTitle:        pr.Title,
		PRState:        pr.State,
		BaseBranch:     baseBranch,
		BaseSHA:        pr.Base.SHA,
		HeadRef:        pr.Head.Ref,
		HeadSHA:        pr.Head.SHA,
		HeadRepo:       pr.Head.Repo.FullName,
		Draft:          pr.Draft,
		Mergeable:      pr.Mergeable,
		MergeableState: pr.MergeableState,
		Sender:         githubSender(payload),
	}
}

func githubPullRequestIsConflicted(pr GitHubPullRequest) bool {
	if pr.Mergeable != nil && !*pr.Mergeable {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(pr.MergeableState), "dirty")
}

func normalizeGitHubCheckRun(deliveryID, eventName string, payload GitHubWebhookPayload, repo githubRepositoryIdentity, monitor config.GitHubPRMonitor, baseBranch string, check GitHubCheckRun) (githubWebhookResult, error) {
	if !githubCheckConclusionFailed(check.Status, check.Conclusion) {
		return githubWebhookResult{Monitor: monitor.Name, IgnoredReason: "check_run did not fail"}, nil
	}
	if len(check.PullRequests) == 0 {
		return githubMergeQueueFailureFromCheck(deliveryID, eventName, payload.Action, payload, repo, monitor, baseBranch, check.HeadBranch, check.HeadSHA, check.HTMLURL)
	}
	pr := githubCheckPullRequestForBase(check.PullRequests, baseBranch)
	prPayload := githubPREventPayloadFromCheck(deliveryID, eventName, payload.Action, payload, repo, monitor, baseBranch, pr)
	prPayload.HeadSHA = firstNonEmpty(check.HeadSHA, prPayload.HeadSHA)
	prPayload.HeadRef = firstNonEmpty(check.HeadBranch, prPayload.HeadRef)
	prPayload.CheckName = firstNonEmpty(check.Name, "check_run")
	prPayload.CheckStatus = check.Status
	prPayload.CheckConclusion = check.Conclusion
	prPayload.CheckURL = check.HTMLURL
	prPayload.FailureKind = "check_failed"
	return githubWebhookResult{
		EventType: events.GitHubPRCheckFailed,
		Payload:   prPayload,
		Subject:   githubPRSubject(repo, prPayload.PRNumber),
		Message:   fmt.Sprintf("%s PR #%d check failed: %s", repo.FullName, prPayload.PRNumber, prPayload.CheckName),
		Monitor:   monitor.Name,
	}, nil
}

func normalizeGitHubCheckSuite(deliveryID, eventName string, payload GitHubWebhookPayload, repo githubRepositoryIdentity, monitor config.GitHubPRMonitor, baseBranch string, check GitHubCheckSuite) (githubWebhookResult, error) {
	if !githubCheckConclusionFailed(check.Status, check.Conclusion) {
		return githubWebhookResult{Monitor: monitor.Name, IgnoredReason: "check_suite did not fail"}, nil
	}
	if len(check.PullRequests) == 0 {
		return githubMergeQueueFailureFromCheck(deliveryID, eventName, payload.Action, payload, repo, monitor, baseBranch, check.HeadBranch, check.HeadSHA, check.HTMLURL)
	}
	pr := githubCheckPullRequestForBase(check.PullRequests, baseBranch)
	prPayload := githubPREventPayloadFromCheck(deliveryID, eventName, payload.Action, payload, repo, monitor, baseBranch, pr)
	prPayload.HeadSHA = firstNonEmpty(check.HeadSHA, prPayload.HeadSHA)
	prPayload.HeadRef = firstNonEmpty(check.HeadBranch, prPayload.HeadRef)
	prPayload.CheckName = "check_suite"
	prPayload.CheckStatus = check.Status
	prPayload.CheckConclusion = check.Conclusion
	prPayload.CheckURL = check.HTMLURL
	prPayload.FailureKind = "check_failed"
	return githubWebhookResult{
		EventType: events.GitHubPRCheckFailed,
		Payload:   prPayload,
		Subject:   githubPRSubject(repo, prPayload.PRNumber),
		Message:   fmt.Sprintf("%s PR #%d check suite failed", repo.FullName, prPayload.PRNumber),
		Monitor:   monitor.Name,
	}, nil
}

func githubCheckPullRequestForBase(prs []GitHubCheckPullRequest, baseBranch string) GitHubCheckPullRequest {
	for _, pr := range prs {
		if strings.EqualFold(normalizeGitHubBranch(pr.Base.Ref), normalizeGitHubBranch(baseBranch)) {
			return pr
		}
	}
	return prs[0]
}

func githubCheckConclusionFailed(status, conclusion string) bool {
	if !strings.EqualFold(strings.TrimSpace(status), "completed") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "failure", "timed_out", "canceled", "action_required", "startup_failure", "stale":
		return true
	default:
		return false
	}
}

func githubPREventPayloadFromCheck(deliveryID, eventName, action string, payload GitHubWebhookPayload, repo githubRepositoryIdentity, monitor config.GitHubPRMonitor, baseBranch string, pr GitHubCheckPullRequest) GitHubPREventPayload {
	return GitHubPREventPayload{
		DeliveryID:  deliveryID,
		SourceEvent: eventName,
		Action:      action,
		Monitor:     monitor.Name,
		Rig:         monitor.Rig,
		RepairRoute: monitor.RepairRoute,
		Owner:       repo.Owner,
		Repo:        repo.Repo,
		FullName:    repo.FullName,
		PRNumber:    pr.Number,
		PRURL:       firstNonEmpty(pr.HTMLURL, pr.URL),
		BaseBranch:  baseBranch,
		BaseSHA:     pr.Base.SHA,
		HeadRef:     pr.Head.Ref,
		HeadSHA:     pr.Head.SHA,
		HeadRepo:    pr.Head.Repo.FullName,
		Sender:      githubSender(payload),
	}
}

func githubMergeQueueFailureFromCheck(deliveryID, eventName, action string, payload GitHubWebhookPayload, repo githubRepositoryIdentity, monitor config.GitHubPRMonitor, baseBranch, headRef, headSHA, url string) (githubWebhookResult, error) {
	if monitor.MergeQueuePolicyOrDefault() == "ignore" {
		return githubWebhookResult{Monitor: monitor.Name, IgnoredReason: "merge queue policy is ignore"}, nil
	}
	mergePayload := githubMergeGroupEventPayload(deliveryID, eventName, action, payload, repo, monitor, baseBranch, headRef, headSHA, url)
	return githubWebhookResult{
		EventType: events.GitHubMergeGroupFailed,
		Payload:   mergePayload,
		Subject:   githubMergeGroupSubject(repo, headSHA),
		Message:   fmt.Sprintf("%s merge group failed on %s", repo.FullName, baseBranch),
		Monitor:   monitor.Name,
	}, nil
}

func normalizeGitHubMergeGroup(deliveryID, eventName string, payload GitHubWebhookPayload, repo githubRepositoryIdentity, monitor config.GitHubPRMonitor, baseBranch string, mergeGroup GitHubMergeGroup) (githubWebhookResult, error) {
	if monitor.MergeQueuePolicyOrDefault() == "ignore" {
		return githubWebhookResult{Monitor: monitor.Name, IgnoredReason: "merge queue policy is ignore"}, nil
	}
	if !githubMergeGroupActionFailed(payload.Action) {
		return githubWebhookResult{Monitor: monitor.Name, IgnoredReason: "merge_group did not fail"}, nil
	}
	mergePayload := githubMergeGroupEventPayload(deliveryID, eventName, payload.Action, payload, repo, monitor, baseBranch, mergeGroup.HeadRef, mergeGroup.HeadSHA, firstNonEmpty(mergeGroup.WebURL, mergeGroup.HTMLURL))
	return githubWebhookResult{
		EventType: events.GitHubMergeGroupFailed,
		Payload:   mergePayload,
		Subject:   githubMergeGroupSubject(repo, mergeGroup.HeadSHA),
		Message:   fmt.Sprintf("%s merge group failed on %s", repo.FullName, baseBranch),
		Monitor:   monitor.Name,
	}, nil
}

func githubMergeGroupActionFailed(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "destroyed", "dequeued", "failed", "failure":
		return true
	default:
		return false
	}
}

func githubMergeGroupEventPayload(deliveryID, eventName, action string, payload GitHubWebhookPayload, repo githubRepositoryIdentity, monitor config.GitHubPRMonitor, baseBranch, headRef, headSHA, url string) GitHubMergeGroupEventPayload {
	return GitHubMergeGroupEventPayload{
		DeliveryID:  deliveryID,
		SourceEvent: eventName,
		Action:      action,
		Monitor:     monitor.Name,
		Rig:         monitor.Rig,
		RepairRoute: monitor.RepairRoute,
		Owner:       repo.Owner,
		Repo:        repo.Repo,
		FullName:    repo.FullName,
		BaseBranch:  baseBranch,
		HeadRef:     headRef,
		HeadSHA:     headSHA,
		URL:         url,
		FailureKind: "merge_group_failed",
		Sender:      githubSender(payload),
	}
}

func githubSender(payload GitHubWebhookPayload) string {
	if payload.Sender == nil {
		return ""
	}
	return payload.Sender.Login
}

func githubPRSubject(repo githubRepositoryIdentity, number int) string {
	if number == 0 {
		return repo.FullName
	}
	return fmt.Sprintf("%s#%d", repo.FullName, number)
}

func githubMergeGroupSubject(repo githubRepositoryIdentity, headSHA string) string {
	if strings.TrimSpace(headSHA) == "" {
		return repo.FullName
	}
	return repo.FullName + "@" + headSHA
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
