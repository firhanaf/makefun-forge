package main

// The forge's state: one viewer (the token's login), its repositories with their refs, issues, labels,
// comments and pull requests, and the operations the REST, GraphQL and MCP surfaces share.

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

type Label struct {
	ID          int64
	Name        string
	Color       string
	Description string
}

type Comment struct {
	ID        int64
	Body      string
	CreatedAt time.Time
}

type Issue struct {
	ID        int64
	Number    int
	Title     string
	Body      string
	State     string // open | closed
	Labels    []*Label
	Comments  []*Comment
	CreatedAt time.Time
	UpdatedAt time.Time
	ClosedAt  *time.Time
	seq       int64 // creation order across the forge
}

type Pull struct {
	ID             int64
	Number         int
	Title          string
	Body           string
	State          string // open | closed
	Head           string
	Base           string
	HeadSha        Oid // frozen once closed
	BaseSha        Oid
	Merged         bool
	MergedAt       *time.Time
	MergeCommitSha Oid
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ClosedAt       *time.Time
	Comments       []*Comment
}

type Repo struct {
	ID            int64
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	PushedAt      time.Time

	Refs      map[string]Oid
	RefsGen   int64 // bumps on every ref change: the pack cache key
	Labels    []*Label
	Issues    []*Issue // by creation order; Issues[i].Number is not i+1 when pulls share numbers
	issueByN  map[int]*Issue
	Pulls     []*Pull
	pullByN   map[int]*Pull
	nextIssue int
	nextPull  int
}

type Store struct {
	mu     sync.RWMutex
	db     *ODB
	token  string
	viewer string // learned from the first owner the suite names
	userID int64

	repos     map[string]*Repo // by lower-case name: every repository is the viewer's
	repoOrder []*Repo
	nextID    int64
	issueSeq  int64
	byNodeID  map[int64]any // database id -> *Repo / *Issue / *Pull / *Comment

	packMu    sync.Mutex
	packCache map[string]*packResult
}

type packResult struct {
	once sync.Once
	body []byte
}

func newStore(token string) *Store {
	return &Store{
		db:        newODB(),
		token:     token,
		userID:    1001,
		repos:     map[string]*Repo{},
		nextID:    100000,
		byNodeID:  map[int64]any{},
		packCache: map[string]*packResult{},
	}
}

func (s *Store) newID() int64 {
	s.nextID++
	return s.nextID
}

// Viewer is the token's login once known (the empty string before the suite named an owner).
func (s *Store) Viewer() string {
	s.mu.RLock()
	v := s.viewer
	s.mu.RUnlock()
	if v == "" {
		return "viewer"
	}
	return v
}

// ownerOK learns the viewer from the first owner a request names and answers whether owner is the viewer.
func (s *Store) ownerOK(owner string) bool {
	if owner == "" {
		return false
	}
	s.mu.RLock()
	v := s.viewer
	s.mu.RUnlock()
	if v == "" {
		s.mu.Lock()
		if s.viewer == "" {
			s.viewer = owner
		}
		v = s.viewer
		s.mu.Unlock()
	}
	return strings.EqualFold(v, owner)
}

// repo answers the viewer's repository named name under owner, or nil.
func (s *Store) repo(owner, name string) *Repo {
	if !s.ownerOK(owner) {
		return nil
	}
	s.mu.RLock()
	r := s.repos[strings.ToLower(name)]
	s.mu.RUnlock()
	return r
}

var errExists = errors.New("name already exists on this account")

func validRepoName(name string) bool {
	if name == "" || len(name) > 100 || name == "." || name == ".." || strings.HasSuffix(name, ".git") {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (s *Store) createRepo(name, description string, private bool) (*Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(name)
	if _, ok := s.repos[key]; ok {
		return nil, errExists
	}
	now := time.Now().UTC()
	r := &Repo{
		ID:            s.newID(),
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		CreatedAt:     now,
		UpdatedAt:     now,
		PushedAt:      now,
		Refs:          map[string]Oid{},
		issueByN:      map[int]*Issue{},
		pullByN:       map[int]*Pull{},
	}
	s.repos[key] = r
	s.repoOrder = append(s.repoOrder, r)
	s.byNodeID[r.ID] = r
	return r, nil
}

func (s *Store) deleteRepo(r *Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.repos, strings.ToLower(r.Name))
	delete(s.byNodeID, r.ID)
	for i, x := range s.repoOrder {
		if x == r {
			s.repoOrder = append(s.repoOrder[:i], s.repoOrder[i+1:]...)
			break
		}
	}
}

func (s *Store) listRepos() []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*Repo(nil), s.repoOrder...)
}

