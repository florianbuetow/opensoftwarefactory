package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	podPrefix   = "connectors/devpods"
	podSelector = "app=devpod"
	stateNew    = "creating"
	stateReady  = "ready"
	authorEmail = "agent@opensoftwarefactory.local"
	workPort    = 8080
	helperPort  = 8081
)

// rule blocks a tool call of an agent with a message.
type rule struct {
	Tool    string `json:"tool"`
	Path    string `json:"path,omitempty"`
	Matches string `json:"matches,omitempty"`
	Message string `json:"message"`
}

// blockedReport is what an agent said when it ended its work with report_blocked.
type blockedReport struct {
	Run    string    `json:"run"`
	Agent  string    `json:"agent"`
	Reason string    `json:"reason"`
	Time   time.Time `json:"time"`
}

// devPod is everything the connector service knows about a dev pod. It is stored in the blob
// store after every change, so a restart of the service loses nothing. The tokens of the
// pod's two servers are not part of it; they live in a Kubernetes Secret.
type devPod struct {
	ID              string               `json:"id"`
	Repository      string               `json:"repository"`
	Name            string               `json:"name"`
	SourceBranch    string               `json:"source_branch"`
	WorkBranch      string               `json:"work_branch"`
	StartCommit     string               `json:"start_commit"`
	State           string               `json:"state"`
	Created         time.Time            `json:"created"`
	Protected       []string             `json:"protected"`
	Rules           []rule               `json:"rules"`
	Users           []string             `json:"users"` // runs that may call tools on the pod
	Holds           map[string]time.Time `json:"holds"` // runs that keep the pod, with their last renewal
	LastActivity    time.Time            `json:"last_activity"`
	IdleSeconds     int                  `json:"idle_seconds"`
	Author          string               `json:"author"` // agent set as commit author in the pod
	Pushed          bool                 `json:"pushed"`
	Blocked         []blockedReport      `json:"blocked"`
	PublishRequests []publishRequest     `json:"publish_requests"`
	PullRequest     *pullRequest         `json:"pull_request,omitempty"`
}

// podConn is how the pod's two servers are reached.
type podConn struct {
	ip, workToken, helperToken string
}

// podView is a pod as the API lists it.
type podView struct {
	ID           string    `json:"id"`
	Repository   string    `json:"repository"`
	Name         string    `json:"name"`
	SourceBranch string    `json:"source_branch"`
	WorkBranch   string    `json:"work_branch"`
	Runs         []string  `json:"runs"`
	LastActivity time.Time `json:"last_activity"`
	Expires      time.Time `json:"expires"`
	Phase        string    `json:"phase"`
}

// podDetail is one pod with everything stored about it.
type podDetail struct {
	podView
	StartCommit     string           `json:"start_commit"`
	State           string           `json:"state"`
	Created         time.Time        `json:"created"`
	Protected       []string         `json:"protected"`
	Rules           []rule           `json:"rules"`
	Users           []string         `json:"users"`
	Pushed          bool             `json:"pushed"`
	Blocked         []blockedReport  `json:"blocked"`
	PublishRequests []publishRequest `json:"publish_requests"`
	PullRequest     *pullRequest     `json:"pull_request,omitempty"`
}

