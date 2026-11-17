package main

// GitHub REST v3 under /api/v3: the API root and meta, repositories, branches, refs, commits, contents,
// labels, issues, comments and pull requests.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func decodeBase64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
}

func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func isoPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return iso(*t)
}

func msg(w http.ResponseWriter, status int, m string) {
	writeJSON(w, status, map[string]any{"message": m, "documentation_url": "https://docs.github.com/rest"})
}

func notFound(w http.ResponseWriter) { msg(w, http.StatusNotFound, "Not Found") }

func validation(w http.ResponseWriter, m string, field string, code string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"message":           m,
		"errors":            []any{map[string]any{"resource": "Issue", "field": field, "code": code, "message": m}},
		"documentation_url": "https://docs.github.com/rest",
	})
}

// decodeBody reads a JSON object body; a malformed one answers 400 and false.
func decodeBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	data, err := readBody(r)
	if err != nil {
		msg(w, http.StatusBadRequest, "Problems parsing JSON")
		return nil, false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, true
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		msg(w, http.StatusBadRequest, "Problems parsing JSON")
		return nil, false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		msg(w, http.StatusBadRequest, "Problems parsing JSON")
		return nil, false
	}
	return obj, true
}

func str(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func boolean(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func intOf(v any) (int, bool) {
	switch t := v.(type) {
	case json.Number:
		n, err := strconv.Atoi(t.String())
		return n, err == nil
	case float64:
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(t)
		return n, err == nil
	}
	return 0, false
}

func stringList(v any) []string {
	var out []string
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			switch y := x.(type) {
			case string:
				out = append(out, y)
			case map[string]any:
				if n, ok := y["name"].(string); ok {
					out = append(out, n)
				}
			}
		}
	case string:
		for _, x := range strings.Split(t, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// object shapes

func (s *Server) baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) userJSON(r *http.Request) map[string]any {
	login := s.st.Viewer()
	base := s.baseURL(r)
	return map[string]any{
		"login": login, "id": s.st.userID, "node_id": nodeID("U", s.st.userID), "type": "User", "site_admin": false,
		"avatar_url": base + "/avatars/" + login, "url": base + "/api/v3/users/" + login, "html_url": base + "/" + login,
		"name": login, "email": login + "@users.noreply.example",
	}
}

func (s *Server) repoJSON(r *http.Request, repo *Repo) map[string]any {
	base := s.baseURL(r)
	owner := s.st.Viewer()
	full := owner + "/" + repo.Name
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	return map[string]any{
		"id": repo.ID, "node_id": nodeID("R", repo.ID), "name": repo.Name, "full_name": full,
		"owner": s.userJSON(r), "private": repo.Private, "visibility": visibility, "description": repo.Description,
		"fork": false, "default_branch": repo.DefaultBranch, "html_url": base + "/" + full,
		"url": base + "/api/v3/repos/" + full, "clone_url": base + "/" + full + ".git",
		"git_url": base + "/" + full + ".git", "has_issues": true, "has_projects": false, "has_wiki": false,
		"archived": false, "disabled": false, "open_issues_count": s.openIssueCount(repo),
		"created_at": iso(repo.CreatedAt), "updated_at": iso(repo.UpdatedAt), "pushed_at": iso(repo.PushedAt),
		"permissions": map[string]any{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true},
		"size": 0, "stargazers_count": 0, "watchers_count": 0, "forks_count": 0,
	}
}

func (s *Server) openIssueCount(repo *Repo) int {
	s.st.mu.RLock()
	defer s.st.mu.RUnlock()
	n := 0
	for _, is := range repo.Issues {
		if is.State == "open" {
			n++
		}
	}
	return n
}

func labelJSON(l *Label) map[string]any {
	return map[string]any{"id": l.ID, "node_id": nodeID("LA", l.ID), "name": l.Name, "color": l.Color, "description": l.Description, "default": false}
}

func (s *Server) issueJSON(r *http.Request, repo *Repo, is *Issue) map[string]any {
	base := s.baseURL(r)
	full := s.st.Viewer() + "/" + repo.Name
	s.st.mu.RLock()
	labels := make([]any, 0, len(is.Labels))
	for _, l := range is.Labels {
		labels = append(labels, labelJSON(l))
	}
	comments := len(is.Comments)
	state, title, body, updated, closed := is.State, is.Title, is.Body, is.UpdatedAt, is.ClosedAt
	s.st.mu.RUnlock()
	stateReason := any(nil)
	if state == "closed" {
		stateReason = "completed"
	}
	return map[string]any{
		"id": is.ID, "node_id": nodeID("I", is.ID), "number": is.Number, "title": title, "body": body,
		"state": state, "state_reason": stateReason, "labels": labels, "user": s.userJSON(r), "assignee": nil, "assignees": []any{},
		"comments": comments, "locked": false, "created_at": iso(is.CreatedAt), "updated_at": iso(updated),
		"closed_at": isoPtr(closed), "author_association": "OWNER", "milestone": nil,
		"url": base + "/api/v3/repos/" + full + "/issues/" + itoa(is.Number), "html_url": base + "/" + full + "/issues/" + itoa(is.Number),
		"repository_url": base + "/api/v3/repos/" + full,
	}
}

func (s *Server) commentJSON(r *http.Request, c *Comment) map[string]any {
	return map[string]any{"id": c.ID, "node_id": nodeID("IC", c.ID), "body": c.Body, "user": s.userJSON(r),
		"created_at": iso(c.CreatedAt), "updated_at": iso(c.CreatedAt), "author_association": "OWNER"}
}

func (s *Server) pullJSON(r *http.Request, repo *Repo, p *Pull) map[string]any {
	base := s.baseURL(r)
	full := s.st.Viewer() + "/" + repo.Name
	head, baseSha := s.st.pullShas(repo, p)
	s.st.mu.RLock()
	state, title, body, merged, mergedAt, closedAt, updated, mcs := p.State, p.Title, p.Body, p.Merged, p.MergedAt, p.ClosedAt, p.UpdatedAt, p.MergeCommitSha
	s.st.mu.RUnlock()
	var mergeCommit any
	if !mcs.IsZero() {
		mergeCommit = mcs.String()
	}
	var mergeable any = nil
	mergeableState := "unknown"
	if state == "open" {
		ok := s.st.mergeable(repo, p)
		mergeable = ok
		if ok {
			mergeableState = "clean"
		} else {
			mergeableState = "dirty"
		}
	}
	repoObj := s.repoJSON(r, repo)
	ref := func(name string, sha Oid) map[string]any {
		return map[string]any{"label": s.st.Viewer() + ":" + name, "ref": name, "sha": sha.String(), "user": s.userJSON(r), "repo": repoObj}
	}
	return map[string]any{
		"id": p.ID, "node_id": nodeID("PR", p.ID), "number": p.Number, "title": title, "body": body, "state": state,
		"locked": false, "draft": false, "merged": merged, "merged_at": isoPtr(mergedAt), "closed_at": isoPtr(closedAt),
		"merge_commit_sha": mergeCommit, "mergeable": mergeable, "mergeable_state": mergeableState,
		"head": ref(p.Head, head), "base": ref(p.Base, baseSha), "user": s.userJSON(r),
		"created_at": iso(p.CreatedAt), "updated_at": iso(updated), "author_association": "OWNER",
		"url": base + "/api/v3/repos/" + full + "/pulls/" + itoa(p.Number), "html_url": base + "/" + full + "/pull/" + itoa(p.Number),
		"diff_url": base + "/" + full + "/pull/" + itoa(p.Number) + ".diff", "comments": len(p.Comments), "commits": 1,
	}
}

func (s *Server) commitJSON(r *http.Request, repo *Repo, o Oid) map[string]any {
	c := s.st.db.commit(o)
	if c == nil {
		return nil
	}
	base := s.baseURL(r)
	full := s.st.Viewer() + "/" + repo.Name
	an, ae := identNameEmail(c.Author)
	cn, ce := identNameEmail(c.Committer)
	parents := make([]any, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, map[string]any{"sha": p.String(), "url": base + "/api/v3/repos/" + full + "/commits/" + p.String()})
	}
	message := strings.TrimSuffix(c.Message, "\n")
	return map[string]any{
		"sha": o.String(), "node_id": "C_" + o.String(), "url": base + "/api/v3/repos/" + full + "/commits/" + o.String(),
		"html_url": base + "/" + full + "/commit/" + o.String(),
		"commit": map[string]any{
			"message":   message,
			"author":    map[string]any{"name": an, "email": ae, "date": iso(time.Unix(identTime(c.Author), 0))},
			"committer": map[string]any{"name": cn, "email": ce, "date": iso(time.Unix(identTime(c.Committer), 0))},
			"tree":      map[string]any{"sha": c.Tree.String()},
		},
		"author": s.userJSON(r), "committer": s.userJSON(r), "parents": parents,
	}
}

// ---------------------------------------------------------------------------------------------
// routing

func (s *Server) serveREST(w http.ResponseWriter, r *http.Request, path string) {
	if path == "" || path == "/" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			notFound(w)
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, read:org, workflow, gist")
		w.Header().Set("X-Accepted-OAuth-Scopes", "")
		base := s.baseURL(r) + "/api/v3"
		writeJSON(w, http.StatusOK, map[string]any{
			"current_user_url": base + "/user", "repository_url": base + "/repos/{owner}/{repo}",
			"issues_url": base + "/repos/{owner}/{repo}/issues", "user_repositories_url": base + "/users/{user}/repos",
		})
		return
	}
	w.Header().Set("X-OAuth-Scopes", "repo, read:org, workflow, gist")
	seg := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range seg {
		if u, err := url.PathUnescape(p); err == nil {
			seg[i] = u
		}
	}
	m := r.Method
	switch {
	case len(seg) == 1 && seg[0] == "meta" && m == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"installed_version": "3.14.0", "verifiable_password_authentication": false})
		return
	case len(seg) == 1 && seg[0] == "rate_limit":
		writeJSON(w, http.StatusOK, map[string]any{"resources": map[string]any{}, "rate": map[string]any{"limit": 5000, "remaining": 5000, "reset": time.Now().Add(time.Hour).Unix(), "used": 0}})
		return
	case len(seg) == 1 && seg[0] == "user" && m == http.MethodGet:
		writeJSON(w, http.StatusOK, s.userJSON(r))
		return
	case len(seg) == 2 && seg[0] == "user" && seg[1] == "repos":
		switch m {
		case http.MethodGet:
			s.listUserRepos(w, r)
		case http.MethodPost:
			s.createRepoREST(w, r)
		default:
			notFound(w)
		}
		return
	case len(seg) == 2 && seg[0] == "users" && m == http.MethodGet:
		if s.st.ownerOK(seg[1]) {
			writeJSON(w, http.StatusOK, s.userJSON(r))
		} else {
			notFound(w)
		}
		return
	case len(seg) == 3 && seg[0] == "users" && seg[2] == "repos" && m == http.MethodGet:
		if !s.st.ownerOK(seg[1]) {
			notFound(w)
			return
		}
		s.listUserRepos(w, r)
		return
	case len(seg) >= 3 && seg[0] == "repos":
		repo := s.st.repo(seg[1], seg[2])
		if repo == nil {
			notFound(w)
			return
		}
		s.serveRepo(w, r, repo, seg[3:])
		return
	}
	notFound(w)
}

