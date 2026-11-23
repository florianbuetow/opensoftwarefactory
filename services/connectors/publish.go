package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	awaitingApproval = "Recorded. A person must approve before this is published."

	// The kinds of a publish request.
	publishPush        = "push"
	publishPullRequest = "pull_request"

	// The states of a publish request.
	requestPending  = "pending"
	requestApproved = "approved"
	requestRejected = "rejected"
	requestDone     = "done"
)

// publishRequest is a push or a pull request that waits for a person's approval.
type publishRequest struct {
	Kind      string     `json:"kind"` // push | pull_request
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"` // pending | approved | rejected | done
	Result    string     `json:"result"`
	Run       string     `json:"run"`
	Agent     string     `json:"agent"`
	Created   time.Time  `json:"created"`
	Decided   *time.Time `json:"decided,omitempty"`
	Comment   string     `json:"comment,omitempty"`
	DecidedBy string     `json:"decided_by,omitempty"`
	N         int        `json:"n"`
}

type pullRequestComment struct {
	Body    string    `json:"body"`
	Created time.Time `json:"created"`
	Agent   string    `json:"agent"`
}

// pullRequest is the pull request of a dev pod as the pod's record keeps it.
type pullRequest struct {
	Kind           string               `json:"kind"`   // plain | github
	Object         string               `json:"object"` // blob of the newest record
	Run            string               `json:"run"`
	Title          string               `json:"title"`
	Body           string               `json:"body"`
	URL            string               `json:"url,omitempty"`
	Created        time.Time            `json:"created"`
	CreatedByAgent string               `json:"created_by_agent"`
	Comments       []pullRequestComment `json:"comments,omitempty"`
	Number         int                  `json:"number,omitempty"`
}

// pullRequestRecord is runs/<run id>/pull-request.json. The fields kind, dev_pod, url, updated,
// comments and number go beyond the contract.
type pullRequestRecord struct {
	Repository     string               `json:"repository"`
	SourceBranch   string               `json:"source_branch"`
	WorkBranch     string               `json:"work_branch"`
	Title          string               `json:"title"`
	Body           string               `json:"body"`
	Commits        []commit             `json:"commits"`
	Patch          string               `json:"patch,omitempty"`
	Created        time.Time            `json:"created"`
	CreatedByAgent string               `json:"created_by_agent"`
	Kind           string               `json:"kind"`
	DevPod         string               `json:"dev_pod"`
	URL            string               `json:"url,omitempty"`
	Updated        time.Time            `json:"updated"`
	Comments       []pullRequestComment `json:"comments,omitempty"`
	Pushed         bool                 `json:"pushed"`
	Number         int                  `json:"number,omitempty"`
}

// pullRequestProvider is what differs between Git hosts. The record in the blob store is
// kept for every kind by the service itself.
type pullRequestProvider interface {
	// open creates the pull request at the host and returns its number and address.
	open(ctx context.Context, p *devPod, repo *repository, title, body string) (number int, address string, err error)
	// edit changes title and body at the host.
	edit(ctx context.Context, repo *repository, pr *pullRequest) error
	// read returns what the host knows about the pull request, or nil.
	read(ctx context.Context, repo *repository, pr *pullRequest) (map[string]any, error)
	// comment adds a comment at the host.
	comment(ctx context.Context, repo *repository, pr *pullRequest, body string) error
}

func (s *service) provider(repo *repository) pullRequestProvider {
	if repo.Kind == kindGitHub {
		return githubProvider{s}
	}
	return plainProvider{}
}

// plainProvider is a Git host without pull requests: the record and its patch are all there is.
type plainProvider struct{}

func (plainProvider) open(context.Context, *devPod, *repository, string, string) (number int, address string, err error) {
	return 0, "", nil
}
func (plainProvider) edit(context.Context, *repository, *pullRequest) error { return nil }
func (plainProvider) read(context.Context, *repository, *pullRequest) (map[string]any, error) {
	return nil, nil
}

func (plainProvider) comment(context.Context, *repository, *pullRequest, string) error { return nil }

// githubProvider calls the GitHub REST API with the repository's token.
type githubProvider struct{ s *service }