func (s *Store) byDatabaseID(id int64) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byNodeID[id]
}

// repoOfIssue finds the repository holding an issue or pull (for global-id lookups).
func (s *Store) repoOf(target any) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.repoOrder {
		switch t := target.(type) {
		case *Issue:
			if r.issueByN[t.Number] == t {
				return r
			}
		case *Pull:
			if r.pullByN[t.Number] == t {
				return r
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// refs

func (s *Store) ref(r *Repo, name string) (Oid, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := r.Refs[name]
	return o, ok
}

func (s *Store) refsSnapshot(r *Repo) (map[string]Oid, string, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Oid, len(r.Refs))
	for k, v := range r.Refs {
		out[k] = v
	}
	return out, r.DefaultBranch, r.RefsGen
}

func sortedRefNames(refs map[string]Oid) []string {
	names := make([]string, 0, len(refs))
	for k := range refs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (s *Store) hasBranches(r *Repo) bool {
	for k := range r.Refs {
		if strings.HasPrefix(k, "refs/heads/") {
			return true
		}
	}
	return false
}

// setRef moves (or with zero deletes) a ref under the store lock; caller holds s.mu.
func (s *Store) setRefLocked(r *Repo, name string, o Oid) {
	if strings.HasPrefix(name, "refs/heads/") && !s.hasBranches(r) && !o.IsZero() {
		// The first branch of an empty repository becomes its default unless the default is being made.
		if _, ok := r.Refs["refs/heads/"+r.DefaultBranch]; !ok && name != "refs/heads/"+r.DefaultBranch {
			r.DefaultBranch = strings.TrimPrefix(name, "refs/heads/")
		}
	}
	if o.IsZero() {
		delete(r.Refs, name)
	} else {
		r.Refs[name] = o
	}
	r.RefsGen++
	r.PushedAt = time.Now().UTC()
}

// resolveRev answers the commit a branch name, full ref or commit id names ("" = the default branch).
func (s *Store) resolveRev(r *Repo, rev string) (Oid, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rev == "" {
		rev = r.DefaultBranch
	}
	if o, ok := r.Refs["refs/heads/"+rev]; ok {
		return o, true
	}
	if o, ok := r.Refs["refs/tags/"+rev]; ok {
		return s.db.peel(o), true
	}
	if o, ok := r.Refs[rev]; ok {
		return s.db.peel(o), true
	}
	if o, ok := parseOid(strings.ToLower(rev)); ok && s.db.Has(o) {
		return o, true
	}
	return zeroOid, false
}

// ---------------------------------------------------------------------------------------------
// commits made by the forge itself (contents API, merges)

func (s *Store) ident() string {
	v := s.Viewer()
	return v + " <" + v + "@users.noreply.example>"
}

// identLocked is ident for a caller that already holds s.mu.
func (s *Store) identLocked() string {
	v := s.viewer
	if v == "" {
		v = "viewer"
	}
	return v + " <" + v + "@users.noreply.example>"
}

var errStale = errors.New("stale sha")
var errNoBranch = errors.New("branch not found")
var errMissingSha = errors.New("sha wasn't supplied")
var errNotFound = errors.New("not found")

type ContentResult struct {
	Commit  Oid
	BlobSha Oid
	Created bool
}

// putFile commits one file on a branch. A missing branch starts from the default branch (or a root commit
// in an empty repository).
func (s *Store) putFile(r *Repo, branch, path string, content []byte, message, sha string) (ContentResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if branch == "" {
		branch = r.DefaultBranch
	}
	refName := "refs/heads/" + branch
	parent, ok := r.Refs[refName]
	if !ok {
		if base, has := r.Refs["refs/heads/"+r.DefaultBranch]; has {
			parent = base
		}
	}
	files := map[string]FileEntry{}
	if !parent.IsZero() {
		c := s.db.commit(parent)
		if c == nil {
			return ContentResult{}, errNotFound
		}
		if err := s.db.flattenTree(c.Tree, "", files); err != nil {
			return ContentResult{}, err
		}
	}
	path = strings.Trim(path, "/")
	existing, exists := files[path]
	if exists {
		if sha == "" {
			return ContentResult{}, errMissingSha
		}
		if !strings.EqualFold(sha, existing.Oid.String()) {
			return ContentResult{}, errStale
		}
	}
	// A path that is a directory (some file lives under it) cannot become a file.
	for p := range files {
		if strings.HasPrefix(p, path+"/") {
			return ContentResult{}, errStale
		}
	}
	blob := s.db.Put(objBlob, content)
	files[path] = FileEntry{Mode: "100644", Oid: blob}
	tree := s.db.writeFlatTree(files)
	var parents []Oid
	if !parent.IsZero() {
		parents = []Oid{parent}
	}
	commit := s.db.Put(objCommit, buildCommit(tree, parents, s.identLocked(), time.Now().Unix(), message))
	s.setRefLocked(r, refName, commit)
	return ContentResult{Commit: commit, BlobSha: blob, Created: !exists}, nil
}

func (s *Store) deleteFile(r *Repo, branch, path, message, sha string) (Oid, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if branch == "" {
		branch = r.DefaultBranch
	}
	refName := "refs/heads/" + branch
	parent, ok := r.Refs[refName]
	if !ok {
		return zeroOid, errNoBranch
	}
	c := s.db.commit(parent)
	if c == nil {
		return zeroOid, errNotFound
	}
	files := map[string]FileEntry{}
	if err := s.db.flattenTree(c.Tree, "", files); err != nil {
		return zeroOid, err
	}
	path = strings.Trim(path, "/")
	existing, exists := files[path]
	if !exists {
		return zeroOid, errNotFound
	}
	if sha == "" {
		return zeroOid, errMissingSha
	}
	if !strings.EqualFold(sha, existing.Oid.String()) {
		return zeroOid, errStale
	}
	delete(files, path)
	tree := s.db.writeFlatTree(files)
	commit := s.db.Put(objCommit, buildCommit(tree, []Oid{parent}, s.identLocked(), time.Now().Unix(), message))
	s.setRefLocked(r, refName, commit)
	return commit, nil
}

// ---------------------------------------------------------------------------------------------
// labels, issues, comments

func (s *Store) findLabel(r *Repo, name string) *Label {
	for _, l := range r.Labels {
		if strings.EqualFold(l.Name, name) {
			return l
		}
	}
	return nil
}

func (s *Store) createLabel(r *Repo, name, color, description string) (*Label, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findLabel(r, name) != nil {
		return nil, errExists
	}
	if color == "" {
		color = "ededed"
	}
	l := &Label{ID: s.newID(), Name: name, Color: strings.TrimPrefix(color, "#"), Description: description}
	r.Labels = append(r.Labels, l)
	return l, nil
}

var errUnknownLabel = errors.New("unknown label")

func (s *Store) createIssue(r *Repo, title, body string, labels []string) (*Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ls []*Label
	for _, name := range labels {
		l := s.findLabel(r, name)
		if l == nil {
			return nil, errUnknownLabel
		}
		ls = append(ls, l)
	}
	now := time.Now().UTC()
	r.nextIssue++
	s.issueSeq++
	is := &Issue{ID: s.newID(), Number: r.nextIssue, Title: title, Body: body, State: "open", Labels: ls, CreatedAt: now, UpdatedAt: now, seq: s.issueSeq}
	r.Issues = append(r.Issues, is)
	r.issueByN[is.Number] = is
	s.byNodeID[is.ID] = is
	return is, nil
}

func (s *Store) issue(r *Repo, n int) *Issue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return r.issueByN[n]
}

func (s *Store) setIssueState(is *Issue, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if state == "closed" && is.State != "closed" {
		is.ClosedAt = &now
	}
	if state == "open" {
		is.ClosedAt = nil
	}
	is.State = state
	is.UpdatedAt = now
}

func (s *Store) addComment(list *[]*Comment, body string) *Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &Comment{ID: s.newID(), Body: body, CreatedAt: time.Now().UTC()}
	*list = append(*list, c)
	s.byNodeID[c.ID] = c
	return c
}

// ---------------------------------------------------------------------------------------------
// pull requests

var errNoHead = errors.New("head branch not found")
var errNoBase = errors.New("base branch not found")
var errDuplicatePull = errors.New("a pull request already exists")

func (s *Store) createPull(r *Repo, title, body, head, base string) (*Pull, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := strings.IndexByte(head, ':'); i >= 0 {
		head = head[i+1:]
	}
	if base == "" {
		base = r.DefaultBranch
	}
	headSha, ok := r.Refs["refs/heads/"+head]
	if !ok {
		return nil, errNoHead
	}
	baseSha, ok := r.Refs["refs/heads/"+base]
	if !ok {
		return nil, errNoBase
	}
	for _, p := range r.Pulls {
		if p.State == "open" && p.Head == head && p.Base == base {
			return nil, errDuplicatePull
		}
	}
	now := time.Now().UTC()
	r.nextPull++
	p := &Pull{ID: s.newID(), Number: r.nextPull, Title: title, Body: body, State: "open", Head: head, Base: base,
		HeadSha: headSha, BaseSha: baseSha, CreatedAt: now, UpdatedAt: now}
	r.Pulls = append(r.Pulls, p)
	r.pullByN[p.Number] = p
	s.byNodeID[p.ID] = p
	return p, nil
}

func (s *Store) pull(r *Repo, n int) *Pull {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return r.pullByN[n]
}

// pullShas answers a pull's live head and base tips (frozen once it is closed).
func (s *Store) pullShas(r *Repo, p *Pull) (Oid, Oid) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p.State != "open" {
		return p.HeadSha, p.BaseSha
	}
	head, base := p.HeadSha, p.BaseSha
	if o, ok := r.Refs["refs/heads/"+p.Head]; ok {
		head = o
	}
	if o, ok := r.Refs["refs/heads/"+p.Base]; ok {
		base = o
	}
	return head, base
}