func pageParams(r *http.Request) (int, int) {
	q := r.URL.Query()
	per, err := strconv.Atoi(q.Get("per_page"))
	if err != nil || per <= 0 {
		per = 30
	}
	if per > 100 {
		per = 100
	}
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page <= 0 {
		page = 1
	}
	return per, page
}

// paginate slices n items and sets the Link header GitHub sends.
func (s *Server) paginate(w http.ResponseWriter, r *http.Request, n int) (int, int) {
	per, page := pageParams(r)
	start := (page - 1) * per
	if start > n {
		start = n
	}
	end := start + per
	if end > n {
		end = n
	}
	last := (n + per - 1) / per
	if last < 1 {
		last = 1
	}
	link := func(p int, rel string) string {
		q := r.URL.Query()
		q.Set("per_page", strconv.Itoa(per))
		q.Set("page", strconv.Itoa(p))
		return "<" + s.baseURL(r) + r.URL.Path + "?" + q.Encode() + ">; rel=\"" + rel + "\""
	}
	var links []string
	if page < last {
		links = append(links, link(page+1, "next"), link(last, "last"))
	}
	if page > 1 {
		links = append(links, link(1, "first"), link(page-1, "prev"))
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	return start, end
}

func (s *Server) listUserRepos(w http.ResponseWriter, r *http.Request) {
	repos := s.st.listRepos()
	start, end := s.paginate(w, r, len(repos))
	out := make([]any, 0, end-start)
	for _, repo := range repos[start:end] {
		out = append(out, s.repoJSON(r, repo))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createRepoREST(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeBody(w, r)
	if !ok {
		return
	}
	name, _ := str(body, "name")
	if !validRepoName(name) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Repository creation failed.", "errors": []any{map[string]any{"resource": "Repository", "code": "missing_field", "field": "name"}}})
		return
	}
	desc, _ := str(body, "description")
	private, _ := boolean(body, "private")
	if v, ok := str(body, "visibility"); ok && v == "private" {
		private = true
	}
	repo, err := s.st.createRepo(name, desc, private)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Repository creation failed: name already exists on this account",
			"errors": []any{map[string]any{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"}}})
		return
	}
	if auto, _ := boolean(body, "auto_init"); auto {
		s.st.putFile(repo, "", "README.md", []byte("# "+name+"\n\n"+desc+"\n"), "Initial commit", "")
	}
	writeJSON(w, http.StatusCreated, s.repoJSON(r, repo))
}

func (s *Server) serveRepo(w http.ResponseWriter, r *http.Request, repo *Repo, seg []string) {
	m := r.Method
	if len(seg) == 0 {
		switch m {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, s.repoJSON(r, repo))
		case http.MethodPatch:
			body, ok := decodeBody(w, r)
			if !ok {
				return
			}
			s.st.mu.Lock()
			if d, ok := str(body, "description"); ok {
				repo.Description = d
			}
			if p, ok := boolean(body, "private"); ok {
				repo.Private = p
			}
			if b, ok := str(body, "default_branch"); ok && b != "" {
				repo.DefaultBranch = b
			}
			repo.UpdatedAt = time.Now().UTC()
			s.st.mu.Unlock()
			if n, ok := str(body, "name"); ok && n != "" && n != repo.Name && validRepoName(n) {
				s.st.mu.Lock()
				if _, taken := s.st.repos[strings.ToLower(n)]; !taken {
					delete(s.st.repos, strings.ToLower(repo.Name))
					repo.Name = n
					s.st.repos[strings.ToLower(n)] = repo
				}
				s.st.mu.Unlock()
			}
			writeJSON(w, http.StatusOK, s.repoJSON(r, repo))
		case http.MethodDelete:
			s.st.deleteRepo(repo)
			writeRawJSON(w, http.StatusNoContent, nil)
		default:
			notFound(w)
		}
		return
	}
	switch seg[0] {
	case "branches":
		s.branches(w, r, repo, seg[1:])
	case "git":
		s.gitData(w, r, repo, seg[1:])
	case "commits":
		s.commits(w, r, repo, seg[1:])
	case "contents":
		s.contents(w, r, repo, strings.Join(seg[1:], "/"))
	case "labels":
		s.labels(w, r, repo, seg[1:])
	case "issues":
		s.issues(w, r, repo, seg[1:])
	case "pulls":
		s.pulls(w, r, repo, seg[1:])
	default:
		notFound(w)
	}
}

func (s *Server) branchJSON(r *http.Request, repo *Repo, name string, o Oid) map[string]any {
	base := s.baseURL(r)
	full := s.st.Viewer() + "/" + repo.Name
	return map[string]any{"name": name, "protected": false,
		"commit": map[string]any{"sha": o.String(), "url": base + "/api/v3/repos/" + full + "/commits/" + o.String()}}
}

func (s *Server) branches(w http.ResponseWriter, r *http.Request, repo *Repo, seg []string) {
	if r.Method != http.MethodGet {
		notFound(w)
		return
	}
	refs, _, _ := s.st.refsSnapshot(repo)
	if len(seg) > 0 {
		name := strings.Join(seg, "/")
		o, ok := refs["refs/heads/"+name]
		if !ok {
			msg(w, http.StatusNotFound, "Branch not found")
			return
		}
		b := s.branchJSON(r, repo, name, o)
		b["commit"] = s.commitJSON(r, repo, o)
		writeJSON(w, http.StatusOK, b)
		return
	}
	var names []string
	for _, n := range sortedRefNames(refs) {
		if strings.HasPrefix(n, "refs/heads/") {
			names = append(names, n)
		}
	}
	start, end := s.paginate(w, r, len(names))
	out := make([]any, 0, end-start)
	for _, n := range names[start:end] {
		out = append(out, s.branchJSON(r, repo, strings.TrimPrefix(n, "refs/heads/"), refs[n]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) refJSON(r *http.Request, repo *Repo, name string, o Oid) map[string]any {
	base := s.baseURL(r)
	full := s.st.Viewer() + "/" + repo.Name
	t := "commit"
	if obj := s.st.db.Get(o); obj != nil {
		t = typeName(obj.Type)
	}
	return map[string]any{"ref": name, "node_id": "REF_" + o.String()[:12], "url": base + "/api/v3/repos/" + full + "/git/" + name,
		"object": map[string]any{"sha": o.String(), "type": t, "url": base + "/api/v3/repos/" + full + "/git/" + t + "s/" + o.String()}}
}

func (s *Server) gitData(w http.ResponseWriter, r *http.Request, repo *Repo, seg []string) {
	if len(seg) == 0 {
		notFound(w)
		return
	}
	switch seg[0] {
	case "refs", "ref":
		if r.Method == http.MethodPost && len(seg) == 1 {
			body, ok := decodeBody(w, r)
			if !ok {
				return
			}
			ref, _ := str(body, "ref")
			sha, _ := str(body, "sha")
			o, okSha := parseOid(strings.ToLower(sha))
			if !strings.HasPrefix(ref, "refs/") || strings.Count(ref, "/") < 2 {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Reference name is not well-formed"})
				return
			}
			if !okSha || !s.st.db.Has(o) {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Object does not exist"})
				return
			}
			s.st.mu.Lock()
			if _, exists := repo.Refs[ref]; exists {
				s.st.mu.Unlock()
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Reference already exists"})
				return
			}
			s.st.setRefLocked(repo, ref, o)
			s.st.mu.Unlock()
			writeJSON(w, http.StatusCreated, s.refJSON(r, repo, ref, o))
			return
		}
		name := "refs/" + strings.Join(seg[1:], "/")
		refs, _, _ := s.st.refsSnapshot(repo)
		switch r.Method {
		case http.MethodGet:
			if o, ok := refs[name]; ok && len(seg) > 1 {
				writeJSON(w, http.StatusOK, s.refJSON(r, repo, name, o))
				return
			}
			var out []any
			for _, n := range sortedRefNames(refs) {
				if strings.HasPrefix(n, name+"/") || (len(seg) == 1) {
					out = append(out, s.refJSON(r, repo, n, refs[n]))
				}
			}
			if len(out) == 0 {
				notFound(w)
				return
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPatch:
			body, ok := decodeBody(w, r)
			if !ok {
				return
			}
			sha, _ := str(body, "sha")
			force, _ := boolean(body, "force")
			o, okSha := parseOid(strings.ToLower(sha))
			cur, exists := refs[name]
			if !exists {
				notFound(w)
				return
			}
			if !okSha || !s.st.db.Has(o) {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Object does not exist"})
				return
			}
			if !force && !s.st.db.isAncestor(cur, o) {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Update is not a fast forward"})
				return
			}
			s.st.mu.Lock()
			s.st.setRefLocked(repo, name, o)
			s.st.mu.Unlock()
			writeJSON(w, http.StatusOK, s.refJSON(r, repo, name, o))
		case http.MethodDelete:
			if _, exists := refs[name]; !exists {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Reference does not exist"})
				return
			}
			s.st.mu.Lock()
			s.st.setRefLocked(repo, name, zeroOid)
			s.st.mu.Unlock()
			writeRawJSON(w, http.StatusNoContent, nil)
		default:
			notFound(w)
		}
	case "commits":
		if len(seg) == 2 && r.Method == http.MethodGet {
			if o, ok := parseOid(strings.ToLower(seg[1])); ok {
				if c := s.commitJSON(r, repo, o); c != nil {
					inner := c["commit"].(map[string]any)
					inner["sha"] = o.String()
					inner["parents"] = c["parents"]
					writeJSON(w, http.StatusOK, inner)
					return
				}
			}
		}
		notFound(w)
	default:
		notFound(w)
	}
}

func (s *Server) commits(w http.ResponseWriter, r *http.Request, repo *Repo, seg []string) {
	if r.Method != http.MethodGet {
		notFound(w)
		return
	}
	if len(seg) > 0 {
		o, ok := s.st.resolveRev(repo, strings.Join(seg, "/"))
		if !ok {
			msg(w, http.StatusNotFound, "No commit found for SHA: "+strings.Join(seg, "/"))
			return
		}
		writeJSON(w, http.StatusOK, s.commitJSON(r, repo, o))
		return
	}
	rev := r.URL.Query().Get("sha")
	tip, ok := s.st.resolveRev(repo, rev)
	if !ok {
		if rev == "" {
			writeJSON(w, http.StatusConflict, map[string]any{"message": "Git Repository is empty."})
			return
		}
		msg(w, http.StatusNotFound, "No commit found for SHA: "+rev)
		return
	}
	log := s.st.db.commitLog(tip, 0)
	start, end := s.paginate(w, r, len(log))
	out := make([]any, 0, end-start)
	for _, o := range log[start:end] {
		out = append(out, s.commitJSON(r, repo, o))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) contentEntryJSON(r *http.Request, repo *Repo, path string, e TreeEntry, ref string) map[string]any {
	base := s.baseURL(r)
	full := s.st.Viewer() + "/" + repo.Name
	t := "file"
	size := 0
	if e.IsDir() {
		t = "dir"
	} else if e.Mode == "160000" {
		t = "submodule"
	} else if e.Mode == "120000" {
		t = "symlink"
	}
	if t == "file" || t == "symlink" {
		size = len(s.st.blobData(e.Oid))
	}
	name := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		name = path[i+1:]
	}
	u := base + "/api/v3/repos/" + full + "/contents/" + path
	if ref != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	return map[string]any{"type": t, "name": name, "path": path, "sha": e.Oid.String(), "size": size, "url": u,
		"html_url": base + "/" + full + "/blob/" + ref + "/" + path, "git_url": base + "/api/v3/repos/" + full + "/git/blobs/" + e.Oid.String(),
		"download_url": base + "/" + full + "/raw/" + path}
}

func (s *Server) contents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	path = strings.Trim(path, "/")
	switch r.Method {
	case http.MethodGet:
		ref := r.URL.Query().Get("ref")
		tip, ok := s.st.resolveRev(repo, ref)
		if !ok {
			if ref == "" {
				msg(w, http.StatusNotFound, "This repository is empty.")
			} else {
				msg(w, http.StatusNotFound, "No commit found for the ref "+ref)
			}
			return
		}
		c := s.st.db.commit(tip)
		if c == nil {
			notFound(w)
			return
		}
		e, found := s.st.db.lookupPath(c.Tree, path)
		if !found {
			notFound(w)
			return
		}
		if e.IsDir() {
			obj := s.st.db.Get(e.Oid)
			entries, _ := parseTree(obj.Data)
			out := make([]any, 0, len(entries))
			for _, child := range entries {
				p := child.Name
				if path != "" {
					p = path + "/" + child.Name
				}
				out = append(out, s.contentEntryJSON(r, repo, p, child, ref))
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		obj := s.contentEntryJSON(r, repo, path, e, ref)
		obj["encoding"] = "base64"
		obj["content"] = base64.StdEncoding.EncodeToString(s.st.blobData(e.Oid))
		writeJSON(w, http.StatusOK, obj)
	case http.MethodPut:
		body, ok := decodeBody(w, r)
		if !ok {
			return
		}
		if path == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "path is required"})
			return
		}
		content, hasContent := str(body, "content")
		message, _ := str(body, "message")
		if !hasContent {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Invalid request.\n\n\"content\" wasn't supplied."})
			return
		}
		data, err := decodeBase64(content)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "content is not valid Base64"})
			return
		}
		if message == "" {
			message = "Update " + path
		}
		branch, _ := str(body, "branch")
		sha, _ := str(body, "sha")
		res, err := s.st.putFile(repo, branch, path, data, message, sha)
		switch err {
		case nil:
		case errStale:
			writeJSON(w, http.StatusConflict, map[string]any{"message": path + " does not match " + sha})
			return
		case errMissingSha:
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Invalid request.\n\n\"sha\" wasn't supplied."})
			return
		default:
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": err.Error()})
			return
		}
		status := http.StatusOK
		if res.Created {
			status = http.StatusCreated
		}
		usedBranch := branch
		if usedBranch == "" {
			usedBranch = repo.DefaultBranch
		}
		entry := s.contentEntryJSON(r, repo, path, TreeEntry{Mode: "100644", Oid: res.BlobSha}, usedBranch)
		entry["encoding"] = "base64"
		entry["content"] = base64.StdEncoding.EncodeToString(data)
		commit := s.commitJSON(r, repo, res.Commit)
		inner := commit["commit"].(map[string]any)
		inner["sha"] = res.Commit.String()
		inner["parents"] = commit["parents"]
		inner["html_url"] = commit["html_url"]
		writeJSON(w, status, map[string]any{"content": entry, "commit": inner})
	case http.MethodDelete:
		body, ok := decodeBody(w, r)
		if !ok {
			return
		}
		message, _ := str(body, "message")
		if message == "" {
			message = "Delete " + path
		}
		branch, _ := str(body, "branch")
		sha, _ := str(body, "sha")
		commit, err := s.st.deleteFile(repo, branch, path, message, sha)
		switch err {
		case nil:
		case errStale:
			writeJSON(w, http.StatusConflict, map[string]any{"message": path + " does not match " + sha})
			return
		case errMissingSha:
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Invalid request.\n\n\"sha\" wasn't supplied."})
			return
		default:
			notFound(w)
			return
		}
		cj := s.commitJSON(r, repo, commit)
		inner := cj["commit"].(map[string]any)
		inner["sha"] = commit.String()
		inner["parents"] = cj["parents"]
		writeJSON(w, http.StatusOK, map[string]any{"content": nil, "commit": inner})
	default:
		notFound(w)
	}
}