func (g githubProvider) call(ctx context.Context, repo *repository, method, path string, in, out any) (err error) {
	owner, name, err := githubRepository(repo.URL)
	if err != nil {
		return err
	}
	secret, err := g.s.kube.Secret(ctx, credentialSecret(repo.Name))
	if err != nil || secret["token"] == "" {
		return fmt.Errorf("the repository %s has no token for GitHub", repo.Name)
	}
	var body io.Reader
	if in != nil {
		encoded, marshalErr := json.Marshal(in)
		if marshalErr != nil {
			return marshalErr
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.s.githubAPI+"/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secret["token"])
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.s.http.Do(req)
	if err != nil {
		return errors.New("GitHub is not reachable")
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var failure struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &failure)
		return fmt.Errorf("GitHub answered %s: %s", resp.Status, failure.Message)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type githubPull struct {
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Number  int    `json:"number"`
	Merged  bool   `json:"merged"`
}

func (g githubProvider) open(ctx context.Context, p *devPod, repo *repository, title, body string) (number int, address string, err error) {
	var pull githubPull
	err = g.call(ctx, repo, http.MethodPost, "/pulls", map[string]any{"title": title, "body": body, "head": p.WorkBranch, "base": p.SourceBranch}, &pull)
	return pull.Number, pull.HTMLURL, err
}

func (g githubProvider) edit(ctx context.Context, repo *repository, pr *pullRequest) error {
	return g.call(ctx, repo, http.MethodPatch, "/pulls/"+strconv.Itoa(pr.Number), map[string]any{"title": pr.Title, "body": pr.Body}, nil)
}

func (g githubProvider) read(ctx context.Context, repo *repository, pr *pullRequest) (map[string]any, error) {
	var pull githubPull
	if err := g.call(ctx, repo, http.MethodGet, "/pulls/"+strconv.Itoa(pr.Number), nil, &pull); err != nil {
		return nil, err
	}
	return map[string]any{"number": pull.Number, "url": pull.HTMLURL, "state": pull.State, "merged": pull.Merged, "title": pull.Title, "body": pull.Body}, nil
}

func (g githubProvider) comment(ctx context.Context, repo *repository, pr *pullRequest, body string) error {
	return g.call(ctx, repo, http.MethodPost, "/issues/"+strconv.Itoa(pr.Number)+"/comments", map[string]any{"body": body}, nil)
}

// gitTool handles the tools that publish the work.
func (s *service) gitTool(ctx context.Context, p *devPod, in toolInput) (any, error) {
	stored, err := s.loadRepository(ctx, p.Repository)
	if err != nil {
		return nil, fmt.Errorf("%s failed: the repository %q of this dev pod could not be read (%v). Try again; if it keeps failing, call report_blocked with the reason.", in.Tool, p.Repository, err)
	}
	repo := &stored
	const noPullRequest = "Refused: this dev pod has no pull request yet. Call create_pull_request with a title and a body first. If you did that already and were told a person must approve, the pull request is created after the approval; call create_pull_request again to change its title or body."
	switch in.Tool {
	case "push_branch":
		err = pushAllowed(repo)
		if err != nil {
			return nil, err
		}
		if repo.ApprovePublish {
			return s.requestPublish(ctx, p, in, publishPush, "", "")
		}
		return s.push(ctx, p, repo)
	case "create_pull_request":
		title, body := strings.TrimSpace(in.text("title")), in.text("body")
		if title == "" {
			return nil, errors.New("Refused: create_pull_request needs a title. Pass a short title that says what the change does, and the description as body.")
		}
		if p.PullRequest != nil {
			return nil, errors.New("Refused: this dev pod already has a pull request. Call update_pull_request to change its title or body, or get_pull_request to read it.")
		}
		if repo.Kind == kindGitHub {
			err = pushAllowed(repo)
			if err != nil {
				return nil, err
			}
		}
		if repo.ApprovePublish {
			return s.requestPublish(ctx, p, in, publishPullRequest, title, body)
		}
		return s.createPullRequest(ctx, p, repo, in.Run, in.Agent, title, body)
	case "update_pull_request":
		if p.PullRequest == nil {
			return nil, errors.New(noPullRequest)
		}
		if !in.has("title") && !in.has("body") {
			return nil, errors.New("Refused: update_pull_request needs a new title, a new body, or both.")
		}
		pr := *p.PullRequest
		if in.has("title") && strings.TrimSpace(in.text("title")) != "" {
			pr.Title = strings.TrimSpace(in.text("title"))
		}
		if in.has("body") {
			pr.Body = in.text("body")
		}
		if err = s.provider(repo).edit(ctx, repo, &pr); err != nil {
			return nil, fmt.Errorf("update_pull_request failed: %v", err)
		}
		return s.storePullRequest(ctx, p, repo, in.Run, &pr)
	case "comment_on_pull_request":
		if p.PullRequest == nil {
			return nil, errors.New(noPullRequest)
		}
		body := strings.TrimSpace(in.text("body"))
		if body == "" {
			return nil, errors.New("Refused: comment_on_pull_request needs a body: the text of the comment.")
		}
		pr := *p.PullRequest
		if err = s.provider(repo).comment(ctx, repo, &pr, body); err != nil {
			return nil, fmt.Errorf("comment_on_pull_request failed: %v", err)
		}
		pr.Comments = append(pr.Comments, pullRequestComment{Body: body, Agent: in.Agent, Created: s.now()})
		return s.storePullRequest(ctx, p, repo, in.Run, &pr)
	default: // get_pull_request
		if p.PullRequest == nil {
			return nil, errors.New(noPullRequest)
		}
		pr := *p.PullRequest
		answer := map[string]any{
			"pull_request": pr.Object, "title": pr.Title, "body": pr.Body, "source_branch": p.SourceBranch,
			"work_branch": p.WorkBranch, "pushed": p.Pushed, "created": pr.Created, "comments": orEmpty(pr.Comments),
		}
		remote, err := s.provider(repo).read(ctx, repo, &pr)
		if err != nil {
			return nil, fmt.Errorf("get_pull_request failed: %v", err)
		}
		for key, value := range remote {
			answer[key] = value
		}
		return answer, nil
	}
}

// pushAllowed refuses a push the repository's credential cannot make. A plain repository
// without a credential is tried, because a host may accept pushes from anyone.
func pushAllowed(repo *repository) error {
	if repo.Credential == credentialReadWrite || (repo.Credential == credentialNone && repo.Kind == kindPlain) {
		return nil
	}
	why := "has no credential"
	if repo.Credential == credentialRead {
		why = "has a credential that may only read"
	}
	instead := "Your commits stay in the dev pod. Call create_pull_request to hand them over as a patch instead."
	if repo.Kind == kindGitHub {
		instead = "Your commits stay in the dev pod. Call report_blocked and say that the repository needs a credential that may write."
	}
	return fmt.Errorf("Refused: the branch cannot be published, because the repository %q %s. %s", repo.Name, why, instead)
}

// push makes git-helper push the work branch.
func (s *service) push(ctx context.Context, p *devPod, repo *repository) (any, error) {
	out, err := s.helper(ctx, p.ID, "/push", map[string]any{"branch": p.WorkBranch})
	if err != nil {
		hint := ""
		if repo.Credential == credentialNone {
			hint = " The repository has no credential that may write; your commits stay in the dev pod, and create_pull_request can still hand them over as a patch."
		}
		return nil, fmt.Errorf("push_branch failed: the branch %q was not pushed to %s.%s Git said: %v", p.WorkBranch, repo.URL, hint, err)
	}
	pushed, err := s.change(ctx, p.ID, func(p *devPod) error { p.Pushed = true; return nil })
	if err != nil {
		return nil, err
	}
	if pushed.PullRequest != nil {
		// The record says whether the branch was pushed.
		if _, err = s.storePullRequest(ctx, &pushed, repo, pushed.PullRequest.Run, pushed.PullRequest); err != nil {
			s.log.Warn("updating the pull request record after a push", "pod", pushed.ID, "error", err)
		}
	}
	return map[string]any{"pushed": true, "branch": pushed.WorkBranch, "repository": repo.Name, "output": out.Output}, nil
}

func (s *service) createPullRequest(ctx context.Context, p *devPod, repo *repository, run, agent, title, body string) (any, error) {
	changes, err := s.helper(ctx, p.ID, "/patch", map[string]any{"base": p.base()})
	if err != nil {
		return nil, fmt.Errorf("create_pull_request failed: the commits of the work branch could not be read: %v", err)
	}
	if len(changes.Commits) == 0 {
		return nil, errors.New("Refused: the work branch has no commits yet, so there is nothing to put into a pull request. Commit your changes first (git add, then git commit), then call create_pull_request again.")
	}
	if repo.Kind == kindGitHub && !p.Pushed {
		return nil, errors.New("Refused: the work branch is not on GitHub yet. Call push_branch first, then create_pull_request again.")
	}
	number, address, err := s.provider(repo).open(ctx, p, repo, title, body)
	if err != nil {
		return nil, fmt.Errorf("create_pull_request failed: %v", err)
	}
	pr := pullRequest{Kind: repo.Kind, Run: run, Title: title, Body: body, Number: number, URL: address, Created: s.now(), CreatedByAgent: agent}
	return s.storePullRequest(ctx, p, repo, run, &pr)
}

// storePullRequest writes the record, and for a plain repository the patch, to the run's
// folder in the blob store and keeps the pull request in the pod's record. The pull request
// it is given stays as it is; the pod's record gets a copy.
func (s *service) storePullRequest(ctx context.Context, p *devPod, repo *repository, run string, given *pullRequest) (any, error) {
	if !segment.MatchString(run) {
		return nil, fmt.Errorf("the run ID %q cannot name a folder in the blob store", run)
	}
	changes, err := s.helper(ctx, p.ID, "/patch", map[string]any{"base": p.base()})
	if err != nil {
		return nil, fmt.Errorf("the commits of the work branch could not be read: %v", err)
	}
	pr := *given
	pr.Run, pr.Object = run, "runs/"+run+"/pull-request.json"
	record := pullRequestRecord{
		Repository: p.Repository, SourceBranch: p.SourceBranch, WorkBranch: p.WorkBranch, Title: pr.Title, Body: pr.Body,
		Commits: orEmpty(changes.Commits), Pushed: p.Pushed, Created: pr.Created, CreatedByAgent: pr.CreatedByAgent, Kind: pr.Kind, DevPod: p.ID,
		Number: pr.Number, URL: pr.URL, Updated: s.now(), Comments: pr.Comments,
	}
	if repo.Kind == kindPlain {
		// The patch is the one piece of repository content that may be stored: it is how the
		// work leaves a repository without pull requests.
		record.Patch = "runs/" + run + "/pull-request.patch"
		err = s.blobs.put(ctx, record.Patch, []byte(changes.Patch), "text/x-patch", pr.CreatedByAgent, run)
		if err != nil {
			return nil, err
		}
	}
	err = s.blobs.putJSON(ctx, pr.Object, record, pr.CreatedByAgent, run)
	if err != nil {
		return nil, err
	}
	if _, err = s.change(ctx, p.ID, func(p *devPod) error { p.PullRequest = &pr; return nil }); err != nil {
		return nil, err
	}
	answer := map[string]any{"pull_request": pr.Object, "title": pr.Title, "commits": len(record.Commits), "pushed": record.Pushed}
	if record.Patch != "" {
		answer["patch"] = record.Patch
	}
	if pr.URL != "" {
		answer["url"], answer["number"] = pr.URL, pr.Number
	}
	return answer, nil
}

// requestPublish records what the agent wants published. Asking again for the same thing
// changes the waiting request instead of adding one.
func (s *service) requestPublish(ctx context.Context, p *devPod, in toolInput, kind, title, body string) (any, error) {
	_, err := s.change(ctx, p.ID, func(p *devPod) error {
		for i := range p.PublishRequests {
			if req := &p.PublishRequests[i]; req.Kind == kind && req.Run == in.Run && req.State == requestPending {
				req.Title, req.Body, req.Agent = title, body, in.Agent
				return nil
			}
		}
		p.PublishRequests = append(p.PublishRequests, publishRequest{
			N: len(p.PublishRequests) + 1, Kind: kind, Title: title, Body: body,
			State: requestPending, Run: in.Run, Agent: in.Agent, Created: s.now(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("The request could not be recorded: %v. Call %s again.", err, in.Tool)
	}
	return awaitingApproval, nil
}

func (s *service) listPublishRequests(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pod(r.PathValue("id"))
	if !ok {
		s.fail(w, 404, errors.New("this dev pod does not exist"))
		return
	}
	run := r.URL.Query().Get("run")
	requests := []publishRequest{}
	for i := range p.PublishRequests {
		if req := &p.PublishRequests[i]; run == "" || req.Run == run {
			requests = append(requests, *req)
		}
	}
	writeJSON(w, 200, requests)
}

// decisions run one after the other, so that an approval is never acted on twice.
var decisions sync.Mutex

// decidePublishRequest records a person's decision and, on approval, publishes. A request
// whose publishing failed stays approved with the failure as its result and can be decided
// again.
func (s *service) decidePublishRequest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Decision  string `json:"decision"`
		Comment   string `json:"comment"`
		DecidedBy string `json:"decided_by"`
	}
	if err := readJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	var approve bool
	switch strings.ToLower(strings.TrimSpace(in.Decision)) {
	case "approve", "approved":
		approve = true
	case "reject", "rejected":
	default:
		s.fail(w, 400, errors.New(`decision must be "approve" or "reject"`))
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	id := r.PathValue("id")
	decisions.Lock()
	defer decisions.Unlock()
	var req publishRequest
	set := func(update func(*publishRequest)) (devPod, error) {
		return s.change(r.Context(), id, func(p *devPod) error {
			for i := range p.PublishRequests {
				if p.PublishRequests[i].N == n {
					update(&p.PublishRequests[i])
					req = p.PublishRequests[i]
					return nil
				}
			}
			return statusError(404, "dev pod %s has no publish request %d", id, n)
		})
	}
	if _, err := set(func(*publishRequest) {}); err != nil {
		s.fail(w, 502, err)
		return
	}
	if req.State != requestPending && req.State != requestApproved {
		s.fail(w, 409, fmt.Errorf("publish request %d is already %s", n, req.State))
		return
	}
	now := s.now()
	if !approve {
		if _, err := set(func(q *publishRequest) {
			q.State, q.Comment, q.DecidedBy, q.Decided, q.Result = requestRejected, in.Comment, in.DecidedBy, &now, "Rejected."
		}); err != nil {
			s.fail(w, 502, err)
			return
		}
		writeJSON(w, 200, req)
		return
	}
	p, err := set(func(q *publishRequest) {
		q.State, q.Comment, q.DecidedBy, q.Decided = requestApproved, in.Comment, in.DecidedBy, &now
	})
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	outcome, err := s.publish(r.Context(), &p, &req)
	if err != nil {
		_, _ = set(func(q *publishRequest) { q.Result = "Failed: " + err.Error() })
		writeJSON(w, 502, map[string]any{"error": err.Error(), "request": req})
		return
	}
	if _, err = set(func(q *publishRequest) { q.State, q.Result = requestDone, outcome }); err != nil {
		s.fail(w, 502, err)
		return
	}
	writeJSON(w, 200, req)
}

// publish carries out an approved request. An approved pull request on GitHub includes the
// push it needs.
func (s *service) publish(ctx context.Context, p *devPod, req *publishRequest) (string, error) {
	stored, err := s.loadRepository(ctx, p.Repository)
	if err != nil {
		return "", fmt.Errorf("the repository %q could not be read: %w", p.Repository, err)
	}
	repo := &stored
	current := p
	if req.Kind == publishPush || (repo.Kind == kindGitHub && !p.Pushed) {
		if _, err = s.push(ctx, p, repo); err != nil {
			return "", err
		}
		if req.Kind == publishPush {
			return fmt.Sprintf("Pushed the branch %s to %s.", p.WorkBranch, repo.URL), nil
		}
		pushed, _ := s.pod(p.ID)
		current = &pushed
	}
	if current.PullRequest != nil {
		return "", errors.New("this dev pod already has a pull request")
	}
	if _, err = s.createPullRequest(ctx, current, repo, req.Run, req.Agent, req.Title, req.Body); err != nil {
		return "", err
	}
	created, _ := s.pod(p.ID)
	if created.PullRequest.URL != "" {
		return "Pull request created: " + created.PullRequest.URL, nil
	}
	return "Pull request recorded: " + created.PullRequest.Object, nil
}