var (
	segment    = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,199}$`)
	branchName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	podID      = regexp.MustCompile(`^[a-f0-9]{10}$`)
)

func podObject(id string) string { return podPrefix + "/" + id + ".json" }
func podName(id string) string   { return "devpod-" + id }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// load reads every pod record from the blob store.
func (s *service) load(ctx context.Context) error {
	ids, err := s.blobs.list(ctx, podPrefix)
	if err != nil {
		return err
	}
	pods := map[string]*devPod{}
	for _, id := range ids {
		var p devPod
		if err = s.blobs.getJSON(ctx, id, &p); err != nil {
			if errors.Is(err, errNotFound) {
				continue
			}
			return err
		}
		if podID.MatchString(p.ID) {
			pods[p.ID] = &p
		}
	}
	s.mu.Lock()
	s.pods, s.loaded = pods, true
	s.mu.Unlock()
	return nil
}

func (p *devPod) copy() devPod {
	var c devPod
	data, _ := json.Marshal(p)
	_ = json.Unmarshal(data, &c)
	return c
}

// pod returns a copy of a pod record.
func (s *service) pod(id string) (devPod, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pods[id]
	if !ok {
		return devPod{}, false
	}
	return p.copy(), true
}

// change applies fn to a pod record and stores the result in the blob store.
func (s *service) change(ctx context.Context, id string, fn func(*devPod) error) (devPod, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pods[id]
	if !ok {
		return devPod{}, statusError(404, "dev pod %s does not exist", id)
	}
	if err := fn(p); err != nil {
		return devPod{}, err
	}
	// A change must survive even when the request that caused it was given up.
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.blobs.putJSON(save, podObject(id), p, "connectors", ""); err != nil {
		return devPod{}, err
	}
	return p.copy(), nil
}

// heldBy returns the runs whose hold on the pod is still valid.
func (s *service) heldBy(p devPod) []string {
	runs := []string{}
	for run, renewed := range p.Holds {
		if s.cfg.holdTTL <= 0 || s.now().Sub(renewed) <= s.cfg.holdTTL {
			runs = append(runs, run)
		}
	}
	sort.Strings(runs)
	return runs
}

func (s *service) idle(p devPod) time.Duration {
	if s.cfg.idleOverride > 0 {
		return s.cfg.idleOverride
	}
	return time.Duration(p.IdleSeconds) * time.Second
}

func (s *service) view(p devPod, phase string) podView {
	return podView{
		ID: p.ID, Repository: p.Repository, Name: p.Name, SourceBranch: p.SourceBranch, WorkBranch: p.WorkBranch,
		Runs: s.heldBy(p), LastActivity: p.LastActivity, Expires: p.LastActivity.Add(s.idle(p)), Phase: phase,
	}
}

func phaseOf(p devPod, info podInfo, found bool) string {
	switch {
	case !found && p.State == stateNew:
		return "Creating"
	case !found:
		return "Missing"
	case info.Deleting:
		return "Terminating"
	case p.State == stateNew:
		return "Creating"
	case info.Phase == "":
		return "Unknown"
	}
	return info.Phase
}

func (s *service) listPods(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	pods := make([]devPod, 0, len(s.pods))
	for _, p := range s.pods {
		pods = append(pods, p.copy())
	}
	s.mu.Unlock()
	sort.Slice(pods, func(i, j int) bool { return pods[i].Created.Before(pods[j].Created) })
	infos, err := s.kube.Pods(r.Context(), podSelector)
	byName := map[string]podInfo{}
	for _, info := range infos {
		byName[info.Name] = info
	}
	views := []podView{}
	for _, p := range pods {
		info, found := byName[podName(p.ID)]
		phase := "Unknown"
		if err == nil {
			phase = phaseOf(p, info, found)
		}
		views = append(views, s.view(p, phase))
	}
	writeJSON(w, 200, views)
}

func (s *service) detail(ctx context.Context, p devPod) podDetail {
	phase := "Unknown"
	info, err := s.kube.Pod(ctx, podName(p.ID))
	if err == nil || errors.Is(err, errNotFound) {
		phase = phaseOf(p, info, err == nil)
	}
	return podDetail{
		podView: s.view(p, phase), StartCommit: p.StartCommit, State: p.State, Created: p.Created, Protected: orEmpty(p.Protected),
		Rules: orEmpty(p.Rules), Users: orEmpty(p.Users), Pushed: p.Pushed, Blocked: orEmpty(p.Blocked),
		PublishRequests: orEmpty(p.PublishRequests), PullRequest: p.PullRequest,
	}
}

func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func (s *service) getPod(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pod(r.PathValue("id"))
	if !ok {
		s.fail(w, 404, errors.New("this dev pod does not exist"))
		return
	}
	writeJSON(w, 200, s.detail(r.Context(), p))
}

type createInput struct {
	Repository   string   `json:"repository"`
	SourceBranch string   `json:"source_branch"`
	WorkBranch   string   `json:"work_branch"`
	Name         string   `json:"name"`
	Run          string   `json:"run"`
	Protected    []string `json:"protected"`
	Rules        []rule   `json:"rules"`
}

// createResult is the answer to a creation. Beyond the contract's id and reused it carries
// what a devpod step writes into its output document.
type createResult struct {
	ID           string `json:"id"`
	Reused       bool   `json:"reused"`
	Repository   string `json:"repository"`
	SourceBranch string `json:"source_branch"`
	WorkBranch   string `json:"work_branch"`
	StartCommit  string `json:"start_commit"`
}

func result(p devPod, reused bool) createResult {
	return createResult{p.ID, reused, p.Repository, p.SourceBranch, p.WorkBranch, p.StartCommit}
}

func (in *createInput) check() error {
	in.Repository, in.SourceBranch, in.WorkBranch = strings.TrimSpace(in.Repository), strings.TrimSpace(in.SourceBranch), strings.TrimSpace(in.WorkBranch)
	in.Name, in.Run = strings.TrimSpace(in.Name), strings.TrimSpace(in.Run)
	if in.Repository == "" {
		return statusError(400, "repository is required")
	}
	if !segment.MatchString(in.Run) {
		return statusError(400, "run is required: the ID of the run that uses the dev pod")
	}
	if !branchName.MatchString(in.WorkBranch) {
		return statusError(400, "work_branch is required and may hold letters, digits, '.', '_', '-' and '/'")
	}
	if in.SourceBranch != "" && !branchName.MatchString(in.SourceBranch) {
		return statusError(400, "source_branch may hold letters, digits, '.', '_', '-' and '/'")
	}
	for _, pattern := range in.Protected {
		if strings.TrimSpace(pattern) == "" {
			return statusError(400, "protected must not hold an empty pattern")
		}
	}
	for i, rl := range in.Rules {
		if err := rl.check(); err != nil {
			return statusError(400, "rule %d: %v", i+1, err)
		}
	}
	return nil
}

func (s *service) createPod(w http.ResponseWriter, r *http.Request) {
	var in createInput
	if err := readJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	if err := in.check(); err != nil {
		s.fail(w, 400, err)
		return
	}
	repo, err := s.loadRepository(r.Context(), in.Repository)
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, fmt.Errorf("repository %q does not exist; add it on the Connectors page", in.Repository))
		return
	}
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	if in.SourceBranch == "" {
		in.SourceBranch = repo.DefaultBranch
	}
	if in.WorkBranch == in.SourceBranch || in.WorkBranch == repo.DefaultBranch {
		s.fail(w, 409, fmt.Errorf("the work branch %q is the source branch or the repository's default branch; a dev pod always works on a branch of its own, so choose another work branch", in.WorkBranch))
		return
	}
	res, err := s.create(r.Context(), repo, in)
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	writeJSON(w, 200, res)
}

// create returns the live pod of the same repository and name, or builds a new one.
func (s *service) create(ctx context.Context, repo repository, in createInput) (createResult, error) {
	deadline := s.now().Add(s.cfg.startTimeout + 15*time.Minute)
	for {
		s.createMu.Lock()
		existing, found := s.named(repo.Name, in.Name)
		if found && existing.State == stateNew {
			// Another request is building this pod: wait for it instead of building a second one.
			s.createMu.Unlock()
			if s.now().After(deadline) {
				return createResult{}, fmt.Errorf("dev pod %q of %s is still being created", in.Name, repo.Name)
			}
			select {
			case <-ctx.Done():
				return createResult{}, ctx.Err()
			case <-time.After(s.cfg.poll):
			}
			continue
		}
		if found {
			if info, err := s.kube.Pod(ctx, podName(existing.ID)); err == nil && !info.Deleting && info.Phase == "Running" {
				s.createMu.Unlock()
				return s.reuse(ctx, existing, repo, in)
			} else if err != nil && !errors.Is(err, errNotFound) {
				s.createMu.Unlock()
				return createResult{}, err
			}
			// The pod behind the record is gone or broken: clear it away and build a new one.
			if err := s.destroy(ctx, existing.ID); err != nil {
				s.createMu.Unlock()
				return createResult{}, err
			}
		}
		now := s.now()
		p := &devPod{
			ID: randomHex(5), Repository: repo.Name, Name: in.Name, SourceBranch: in.SourceBranch, WorkBranch: in.WorkBranch,
			State: stateNew, Created: now, Protected: orEmpty(in.Protected), Rules: orEmpty(in.Rules), Users: []string{in.Run},
			Holds: map[string]time.Time{in.Run: now}, LastActivity: now, IdleSeconds: repo.IdleMinutes * 60,
			Blocked: []blockedReport{}, PublishRequests: []publishRequest{},
		}
		s.mu.Lock()
		s.pods[p.ID] = p
		s.mu.Unlock()
		_, err := s.change(ctx, p.ID, func(*devPod) error { return nil })
		s.createMu.Unlock()
		if err == nil {
			err = s.build(ctx, p.ID, repo)
		}
		if err != nil {
			clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if derr := s.destroy(clean, p.ID); derr != nil {
				s.log.Warn("clearing away a dev pod that failed to start", "pod", p.ID, "error", derr)
			}
			return createResult{}, fmt.Errorf("the dev pod could not be created: %w", err)
		}
		ready, _ := s.pod(p.ID)
		return result(ready, false), nil
	}
}

// named finds the pod of a repository with a name. Pods without a name are never reused.
func (s *service) named(repo, name string) (devPod, bool) {
	if name == "" {
		return devPod{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pods {
		if p.Repository == repo && p.Name == name {
			return p.copy(), true
		}
	}
	return devPod{}, false
}

// reuse lets another run continue in a live pod. Rules given with the request replace the
// pod's rules; protected paths are added, because a protection cannot be taken back.
func (s *service) reuse(ctx context.Context, existing devPod, repo repository, in createInput) (createResult, error) {
	added := []string{}
	for _, pattern := range in.Protected {
		if !contains(existing.Protected, pattern) {
			added = append(added, pattern)
		}
	}
	if len(added) > 0 {
		if _, err := s.helper(ctx, existing.ID, "/protect", map[string]any{"paths": append(existing.Protected, added...)}); err != nil {
			return createResult{}, fmt.Errorf("protecting paths in the reused dev pod: %w", err)
		}
	}
	p, err := s.change(ctx, existing.ID, func(p *devPod) error {
		p.Protected = append(p.Protected, added...)
		if in.Rules != nil {
			p.Rules = in.Rules
		}
		if !contains(p.Users, in.Run) {
			p.Users = append(p.Users, in.Run)
		}
		p.Holds[in.Run] = s.now()
		p.IdleSeconds = repo.IdleMinutes * 60
		return nil
	})
	if err != nil {
		return createResult{}, err
	}
	return result(p, true), nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// build starts the pod and prepares its workspace: clone, setup command, protected paths,
// and only then the network policy, because the setup command may use the internet.
func (s *service) build(ctx context.Context, id string, repo repository) error {
	p, _ := s.pod(id)
	tokens := map[string]string{"work-token": randomHex(24), "helper-token": randomHex(24)}
	labels := map[string]string{"app": "devpod-token", "devpod-id": id}
	if err := s.kube.ApplySecret(ctx, podName(id), labels, tokens); err != nil {
		return err
	}
	if err := s.kube.CreatePod(ctx, s.podSpec(p, repo)); err != nil {
		return err
	}
	if err := s.waitReady(ctx, id); err != nil {
		return err
	}
	cloned, err := s.helper(ctx, id, "/clone", map[string]any{
		"url": repo.URL, "source_branch": p.SourceBranch, "work_branch": p.WorkBranch,
		"author_name": "Factory agent", "author_email": authorEmail,
	})
	if err != nil {
		return fmt.Errorf("cloning %s: %w", repo.URL, err)
	}
	if strings.TrimSpace(repo.SetupCommand) != "" {
		out, err := s.runCommand(ctx, id, repo.SetupCommand, 600)
		if err != nil {
			return fmt.Errorf("running the setup command: %w", err)
		}
		if out.ExitCode != 0 {
			return fmt.Errorf("the setup command %q ended with exit code %d: %s", repo.SetupCommand, out.ExitCode, tail(out.Stdout+"\n"+out.Stderr, 2000))
		}
	}
	if len(p.Protected) > 0 {
		if _, err = s.helper(ctx, id, "/protect", map[string]any{"paths": p.Protected}); err != nil {
			return fmt.Errorf("protecting paths: %w", err)
		}
	}
	if err = s.kube.ApplyNetworkPolicy(ctx, podName(id), s.networkPolicy(id, repo)); err != nil {
		return err
	}
	_, err = s.change(ctx, id, func(p *devPod) error {
		p.State, p.StartCommit, p.LastActivity = stateReady, cloned.StartCommit, s.now()
		return nil
	})
	return err
}

func tail(text string, n int) string {
	text = strings.TrimSpace(text)
	if len(text) > n {
		return "..." + text[len(text)-n:]
	}
	return text
}

// waitReady waits until both servers of the pod answer /healthz.
func (s *service) waitReady(ctx context.Context, id string) error {
	deadline := time.Now().Add(s.cfg.startTimeout)
	last := "the pod has no address yet"
	for {
		info, err := s.kube.Pod(ctx, podName(id))
		switch {
		case err != nil:
			last = err.Error()
		case info.Phase == "Failed" || info.Phase == "Succeeded":
			return fmt.Errorf("the pod ended while starting (phase %s)", info.Phase)
		case info.IP == "":
			last = "the pod has no address yet (phase " + info.Phase + ")"
		default:
			s.forget(id)
			if err = s.healthy(ctx, id); err == nil {
				return nil
			}
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the pod did not answer within %s: %s", s.cfg.startTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cfg.poll):
		}
	}
}

func (s *service) healthy(ctx context.Context, id string) error {
	for _, port := range []int{workPort, helperPort} {
		check, cancel := context.WithTimeout(ctx, 3*time.Second)
		status, _, err := s.callPod(check, id, port, http.MethodGet, "/healthz", nil)
		cancel()
		if err != nil {
			return err
		}
		if status != 200 {
			return fmt.Errorf("port %d answered /healthz with status %d", port, status)
		}
	}
	return nil
}

// destroy deletes the pod and everything that belongs to it. The record stays when the pod
// could not be deleted, so the next sweep tries again.
func (s *service) destroy(ctx context.Context, id string) error {
	if err := s.kube.DeletePod(ctx, podName(id)); err != nil {
		return err
	}
	if err := s.kube.DeleteNetworkPolicy(ctx, podName(id)); err != nil {
		s.log.Warn("deleting a dev pod's network policy", "pod", id, "error", err)
	}
	if err := s.kube.DeleteSecret(ctx, podName(id)); err != nil {
		s.log.Warn("deleting a dev pod's token secret", "pod", id, "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.blobs.delete(ctx, podObject(id)); err != nil {
		return err
	}
	delete(s.pods, id)
	delete(s.conns, id)
	return nil
}

func (s *service) deletePod(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.pod(id); !ok {
		s.fail(w, 404, errors.New("this dev pod does not exist"))
		return
	}
	if err := s.destroy(r.Context(), id); err != nil {
		s.fail(w, 502, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type runInput struct {
	Run string `json:"run"`
}

func (s *service) holdPod(w http.ResponseWriter, r *http.Request) {
	var in runInput
	if err := readJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	if !segment.MatchString(in.Run) {
		s.fail(w, 400, errors.New("run is required"))
		return
	}
	p, err := s.change(r.Context(), r.PathValue("id"), func(p *devPod) error {
		if p.Holds == nil {
			p.Holds = map[string]time.Time{}
		}
		p.Holds[in.Run] = s.now()
		if !contains(p.Users, in.Run) {
			p.Users = append(p.Users, in.Run)
		}
		return nil
	})
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	writeJSON(w, 200, s.view(p, phaseOf(p, podInfo{Phase: "Running"}, true)))
}

// releasePod ends a run's hold. Releasing a pod that is gone already is not an error.
func (s *service) releasePod(w http.ResponseWriter, r *http.Request) {
	var in runInput
	if err := readJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	if _, ok := s.pod(r.PathValue("id")); !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p, err := s.change(r.Context(), r.PathValue("id"), func(p *devPod) error {
		delete(p.Holds, in.Run)
		return nil
	})
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	writeJSON(w, 200, s.view(p, phaseOf(p, podInfo{Phase: "Running"}, true)))
}

// sweepLoop looks for pods to delete at the configured interval.
func (s *service) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.sweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// sweep deletes the pods no run holds and no agent used for their idle time, clears away
// pods whose creation was cut off, and brings records and cluster back in line.
func (s *service) sweep(ctx context.Context) {
	s.mu.Lock()
	if !s.loaded {
		s.mu.Unlock()
		return
	}
	pods := make([]devPod, 0, len(s.pods))
	for _, p := range s.pods {
		pods = append(pods, p.copy())
	}
	s.mu.Unlock()
	now := s.now()
	infos, listErr := s.kube.Pods(ctx, podSelector)
	inCluster := map[string]bool{}
	for _, info := range infos {
		inCluster[info.Name] = true
	}
	known := map[string]bool{}
	for _, p := range pods {
		known[podName(p.ID)] = true
		reason := ""
		switch {
		case p.State == stateNew && now.Sub(p.Created) > s.cfg.startTimeout+30*time.Minute:
			reason = "its creation was cut off"
		case p.State == stateReady && listErr == nil && !inCluster[podName(p.ID)]:
			reason = "its pod no longer exists"
		case p.State == stateReady && len(s.heldBy(p)) == 0 && now.Sub(p.LastActivity) > s.idle(p):
			reason = "no run holds it and no agent used it for " + s.idle(p).String()
		}
		if reason == "" {
			continue
		}
		if err := s.destroy(ctx, p.ID); err != nil {
			s.log.Warn("deleting a dev pod failed", "pod", p.ID, "error", err)
			continue
		}
		s.log.Info("dev pod deleted", "pod", p.ID, "repository", p.Repository, "reason", reason)
	}
	// A pod without a record cannot be used by anyone.
	for _, info := range infos {
		if !known[info.Name] && !info.Deleting && !s.recorded(info.Name) {
			if err := s.kube.DeletePod(ctx, info.Name); err != nil {
				s.log.Warn("deleting a dev pod without a record failed", "pod", info.Name, "error", err)
			}
		}
	}
}

// recorded reports whether a pod of the cluster has a record by now; a creation may have
// added one after the sweep took its copy.
func (s *service) recorded(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pods[strings.TrimPrefix(name, "devpod-")]
	return ok
}