func (s *Server) labels(w http.ResponseWriter, r *http.Request, repo *Repo, seg []string) {
	switch {
	case len(seg) == 0 && r.Method == http.MethodGet:
		s.st.mu.RLock()
		out := make([]any, 0, len(repo.Labels))
		for _, l := range repo.Labels {
			out = append(out, labelJSON(l))
		}
		s.st.mu.RUnlock()
		writeJSON(w, http.StatusOK, out)
	case len(seg) == 0 && r.Method == http.MethodPost:
		body, ok := decodeBody(w, r)
		if !ok {
			return
		}
		name, _ := str(body, "name")
		if strings.TrimSpace(name) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "Label", "code": "missing_field", "field": "name"}}})
			return
		}
		color, _ := str(body, "color")
		desc, _ := str(body, "description")
		l, err := s.st.createLabel(repo, name, color, desc)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "Label", "code": "already_exists", "field": "name"}}})
			return
		}
		writeJSON(w, http.StatusCreated, labelJSON(l))
	case len(seg) == 1 && r.Method == http.MethodGet:
		s.st.mu.RLock()
		l := s.st.findLabel(repo, seg[0])
		s.st.mu.RUnlock()
		if l == nil {
			notFound(w)
			return
		}
		writeJSON(w, http.StatusOK, labelJSON(l))
	default:
		notFound(w)
	}
}

