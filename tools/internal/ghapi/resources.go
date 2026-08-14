package ghapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// pageSize is the largest page GitHub serves for the listings this package
// reads, so a full listing costs the fewest requests.
const pageSize = 100

// maxPages bounds a listing. The engine publishes to a handful of repositories
// and tracks one issue per repository, so a listing that runs past this bound
// is a server that never stops paginating rather than a real result set.
const maxPages = 50

// Account is a GitHub user or organization.
type Account struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

// Repository is the subset of a GitHub repository the engine reads.
type Repository struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	FullName      string  `json:"full_name"`
	Owner         Account `json:"owner"`
	Private       bool    `json:"private"`
	Fork          bool    `json:"fork"`
	Archived      bool    `json:"archived"`
	Disabled      bool    `json:"disabled"`
	DefaultBranch string  `json:"default_branch"`
}

// Workflow states GitHub reports for an Actions workflow.
const (
	WorkflowActive             = "active"
	WorkflowDisabledManually   = "disabled_manually"
	WorkflowDisabledInactivity = "disabled_inactivity"
	WorkflowDisabledFork       = "disabled_fork"
)

// Workflow is an Actions workflow as GitHub reports it.
//
// The scheduled publishing workflow is disabled by GitHub after a period of
// repository inactivity, and a disabled workflow fails silently by simply never
// running, so each run checks that it is still active.
type Workflow struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

// Enabled reports whether the workflow still runs.
func (w Workflow) Enabled() bool { return w.State == WorkflowActive }

// Label is an issue label.
type Label struct {
	Name string `json:"name"`
}

// Issue is the subset of a GitHub issue the engine reads and writes. The engine
// keeps at most one tracking issue per repository.
type Issue struct {
	Number    int64     `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Labels    []Label   `json:"labels"`
	User      Account   `json:"user"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// PullRequest is present when this issue is really a pull request. The
	// issues endpoint returns both kinds, and a pull request is never a
	// tracking issue, so listings drop the entries that carry it.
	PullRequest *IssueLink `json:"pull_request"`
}

// IssueLink is the pull request reference attached to an issue.
type IssueLink struct {
	HTMLURL string `json:"html_url"`
}

// IssueComment is a comment on an issue.
type IssueComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      Account   `json:"user"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
}

// IssueRequest creates an issue.
type IssueRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// IssueUpdate edits an existing issue. An empty field is left as it is.
type IssueUpdate struct {
	Title  string   `json:"title,omitempty"`
	Body   string   `json:"body,omitempty"`
	State  string   `json:"state,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// commentRequest posts a comment body.
type commentRequest struct {
	Body string `json:"body"`
}

// Repository reads one repository's metadata, including its default branch.
func (c *Client) Repository(ctx context.Context, owner, name string) (Repository, error) {
	if err := validateRepository(owner, name); err != nil {
		return Repository{}, fmt.Errorf("github repository: %w", err)
	}
	return request[Repository](ctx, c, http.MethodGet, []string{"repos", owner, name}, nil, nil)
}

// DefaultBranch reads one repository's default branch.
func (c *Client) DefaultBranch(ctx context.Context, owner, name string) (string, error) {
	repository, err := c.Repository(ctx, owner, name)
	if err != nil {
		return "", err
	}
	if repository.DefaultBranch == "" {
		return "", fmt.Errorf("github repository %s/%s: reported no default branch", owner, name)
	}
	return repository.DefaultBranch, nil
}

// Workflow reads one Actions workflow by its file name, such as sync.yml.
func (c *Client) Workflow(ctx context.Context, owner, repo, file string) (Workflow, error) {
	if err := validateRepository(owner, repo); err != nil {
		return Workflow{}, fmt.Errorf("github workflow: %w", err)
	}
	if err := validateName("workflow file", file); err != nil {
		return Workflow{}, fmt.Errorf("github workflow: %w", err)
	}
	segments := []string{"repos", owner, repo, "actions", "workflows", file}
	return request[Workflow](ctx, c, http.MethodGet, segments, nil, nil)
}

