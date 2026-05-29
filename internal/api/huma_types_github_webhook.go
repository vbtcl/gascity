package api

// GitHubWebhookInput is the Huma input for
// POST /v0/city/{cityName}/github/webhook.
type GitHubWebhookInput struct {
	CityScope
	Event        string `header:"X-GitHub-Event" required:"true" minLength:"1" doc:"GitHub webhook event name."`
	DeliveryID   string `header:"X-GitHub-Delivery" required:"true" minLength:"1" doc:"GitHub delivery identifier."`
	Signature256 string `header:"X-Hub-Signature-256" required:"true" minLength:"1" doc:"GitHub SHA-256 HMAC signature in sha256=<hex> form."`
	RawBody      []byte
	Body         GitHubWebhookPayload
}

// GitHubWebhookPayload is the typed subset of supported GitHub repository
// webhook payloads. GitHub owns the source schema; Gas City decodes only the
// fields needed to normalize configured PR readiness events.
type GitHubWebhookPayload struct {
	Action      string               `json:"action,omitempty"`
	Repository  GitHubRepository     `json:"repository"`
	PullRequest *GitHubPullRequest   `json:"pull_request,omitempty"`
	CheckRun    *GitHubCheckRun      `json:"check_run,omitempty"`
	CheckSuite  *GitHubCheckSuite    `json:"check_suite,omitempty"`
	MergeGroup  *GitHubMergeGroup    `json:"merge_group,omitempty"`
	Sender      *GitHubWebhookSender `json:"sender,omitempty"`
}

// GitHubRepository is the repository subset shared across supported webhooks.
type GitHubRepository struct {
	Name     string                `json:"name,omitempty"`
	FullName string                `json:"full_name,omitempty"`
	Owner    GitHubRepositoryOwner `json:"owner,omitempty"`
}

// GitHubRepositoryOwner is the repository owner subset in GitHub webhooks.
type GitHubRepositoryOwner struct {
	Login string `json:"login,omitempty"`
	Name  string `json:"name,omitempty"`
}

// GitHubWebhookSender identifies the GitHub actor that triggered a webhook.
type GitHubWebhookSender struct {
	Login string `json:"login,omitempty"`
}

// GitHubPullRequest is the PR subset needed by readiness events.
type GitHubPullRequest struct {
	Number         int         `json:"number,omitempty"`
	HTMLURL        string      `json:"html_url,omitempty"`
	Title          string      `json:"title,omitempty"`
	State          string      `json:"state,omitempty"`
	Draft          bool        `json:"draft,omitempty"`
	Mergeable      *bool       `json:"mergeable,omitempty"`
	MergeableState string      `json:"mergeable_state,omitempty"`
	Head           GitHubPRRef `json:"head,omitempty"`
	Base           GitHubPRRef `json:"base,omitempty"`
}

// GitHubPRRef describes a PR head or base ref in GitHub webhook payloads.
type GitHubPRRef struct {
	Ref  string        `json:"ref,omitempty"`
	SHA  string        `json:"sha,omitempty"`
	Repo GitHubRefRepo `json:"repo,omitempty"`
}

// GitHubRefRepo is the repository subset nested under PR refs.
type GitHubRefRepo struct {
	FullName string `json:"full_name,omitempty"`
}

// GitHubCheckRun is the check_run subset needed by readiness events.
type GitHubCheckRun struct {
	ID           int64                    `json:"id,omitempty"`
	Name         string                   `json:"name,omitempty"`
	HeadBranch   string                   `json:"head_branch,omitempty"`
	HeadSHA      string                   `json:"head_sha,omitempty"`
	Status       string                   `json:"status,omitempty"`
	Conclusion   string                   `json:"conclusion,omitempty"`
	HTMLURL      string                   `json:"html_url,omitempty"`
	PullRequests []GitHubCheckPullRequest `json:"pull_requests,omitempty"`
}

// GitHubCheckSuite is the check_suite subset needed by readiness events.
type GitHubCheckSuite struct {
	ID           int64                    `json:"id,omitempty"`
	HeadBranch   string                   `json:"head_branch,omitempty"`
	HeadSHA      string                   `json:"head_sha,omitempty"`
	Status       string                   `json:"status,omitempty"`
	Conclusion   string                   `json:"conclusion,omitempty"`
	HTMLURL      string                   `json:"html_url,omitempty"`
	PullRequests []GitHubCheckPullRequest `json:"pull_requests,omitempty"`
}

// GitHubCheckPullRequest is the PR subset nested under check_run/check_suite.
type GitHubCheckPullRequest struct {
	Number  int         `json:"number,omitempty"`
	URL     string      `json:"url,omitempty"`
	HTMLURL string      `json:"html_url,omitempty"`
	Head    GitHubPRRef `json:"head,omitempty"`
	Base    GitHubPRRef `json:"base,omitempty"`
}

// GitHubMergeGroup is the merge_group subset needed by readiness events.
type GitHubMergeGroup struct {
	HeadSHA string `json:"head_sha,omitempty"`
	HeadRef string `json:"head_ref,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
	WebURL  string `json:"web_url,omitempty"`
	HTMLURL string `json:"html_url,omitempty"`
}

// GitHubWebhookResponse is returned after a supported webhook is accepted.
type GitHubWebhookResponse struct {
	Recorded  bool   `json:"recorded" doc:"Whether the webhook produced a Gas City event."`
	Status    string `json:"status" enum:"recorded,ignored" doc:"Ingestion status."`
	EventType string `json:"event_type,omitempty" doc:"Recorded Gas City event type."`
	Monitor   string `json:"monitor,omitempty" doc:"Configured monitor that accepted the webhook."`
	Reason    string `json:"reason,omitempty" doc:"Reason when the webhook was accepted but ignored."`
}

// GitHubWebhookOutput wraps GitHubWebhookResponse for Huma.
type GitHubWebhookOutput struct {
	Body GitHubWebhookResponse
}