// filteredIssues answers the issues matching a state filter, sorted as asked.
func (s *Server) filteredIssues(repo *Repo, state, sortBy, direction string, labels []string) []*Issue {
	s.st.mu.RLock()
	var out []*Issue
	for _, is := range repo.Issues {
		if state != "all" && is.State != state {
			continue
		}
		if len(labels) > 0 {
			match := true
			for _, want := range labels {
				found := false
				for _, l := range is.Labels {
					if strings.EqualFold(l.Name, want) {
						found = true
					}
				}
				if !found {
					match = false
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, is)
	}
	s.st.mu.RUnlock()
	asc := direction == "asc"
	key := func(is *Issue) int64 {
		if sortBy == "updated" {
			return is.UpdatedAt.UnixNano()
		}
		if sortBy == "comments" {
			return int64(len(is.Comments))
		}
		return is.seq
	}
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := key(out[i]), key(out[j])
		if ki == kj {
			if asc {
				return out[i].Number < out[j].Number
			}
			return out[i].Number > out[j].Number
		}
		if asc {
			return ki < kj
		}
		return ki > kj
	})
	return out
}

func (s *Server) issues(w http.ResponseWriter, r *http.Request, repo *Repo, seg []string) {
	m := r.Method
	if len(seg) == 0 {
		switch m {
		case http.MethodGet:
			q := r.URL.Query()
			state := q.Get("state")
			if state == "" {
				state = "open"
			}
			list := s.filteredIssues(repo, state, q.Get("sort"), q.Get("direction"), stringList(q.Get("labels")))
			start, end := s.paginate(w, r, len(list))
			out := make([]any, 0, end-start)
			for _, is := range list[start:end] {
				out = append(out, s.issueJSON(r, repo, is))
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			body, ok := decodeBody(w, r)
			if !ok {
				return
			}
			title, _ := str(body, "title")
			if strings.TrimSpace(title) == "" {
				validation(w, "Validation Failed", "title", "missing_field")
				return
			}
			text, _ := str(body, "body")
			is, err := s.st.createIssue(repo, title, text, stringList(body["labels"]))
			if err != nil {
				validation(w, "Validation Failed: the label does not exist", "labels", "invalid")
				return
			}
			writeJSON(w, http.StatusCreated, s.issueJSON(r, repo, is))
		default:
			notFound(w)
		}
		return
	}
	n, err := strconv.Atoi(seg[0])
	if err != nil {
		notFound(w)
		return
	}
	is := s.st.issue(repo, n)
	if is == nil {
		notFound(w)
		return
	}
	if len(seg) == 1 {
		switch m {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, s.issueJSON(r, repo, is))
		case http.MethodPatch:
			body, ok := decodeBody(w, r)
			if !ok {
				return
			}
			if v, ok := body["labels"]; ok {
				var ls []*Label
				s.st.mu.RLock()
				for _, name := range stringList(v) {
					l := s.st.findLabel(repo, name)
					if l == nil {
						s.st.mu.RUnlock()
						validation(w, "Validation Failed: the label does not exist", "labels", "invalid")
						return
					}
					ls = append(ls, l)
				}
				s.st.mu.RUnlock()
				s.st.mu.Lock()
				is.Labels = ls
				s.st.mu.Unlock()
			}
			s.st.mu.Lock()
			if t, ok := str(body, "title"); ok && t != "" {
				is.Title = t
			}
			if b, ok := str(body, "body"); ok {
				is.Body = b
			}
			is.UpdatedAt = time.Now().UTC()
			s.st.mu.Unlock()
			if st, ok := str(body, "state"); ok {
				if st != "open" && st != "closed" {
					validation(w, "Validation Failed", "state", "invalid")
					return
				}
				s.st.setIssueState(is, st)
			}
			writeJSON(w, http.StatusOK, s.issueJSON(r, repo, is))
		default:
			notFound(w)
		}
		return
	}
	if len(seg) == 2 && seg[1] == "comments" {
		switch m {
		case http.MethodGet:
			s.st.mu.RLock()
			comments := append([]*Comment(nil), is.Comments...)
			s.st.mu.RUnlock()
			out := make([]any, 0, len(comments))
			for _, c := range comments {
				out = append(out, s.commentJSON(r, c))
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			body, ok := decodeBody(w, r)
			if !ok {
				return
			}
			text, ok := str(body, "body")
			if !ok || text == "" {
				validation(w, "Validation Failed", "body", "missing_field")
				return
			}
			c := s.st.addComment(&is.Comments, text)
			writeJSON(w, http.StatusCreated, s.commentJSON(r, c))
		default:
			notFound(w)
		}
		return
	}
	if len(seg) == 2 && seg[1] == "labels" && m == http.MethodGet {
		s.st.mu.RLock()
		out := make([]any, 0, len(is.Labels))
		for _, l := range is.Labels {
			out = append(out, labelJSON(l))
		}
		s.st.mu.RUnlock()
		writeJSON(w, http.StatusOK, out)
		return
	}
	notFound(w)
}

func (s *Server) pullList(repo *Repo, state string) []*Pull {
	s.st.mu.RLock()
	defer s.st.mu.RUnlock()
	var out []*Pull
	for _, p := range repo.Pulls {
		if state == "all" || p.State == state {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) pulls(w http.ResponseWriter, r *http.Request, repo *Repo, seg []string) {
	m := r.Method
	if len(seg) == 0 {
		switch m {
		case http.MethodGet:
			q := r.URL.Query()
			state := q.Get("state")
			if state == "" {
				state = "open"
			}
			list := s.pullList(repo, state)
			if q.Get("direction") != "asc" {
				for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
					list[i], list[j] = list[j], list[i]
				}
			}
			if head := q.Get("head"); head != "" {
				if i := strings.IndexByte(head, ':'); i >= 0 {
					head = head[i+1:]
				}
				var filtered []*Pull
				for _, p := range list {
					if p.Head == head {
						filtered = append(filtered, p)
					}
				}
				list = filtered
			}
			start, end := s.paginate(w, r, len(list))
			out := make([]any, 0, end-start)
			for _, p := range list[start:end] {
				out = append(out, s.pullJSON(r, repo, p))
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			body, ok := decodeBody(w, r)
			if !ok {
				return
			}
			title, _ := str(body, "title")
			head, _ := str(body, "head")
			base, _ := str(body, "base")
			text, _ := str(body, "body")
			if head == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "PullRequest", "code": "missing_field", "field": "head"}}})
				return
			}
			if title == "" {
				title = head
			}
			p, err := s.st.createPull(repo, title, text, head, base)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed: " + err.Error(),
					"errors": []any{map[string]any{"resource": "PullRequest", "code": "invalid", "message": err.Error()}}})
				return
			}
			writeJSON(w, http.StatusCreated, s.pullJSON(r, repo, p))
		default:
			notFound(w)
		}
		return
	}
	n, err := strconv.Atoi(seg[0])
	if err != nil {
		notFound(w)
		return
	}
	p := s.st.pull(repo, n)
	if p == nil {
		notFound(w)
		return
	}
	if len(seg) == 1 {
		switch m {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, s.pullJSON(r, repo, p))
		case http.MethodPatch:
			body, ok := decodeBody(w, r)
			if !ok {
				return
			}
			s.st.mu.Lock()
			if t, ok := str(body, "title"); ok && t != "" {
				p.Title = t
			}
			if b, ok := str(body, "body"); ok {
				p.Body = b
			}
			s.st.mu.Unlock()
			if st, ok := str(body, "state"); ok && (st == "open" || st == "closed") && !p.Merged {
				s.st.setPullState(repo, p, st)
			}
			writeJSON(w, http.StatusOK, s.pullJSON(r, repo, p))
		default:
			notFound(w)
		}
		return
	}
	switch seg[1] {
	case "files":
		files := s.st.pullFiles(repo, p)
		start, end := s.paginate(w, r, len(files))
		out := make([]any, 0, end-start)
		for _, f := range files[start:end] {
			sha := f.NewOid
			if sha.IsZero() {
				sha = f.OldOid
			}
			out = append(out, map[string]any{"sha": sha.String(), "filename": f.Path, "status": f.Status,
				"additions": f.Additions, "deletions": f.Deletions, "changes": f.Additions + f.Deletions})
		}
		writeJSON(w, http.StatusOK, out)
	case "commits":
		head, base := s.st.pullShas(repo, p)
		baseSet := s.st.db.ancestors(base)
		var out []any
		log := s.st.db.commitLog(head, 250)
		for i := len(log) - 1; i >= 0; i-- {
			if !baseSet[log[i]] {
				out = append(out, s.commitJSON(r, repo, log[i]))
			}
		}
		if out == nil {
			out = []any{}
		}
		writeJSON(w, http.StatusOK, out)
	case "merge":
		switch m {
		case http.MethodGet:
			if p.Merged {
				writeRawJSON(w, http.StatusNoContent, nil)
			} else {
				notFound(w)
			}
		case http.MethodPut:
			body, ok := decodeBody(w, r)
			if !ok {
				return
			}
			title, _ := str(body, "commit_title")
			message, _ := str(body, "commit_message")
			sha, err := s.st.mergePull(repo, p, title, message)
			if err != nil {
				msg(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"sha": sha.String(), "merged": true, "message": "Pull Request successfully merged"})
		default:
			notFound(w)
		}
	default:
		notFound(w)
	}
}