// ListOpenIssues lists the open issues carrying every one of labels, with pull
// requests dropped.
func (c *Client) ListOpenIssues(ctx context.Context, owner, repo string, labels []string) ([]Issue, error) {
	if err := validateRepository(owner, repo); err != nil {
		return nil, fmt.Errorf("github issues: %w", err)
	}
	for _, label := range labels {
		if err := validateLabel(label); err != nil {
			return nil, fmt.Errorf("github issues: %w", err)
		}
	}
	var all []Issue
	for page := 1; page <= maxPages; page++ {
		query := url.Values{
			"state":    {"open"},
			"per_page": {strconv.Itoa(pageSize)},
			"page":     {strconv.Itoa(page)},
		}
		if len(labels) > 0 {
			query.Set("labels", strings.Join(labels, ","))
		}
		got, err := request[[]Issue](ctx, c, http.MethodGet, []string{"repos", owner, repo, "issues"}, query, nil)
		if err != nil {
			return nil, err
		}
		for _, issue := range got {
			if issue.PullRequest == nil {
				all = append(all, issue)
			}
		}
		if len(got) < pageSize {
			return all, nil
		}
	}
	return nil, fmt.Errorf("github issues: listing did not end within %d pages", maxPages)
}

// CreateIssue opens an issue.
func (c *Client) CreateIssue(ctx context.Context, owner, repo string, issue IssueRequest) (Issue, error) {
	if err := validateRepository(owner, repo); err != nil {
		return Issue{}, fmt.Errorf("github create issue: %w", err)
	}
	if strings.TrimSpace(issue.Title) == "" {
		return Issue{}, fmt.Errorf("github create issue: an issue needs a title")
	}
	return request[Issue](ctx, c, http.MethodPost, []string{"repos", owner, repo, "issues"}, nil, issue)
}

// UpdateIssue edits an existing issue.
func (c *Client) UpdateIssue(ctx context.Context, owner, repo string, number int64, update IssueUpdate) (Issue, error) {
	if err := validateRepository(owner, repo); err != nil {
		return Issue{}, fmt.Errorf("github update issue: %w", err)
	}
	if number <= 0 {
		return Issue{}, fmt.Errorf("github update issue: issue number %d must be positive", number)
	}
	if update.Title == "" && update.Body == "" && update.State == "" && len(update.Labels) == 0 {
		return Issue{}, fmt.Errorf("github update issue: an update needs at least one field")
	}
	segments := []string{"repos", owner, repo, "issues", strconv.FormatInt(number, 10)}
	return request[Issue](ctx, c, http.MethodPatch, segments, nil, update)
}

// CreateIssueComment adds a comment to an issue.
func (c *Client) CreateIssueComment(ctx context.Context, owner, repo string, number int64, body string) (IssueComment, error) {
	if err := validateRepository(owner, repo); err != nil {
		return IssueComment{}, fmt.Errorf("github issue comment: %w", err)
	}
	if number <= 0 {
		return IssueComment{}, fmt.Errorf("github issue comment: issue number %d must be positive", number)
	}
	if body == "" {
		return IssueComment{}, fmt.Errorf("github issue comment: a comment needs a body")
	}
	segments := []string{"repos", owner, repo, "issues", strconv.FormatInt(number, 10), "comments"}
	return request[IssueComment](ctx, c, http.MethodPost, segments, nil, commentRequest{Body: body})
}

// validateLabel holds a label to what the issues query can actually express.
//
// GitHub's labels parameter is one comma separated list, so a label containing
// a comma cannot be asked for: it would arrive as two labels and the listing
// would answer a different question than the caller asked, quietly and with a
// plausible looking result. Splitting or escaping is not available, so the
// input is refused instead. Surrounding whitespace is refused for the same
// reason, because the list separator swallows it and two labels that differ
// only by a space would become one.
func validateLabel(label string) error {
	switch {
	case label == "":
		return errors.New("a label must not be empty")
	case strings.Contains(label, ","):
		return fmt.Errorf("label %q must not contain a comma, the issues query separates labels with one", label)
	case strings.TrimSpace(label) != label:
		return fmt.Errorf("label %q must not be surrounded by whitespace", label)
	case strings.ContainsFunc(label, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return errors.New("a label must not contain control characters")
	}
	return nil
}

// validateRepository checks the two caller supplied segments that name a
// repository.
func validateRepository(owner, name string) error {
	if err := validateName("owner", owner); err != nil {
		return err
	}
	return validateName("repository", name)
}

// validateName holds a path parameter to the characters GitHub itself allows in
// a name. Escaping already keeps a stray value from changing a URL's structure,
// so this exists to turn a caller's mistake into a message that names the field
// rather than a 404 from the far end.
func validateName(field, value string) error {
	if value == "" {
		return fmt.Errorf("a %s name is required", field)
	}
	if value == "." || value == ".." {
		return fmt.Errorf("%s name %q must not traverse", field, value)
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%s name %q may only contain letters, digits, and -._", field, value)
		}
	}
	return nil
}