type FileChange struct {
	Path      string
	Status    string // added | modified | removed
	Additions int
	Deletions int
	OldOid    Oid
	NewOid    Oid
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func splitLines(b []byte) []string {
	s := string(b)
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineDiff counts added and deleted lines by a longest-common-subsequence over lines (bounded size).
func lineDiff(a, b []byte) (int, int) {
	la, lb := splitLines(a), splitLines(b)
	if len(la)*len(lb) > 4_000_000 {
		return len(lb), len(la)
	}
	prev := make([]int, len(lb)+1)
	cur := make([]int, len(lb)+1)
	for i := 1; i <= len(la); i++ {
		for j := 1; j <= len(lb); j++ {
			if la[i-1] == lb[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] >= cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	common := prev[len(lb)]
	return len(lb) - common, len(la) - common
}

// pullFiles compares the head tree with the merge base's tree.
func (s *Store) pullFiles(r *Repo, p *Pull) []FileChange {
	head, base := s.pullShas(r, p)
	from := base
	if p.State == "open" || !p.Merged {
		if mb := s.db.mergeBase(base, head); !mb.IsZero() {
			from = mb
		}
	} else if mb := s.db.mergeBase(p.BaseSha, p.HeadSha); !mb.IsZero() {
		from = mb
	}
	oldFiles, newFiles := map[string]FileEntry{}, map[string]FileEntry{}
	if c := s.db.commit(from); c != nil {
		s.db.flattenTree(c.Tree, "", oldFiles)
	}
	if c := s.db.commit(head); c != nil {
		s.db.flattenTree(c.Tree, "", newFiles)
	}
	var out []FileChange
	for path, nf := range newFiles {
		of, ok := oldFiles[path]
		if ok && of.Oid == nf.Oid {
			continue
		}
		newData := s.blobData(nf.Oid)
		if !ok {
			out = append(out, FileChange{Path: path, Status: "added", Additions: countLines(newData), NewOid: nf.Oid})
			continue
		}
		add, del := lineDiff(s.blobData(of.Oid), newData)
		out = append(out, FileChange{Path: path, Status: "modified", Additions: add, Deletions: del, OldOid: of.Oid, NewOid: nf.Oid})
	}
	for path, of := range oldFiles {
		if _, ok := newFiles[path]; !ok {
			out = append(out, FileChange{Path: path, Status: "removed", Deletions: countLines(s.blobData(of.Oid)), OldOid: of.Oid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (s *Store) blobData(o Oid) []byte {
	if obj := s.db.Get(o); obj != nil {
		return obj.Data
	}
	return nil
}

var errNotMergeable = errors.New("Pull Request is not mergeable")

// threeWay merges head into base over the merge base, file by file; ok is false on a conflict.
func (s *Store) threeWay(base, head Oid) (map[string]FileEntry, bool) {
	mb := s.db.mergeBase(base, head)
	ancestor, ours, theirs := map[string]FileEntry{}, map[string]FileEntry{}, map[string]FileEntry{}
	if c := s.db.commit(mb); c != nil {
		s.db.flattenTree(c.Tree, "", ancestor)
	}
	if c := s.db.commit(base); c != nil {
		s.db.flattenTree(c.Tree, "", ours)
	}
	if c := s.db.commit(head); c != nil {
		s.db.flattenTree(c.Tree, "", theirs)
	}
	paths := map[string]bool{}
	for p := range ancestor {
		paths[p] = true
	}
	for p := range ours {
		paths[p] = true
	}
	for p := range theirs {
		paths[p] = true
	}
	out := map[string]FileEntry{}
	for p := range paths {
		a, inA := ancestor[p]
		o, inO := ours[p]
		t, inT := theirs[p]
		same := func(x FileEntry, inX bool, y FileEntry, inY bool) bool {
			return inX == inY && (!inX || x.Oid == y.Oid)
		}
		switch {
		case same(o, inO, t, inT):
			if inO {
				out[p] = o
			}
		case same(a, inA, o, inO):
			if inT {
				out[p] = t
			}
		case same(a, inA, t, inT):
			if inO {
				out[p] = o
			}
		default:
			return nil, false
		}
	}
	return out, true
}

// mergeable answers MERGEABLE or CONFLICTING for an open pull.
func (s *Store) mergeable(r *Repo, p *Pull) bool {
	head, base := s.pullShas(r, p)
	if s.db.isAncestor(head, base) {
		return true
	}
	_, ok := s.threeWay(base, head)
	return ok
}

// mergePull squash-merges (or merge/rebase, written the same way: one commit on the base) a pull.
func (s *Store) mergePull(r *Repo, p *Pull, title, message string) (Oid, error) {
	head, base := s.pullShas(r, p)
	if p.State != "open" {
		return zeroOid, errNotMergeable
	}
	files, ok := s.threeWay(base, head)
	if !ok {
		return zeroOid, errNotMergeable
	}
	if title == "" {
		title = p.Title + " (#" + itoa(p.Number) + ")"
	}
	msg := title
	if message != "" {
		msg += "\n\n" + message
	}
	tree := s.db.writeFlatTree(files)
	commit := s.db.Put(objCommit, buildCommit(tree, []Oid{base}, s.ident(), time.Now().Unix(), msg))
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := r.Refs["refs/heads/"+p.Base]; cur != base || p.State != "open" {
		return zeroOid, errNotMergeable
	}
	s.setRefLocked(r, "refs/heads/"+p.Base, commit)
	now := time.Now().UTC()
	p.State, p.Merged, p.MergedAt, p.ClosedAt, p.UpdatedAt = "closed", true, &now, &now, now
	p.HeadSha, p.BaseSha, p.MergeCommitSha = head, base, commit
	return commit, nil
}

func (s *Store) setPullState(r *Repo, p *Pull, state string) {
	head, base := s.pullShas(r, p)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if state == "closed" && p.State == "open" {
		p.HeadSha, p.BaseSha, p.ClosedAt = head, base, &now
	}
	if state == "open" {
		p.ClosedAt = nil
	}
	p.State, p.UpdatedAt = state, now
}
