package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const repositoryPrefix = "connectors/git"

// repository is a Git repository agents may work on, as stored in the blob store. Its token
// is never part of it: the token lives in a Kubernetes Secret only.
type repository struct {
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	URL            string `json:"url"`
	DefaultBranch  string `json:"default_branch"`
	SetupCommand   string `json:"setup_command"`
	IdleMinutes    int    `json:"idle_minutes"`
	ApprovePublish bool   `json:"approve_publish"`
	Credential     string `json:"credential"`
}

// repositoryInput is the body of a PUT. Missing fields take their defaults.
type repositoryInput struct {
	Kind           string  `json:"kind"`
	URL            string  `json:"url"`
	DefaultBranch  string  `json:"default_branch"`
	SetupCommand   string  `json:"setup_command"`
	IdleMinutes    *int    `json:"idle_minutes"`
	ApprovePublish *bool   `json:"approve_publish"`
	Credential     string  `json:"credential"`
	Token          *string `json:"token"`
}

// A repository name is also part of the name of its Secret, so it follows those rules.
var repositoryName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)

func repositoryObject(name string) string { return repositoryPrefix + "/" + name + ".json" }

func credentialSecret(name string) string { return "git-" + name }

func (s *service) loadRepository(ctx context.Context, name string) (repository, error) {
	var repo repository
	if !repositoryName.MatchString(name) {
		return repo, errNotFound
	}
	err := s.blobs.getJSON(ctx, repositoryObject(name), &repo)
	return repo, err
}

func (s *service) listRepositories(w http.ResponseWriter, r *http.Request) {
	ids, err := s.blobs.list(r.Context(), repositoryPrefix)
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	repos := []repository{}
	for _, id := range ids {
		var repo repository
		if err = s.blobs.getJSON(r.Context(), id, &repo); err != nil {
			if errors.Is(err, errNotFound) {
				continue
			}
			s.fail(w, 502, err)
			return
		}
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	writeJSON(w, 200, repos)
}

func (s *service) getRepository(w http.ResponseWriter, r *http.Request) {
	repo, err := s.loadRepository(r.Context(), r.PathValue("name"))
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("this repository does not exist"))
		return
	}
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	writeJSON(w, 200, repo)
}

func (s *service) putRepository(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !repositoryName.MatchString(name) {
		s.fail(w, 400, errors.New("a repository name has 1 to 63 characters: lower-case letters, digits, '-' and '.', starting and ending with a letter or digit"))
		return
	}
	var in repositoryInput
	if err := readJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	old, err := s.loadRepository(r.Context(), name)
	exists := err == nil
	if err != nil && !errors.Is(err, errNotFound) {
		s.fail(w, 502, err)
		return
	}
	token := ""
	if in.Token != nil {
		token = strings.TrimSpace(*in.Token)
	}
	repo, err := in.repository(name, token != "")
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	switch {
	case repo.Credential == "none":
		// Without a credential no token may stay behind.
		if err = s.kube.DeleteSecret(r.Context(), credentialSecret(name)); err != nil {
			s.fail(w, 502, err)
			return
		}
	case token != "":
		labels := map[string]string{"app": "devpod-credential", "repository": name}
		if err = s.kube.ApplySecret(r.Context(), credentialSecret(name), labels, map[string]string{"token": token}); err != nil {
			s.fail(w, 502, err)
			return
		}
	case !exists || old.Credential == "none":
		s.fail(w, 400, errors.New(`credential "`+repo.Credential+`" needs a token: send it as "token"`))
		return
	}
	if err = s.blobs.putJSON(r.Context(), repositoryObject(name), repo, "connectors", ""); err != nil {
		s.fail(w, 502, err)
		return
	}
	writeJSON(w, 200, repo)
}

// repository checks the input and fills in the defaults.
func (in repositoryInput) repository(name string, hasToken bool) (repository, error) {
	repo := repository{
		Name: name, Kind: in.Kind, URL: strings.TrimSpace(in.URL), DefaultBranch: strings.TrimSpace(in.DefaultBranch),
		SetupCommand: in.SetupCommand, IdleMinutes: 30, Credential: in.Credential,
	}
	if repo.URL == "" {
		return repo, errors.New("url is required")
	}
	if _, _, err := repositoryHost(repo.URL); err != nil {
		return repo, err
	}
	if repo.Kind == "" {
		repo.Kind = "plain"
		if host, _, _ := repositoryHost(repo.URL); host == "github.com" {
			repo.Kind = "github"
		}
	}
	if repo.Kind != "github" && repo.Kind != "plain" {
		return repo, errors.New(`kind must be "github" or "plain"`)
	}
	if repo.Kind == "github" {
		if _, _, err := githubRepository(repo.URL); err != nil {
			return repo, err
		}
	}
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = "main"
	}
	if in.IdleMinutes != nil {
		repo.IdleMinutes = *in.IdleMinutes
	}
	if repo.IdleMinutes < 1 {
		return repo, errors.New("idle_minutes must be 1 or more")
	}
	repo.ApprovePublish = repo.Kind == "github"
	if in.ApprovePublish != nil {
		repo.ApprovePublish = *in.ApprovePublish
	}
	if repo.Credential == "" {
		repo.Credential = "none"
		if hasToken {
			repo.Credential = "read"
		}
	}
	if repo.Credential != "none" && repo.Credential != "read" && repo.Credential != "read-write" {
		return repo, errors.New(`credential must be "none", "read" or "read-write"`)
	}
	return repo, nil
}

func (s *service) deleteRepository(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.loadRepository(r.Context(), name); err != nil {
		if errors.Is(err, errNotFound) {
			s.fail(w, 404, errors.New("this repository does not exist"))
			return
		}
		s.fail(w, 502, err)
		return
	}
	s.mu.Lock()
	inUse := false
	for _, p := range s.pods {
		inUse = inUse || p.Repository == name
	}
	s.mu.Unlock()
	if inUse {
		s.fail(w, 409, errors.New("this repository still has dev pods; destroy them first"))
		return
	}
	if err := s.kube.DeleteSecret(r.Context(), credentialSecret(name)); err != nil {
		s.fail(w, 502, err)
		return
	}
	if err := s.blobs.delete(r.Context(), repositoryObject(name)); err != nil {
		s.fail(w, 502, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// repositoryHost returns the host and port a repository URL points at. It reads http, https
// and ssh URLs and the short form git@host:path.
func repositoryHost(raw string) (host string, port string, err error) {
	if !strings.Contains(raw, "://") {
		if at := strings.Index(raw, "@"); at >= 0 {
			if h, _, ok := strings.Cut(raw[at+1:], ":"); ok && h != "" {
				return h, "22", nil
			}
		}
		return "", "", errors.New("url must be an http, https or ssh URL of a Git repository")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", "", errors.New("url must be an http, https or ssh URL of a Git repository")
	}
	port = u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		case "ssh":
			port = "22"
		default:
			return "", "", errors.New("url must be an http, https or ssh URL of a Git repository")
		}
	}
	return u.Hostname(), port, nil
}

// githubRepository returns the owner and the name of a GitHub repository URL.
func githubRepository(raw string) (owner string, name string, err error) {
	path := raw
	if strings.Contains(raw, "://") {
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", "", errors.New("url is not a GitHub repository URL like https://github.com/owner/name")
		}
		path = u.Path
	} else if _, rest, ok := strings.Cut(raw, ":"); ok {
		path = rest
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(strings.TrimSpace(path), ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("url is not a GitHub repository URL like https://github.com/owner/name")
	}
	return parts[0], parts[1], nil
}
