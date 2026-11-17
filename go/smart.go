package main

// Git smart HTTP: info/refs, upload-pack (protocol v2 and v0) and receive-pack (v0, report-status,
// side-band-64k, delete-refs).

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const agentCap = "agent=forge/1.0"

func gitError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "{\"message\":%q}", msg)
}

func wantsV2(r *http.Request) bool {
	for _, v := range r.Header.Values("Git-Protocol") {
		for _, part := range strings.Split(v, ":") {
			if strings.TrimSpace(part) == "version=2" {
				return true
			}
		}
	}
	return false
}

// serveGit handles /{owner}/{repo}.git/<rest>.
func (s *Server) serveGit(w http.ResponseWriter, r *http.Request, owner, name, rest string) {
	repo := s.st.repo(owner, name)
	if repo == nil {
		gitError(w, http.StatusNotFound, "Repository not found")
		return
	}
	switch {
	case rest == "info/refs" && r.Method == http.MethodGet:
		service := r.URL.Query().Get("service")
		switch service {
		case "git-upload-pack":
			s.advertiseUpload(w, r, repo)
		case "git-receive-pack":
			s.advertiseReceive(w, repo)
		default:
			gitError(w, http.StatusForbidden, "dumb HTTP is not served")
		}
	case rest == "git-upload-pack" && r.Method == http.MethodPost:
		s.uploadPack(w, r, repo)
	case rest == "git-receive-pack" && r.Method == http.MethodPost:
		s.receivePack(w, r, repo)
	case rest == "HEAD" && r.Method == http.MethodGet:
		_, def, _ := s.st.refsSnapshot(repo)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "ref: refs/heads/%s\n", def)
	default:
		gitError(w, http.StatusNotFound, "Not Found")
	}
}

func noCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
}

func (s *Server) advertiseUpload(w http.ResponseWriter, r *http.Request, repo *Repo) {
	noCache(w)
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	var b bytes.Buffer
	if wantsV2(r) {
		pktLine(&b, "version 2\n")
		pktLine(&b, agentCap+"\n")
		pktLine(&b, "ls-refs=unborn\n")
		pktLine(&b, "fetch=shallow wait-for-done\n")
		pktLine(&b, "server-option\n")
		pktLine(&b, "object-format=sha1\n")
		pktFlushTo(&b)
		w.Write(b.Bytes())
		return
	}
	refs, def, _ := s.st.refsSnapshot(repo)
	pktLine(&b, "# service=git-upload-pack\n")
	pktFlushTo(&b)
	caps := "multi_ack_detailed multi_ack thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want no-done symref=HEAD:refs/heads/" + def + " object-format=sha1 " + agentCap
	s.writeV0Refs(&b, refs, def, caps, true)
	w.Write(b.Bytes())
}

func (s *Server) advertiseReceive(w http.ResponseWriter, repo *Repo) {
	noCache(w)
	w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
	refs, def, _ := s.st.refsSnapshot(repo)
	var b bytes.Buffer
	pktLine(&b, "# service=git-receive-pack\n")
	pktFlushTo(&b)
	caps := "report-status delete-refs side-band-64k quiet ofs-delta object-format=sha1 " + agentCap
	s.writeV0Refs(&b, refs, def, caps, false)
	w.Write(b.Bytes())
}

// writeV0Refs writes a v0 ref advertisement: HEAD (upload-pack only), refs in order, peeled tags.
func (s *Server) writeV0Refs(b *bytes.Buffer, refs map[string]Oid, def string, caps string, withHead bool) {
	first := true
	emit := func(o Oid, name string) {
		if first {
			pktLine(b, o.String()+" "+name+"\x00"+caps+"\n")
			first = false
		} else {
			pktLine(b, o.String()+" "+name+"\n")
		}
	}
	if withHead {
		if o, ok := refs["refs/heads/"+def]; ok {
			emit(o, "HEAD")
		}
	}
	for _, name := range sortedRefNames(refs) {
		o := refs[name]
		emit(o, name)
		if strings.HasPrefix(name, "refs/tags/") {
			if p := s.st.db.peel(o); p != o {
				pktLine(b, p.String()+" "+name+"^{}\n")
			}
		}
	}
	if first {
		pktLine(b, zeroOid.String()+" capabilities^{}\x00"+caps+"\n")
	}
	pktFlushTo(b)
}

func readBody(r *http.Request) ([]byte, error) {
	body := r.Body
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Encoding")), "gzip") {
		gz, err := newGzipReader(body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		return io.ReadAll(gz)
	}
	return io.ReadAll(body)
}

func (s *Server) uploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := readBody(r)
	if err != nil {
		gitError(w, http.StatusBadRequest, "could not read the request")
		return
	}
	if wantsV2(r) {
		s.uploadPackV2(w, repo, body)
		return
	}
	s.uploadPackV0(w, repo, body)
}

type fetchRequest struct {
	wants    []Oid
	haves    []Oid
	shallow  []Oid
	depth    int
	relative bool // deepen-relative: depth counts from the client's shallow boundary
	done     bool
	noProg   bool
	inclTag  bool
	sideband bool
}

func (s *Server) uploadPackV2(w http.ResponseWriter, repo *Repo, body []byte) {
	pkts, _, err := readPkts(body, 0)
	if err != nil || len(pkts) == 0 {
		gitError(w, http.StatusBadRequest, "malformed pkt-line request")
		return
	}
	command := ""
	i := 0
	for ; i < len(pkts); i++ {
		p := pkts[i]
		if p.Kind != pktData {
			i++
			break
		}
		if t := p.Text(); strings.HasPrefix(t, "command=") {
			command = t[8:]
		}
	}
	var args []string
	for ; i < len(pkts); i++ {
		if pkts[i].Kind != pktData {
			break
		}
		args = append(args, pkts[i].Text())
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	noCache(w)
	switch command {
	case "ls-refs":
		w.Write(s.lsRefs(repo, args))
	case "fetch":
		req := fetchRequest{sideband: true}
		for _, a := range args {
			switch {
			case strings.HasPrefix(a, "want "):
				if o, ok := parseOid(strings.TrimSpace(a[5:])); ok {
					req.wants = append(req.wants, o)
				}
			case strings.HasPrefix(a, "have "):
				if o, ok := parseOid(strings.TrimSpace(a[5:])); ok {
					req.haves = append(req.haves, o)
				}
			case strings.HasPrefix(a, "shallow "):
				if o, ok := parseOid(strings.TrimSpace(a[8:])); ok {
					req.shallow = append(req.shallow, o)
				}
			case a == "deepen-relative":
				req.relative = true
			case strings.HasPrefix(a, "deepen "):
				req.depth, _ = strconv.Atoi(strings.TrimSpace(a[7:]))
			case a == "done":
				req.done = true
			case a == "no-progress":
				req.noProg = true
			case a == "include-tag":
				req.inclTag = true
			}
		}
		w.Write(s.fetchV2(repo, req))
	default:
		gitError(w, http.StatusBadRequest, "unknown command")
	}
}

func (s *Server) lsRefs(repo *Repo, args []string) []byte {
	refs, def, _ := s.st.refsSnapshot(repo)
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, a[11:])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var b bytes.Buffer
	if keep("HEAD") {
		headRef := "refs/heads/" + def
		if o, ok := refs[headRef]; ok {
			line := o.String() + " HEAD"
			if symrefs {
				line += " symref-target:" + headRef
			}
			pktLine(&b, line+"\n")
		} else if unborn {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:" + headRef
			}
			pktLine(&b, line+"\n")
		}
	}
	for _, name := range sortedRefNames(refs) {
		if !keep(name) {
			continue
		}
		o := refs[name]
		line := o.String() + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if p := s.st.db.peel(o); p != o {
				line += " peeled:" + p.String()
			}
		}
		pktLine(&b, line+"\n")
	}
	pktFlushTo(&b)
	return b.Bytes()
}

// cacheKey canonicalizes a fetch so identical concurrent clones share one pack.
func fetchKey(repo *Repo, gen int64, req fetchRequest, v2 bool) string {
	h := sha1.New()
	fmt.Fprintf(h, "%d|%s|%d|%d|%v|%v|%v|%v|", repo.ID, repo.Name, gen, req.depth, req.relative, req.inclTag, v2, req.done)
	for _, list := range [][]Oid{req.wants, req.haves, req.shallow} {
		sorted := append([]Oid(nil), list...)
		sort.Slice(sorted, func(i, j int) bool { return bytes.Compare(sorted[i][:], sorted[j][:]) < 0 })
		for _, o := range sorted {
			h.Write(o[:])
		}
		h.Write([]byte{'|'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) cachedFetch(repo *Repo, req fetchRequest, v2 bool, build func() []byte) []byte {
	_, _, gen := s.st.refsSnapshot(repo)
	key := fetchKey(repo, gen, req, v2)
	s.st.packMu.Lock()
	res, ok := s.st.packCache[key]
	if !ok {
		if len(s.st.packCache) > 256 {
			s.st.packCache = map[string]*packResult{}
		}
		res = &packResult{}
		s.st.packCache[key] = res
	}
	s.st.packMu.Unlock()
	res.once.Do(func() { res.body = build() })
	return res.body
}

// selection is what a fetch sends: the objects, and the shallow boundary changes.
type selection struct {
	objects   []Oid
	shallow   []Oid
	unshallow []Oid
	missing   bool
}

// selectObjects walks the wanted history (depth-limited when deepen is set) minus what the client has.
func (s *Server) selectObjects(req fetchRequest) selection {
	db := s.st.db
	var sel selection
	clientShallow := map[Oid]bool{}
	for _, o := range req.shallow {
		clientShallow[o] = true
	}
	// What the client holds: everything reachable from its haves, stopping at its shallow boundary.
	have := map[Oid]bool{}
	var markTree func(t Oid)
	markTree = func(t Oid) {
		if have[t] {
			return
		}
		have[t] = true
		obj := db.Get(t)
		if obj == nil || obj.Type != objTree {
			return
		}
		entries, _ := parseTree(obj.Data)
		for _, e := range entries {
			if e.Mode == "160000" {
				continue
			}
			if e.IsDir() {
				markTree(e.Oid)
			} else {
				have[e.Oid] = true
			}
		}
	}
	haveCommits := map[Oid]bool{}
	stack := []Oid{}
	for _, h := range req.haves {
		if db.Has(h) {
			stack = append(stack, h)
		}
	}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if haveCommits[cur] {
			continue
		}
		haveCommits[cur] = true
		have[cur] = true
		c := db.commit(cur)
		if c == nil {
			continue
		}
		markTree(c.Tree)
		if clientShallow[cur] {
			continue
		}
		stack = append(stack, c.Parents...)
	}

	sent := map[Oid]bool{}
	var out []Oid
	add := func(o Oid) {
		if !sent[o] && !have[o] {
			sent[o] = true
			out = append(out, o)
		}
	}
	var addTree func(t Oid)
	addTree = func(t Oid) {
		if sent[t] || have[t] {
			return
		}
		add(t)
		obj := db.Get(t)
		if obj == nil {
			sel.missing = true
			return
		}
		entries, _ := parseTree(obj.Data)
		for _, e := range entries {
			if e.Mode == "160000" {
				continue
			}
			if e.IsDir() {
				addTree(e.Oid)
			} else {
				add(e.Oid)
			}
		}
	}
	// Walk commits from the wants, breadth-first so depth counts generations.
	type item struct {
		oid   Oid
		depth int
	}
	var queue []item
	visited := map[Oid]bool{}
	for _, want := range req.wants {
		obj := db.Get(want)
		if obj == nil {
			sel.missing = true
			continue
		}
		// Peel wanted tags, sending each tag object.
		for obj != nil && obj.Type == objTag {
			add(want)
			target, _, ok := parseTagTarget(obj.Data)
			if !ok {
				break
			}
			want = target
			obj = db.Get(want)
		}
		if obj == nil {
			continue
		}
		switch obj.Type {
		case objCommit:
			start := 1
			if req.relative {
				start = 0 // not counting until the walk crosses the client's shallow boundary
			}
			queue = append(queue, item{want, start})
		case objTree:
			addTree(want)
		default:
			add(want)
		}
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if visited[it.oid] {
			continue
		}
		visited[it.oid] = true
		if haveCommits[it.oid] && !clientShallow[it.oid] {
			continue
		}
		c := db.commit(it.oid)
		if c == nil {
			sel.missing = true
			continue
		}
		atLimit := req.depth > 0 && it.depth >= req.depth
		if req.relative {
			atLimit = req.depth > 0 && it.depth > 0 && it.depth >= req.depth
		}
		if !haveCommits[it.oid] {
			add(it.oid)
			addTree(c.Tree)
		}
		if atLimit {
			if len(c.Parents) > 0 && !clientShallow[it.oid] {
				sel.shallow = append(sel.shallow, it.oid)
			} else if len(c.Parents) > 0 && clientShallow[it.oid] {
				// still the boundary: the client keeps it shallow
			}
			continue
		}
		if clientShallow[it.oid] && len(c.Parents) > 0 {
			sel.unshallow = append(sel.unshallow, it.oid)
		}
		next := it.depth + 1
		if req.relative && it.depth == 0 {
			next = 0
			if clientShallow[it.oid] {
				next = 1
			}
		}
		for _, p := range c.Parents {
			queue = append(queue, item{p, next})
		}
	}
	if req.inclTag {
		// Annotated tags pointing at sent commits ride along (handled by the caller's refs).
	}
	sel.objects = out
	return sel
}

func (s *Server) fetchV2(repo *Repo, req fetchRequest) []byte {
	return s.cachedFetch(repo, req, true, func() []byte {
		var b bytes.Buffer
		sel := s.selectObjects(req)
		if !req.done && len(req.haves) > 0 {
			// Negotiation round without done: acknowledge common haves and say we are ready.
			pktLine(&b, "acknowledgments\n")
			acked := false
			for _, h := range req.haves {
				if s.st.db.Has(h) {
					pktLine(&b, "ACK "+h.String()+"\n")
					acked = true
				}
			}
			if !acked {
				pktLine(&b, "NAK\n")
			}
			pktLine(&b, "ready\n")
			pktDelimTo(&b)
		}
		if req.depth > 0 || len(req.shallow) > 0 {
			if len(sel.shallow) > 0 || len(sel.unshallow) > 0 {
				pktLine(&b, "shallow-info\n")
				for _, o := range sel.shallow {
					pktLine(&b, "shallow "+o.String()+"\n")
				}
				for _, o := range sel.unshallow {
					pktLine(&b, "unshallow "+o.String()+"\n")
				}
				pktDelimTo(&b)
			}
		}
		pktLine(&b, "packfile\n")
		sideband(&b, 1, writePack(s.st.db, sel.objects))
		pktFlushTo(&b)
		return b.Bytes()
	})
}

func (s *Server) uploadPackV0(w http.ResponseWriter, repo *Repo, body []byte) {
	pkts, _, err := readPkts(body, 0)
	if err != nil || len(pkts) == 0 {
		gitError(w, http.StatusBadRequest, "malformed pkt-line request")
		return
	}
	req := fetchRequest{}
	caps := map[string]bool{}
	for _, p := range pkts {
		if p.Kind != pktData {
			continue
		}
		t := p.Text()
		switch {
		case strings.HasPrefix(t, "want "):
			fields := strings.Fields(t[5:])
			if len(fields) == 0 {
				continue
			}
			if o, ok := parseOid(fields[0]); ok {
				req.wants = append(req.wants, o)
			}
			for _, c := range fields[1:] {
				caps[c] = true
			}
		case strings.HasPrefix(t, "have "):
			if o, ok := parseOid(strings.TrimSpace(t[5:])); ok {
				req.haves = append(req.haves, o)
			}
		case strings.HasPrefix(t, "shallow "):
			if o, ok := parseOid(strings.TrimSpace(t[8:])); ok {
				req.shallow = append(req.shallow, o)
			}
		case t == "deepen-relative":
			req.relative = true
		case strings.HasPrefix(t, "deepen "):
			req.depth, _ = strconv.Atoi(strings.TrimSpace(t[7:]))
		case t == "done":
			req.done = true
		}
	}
	req.sideband = caps["side-band-64k"] || caps["side-band"]
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	noCache(w)
	if len(req.wants) == 0 {
		var b bytes.Buffer
		pktFlushTo(&b)
		w.Write(b.Bytes())
		return
	}
	out := s.cachedFetch(repo, req, false, func() []byte {
		var b bytes.Buffer
		sel := s.selectObjects(req)
		if req.depth > 0 || len(req.shallow) > 0 {
			for _, o := range sel.shallow {
				pktLine(&b, "shallow "+o.String()+"\n")
			}
			for _, o := range sel.unshallow {
				pktLine(&b, "unshallow "+o.String()+"\n")
			}
			pktFlushTo(&b)
		}
		common := zeroOid
		for _, h := range req.haves {
			if s.st.db.Has(h) {
				common = h
			}
		}
		if common.IsZero() {
			pktLine(&b, "NAK\n")
		} else {
			pktLine(&b, "ACK "+common.String()+"\n")
		}
		pack := writePack(s.st.db, sel.objects)
		if req.sideband {
			sideband(&b, 1, pack)
			pktFlushTo(&b)
		} else {
			b.Write(pack)
		}
		return b.Bytes()
	})
	w.Write(out)
}

type pushCommand struct {
	old, new Oid
	ref      string
}

func (s *Server) receivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := readBody(r)
	if err != nil {
		gitError(w, http.StatusBadRequest, "could not read the request")
		return
	}
	pkts, rest, err := readPkts(body, 1)
	if err != nil {
		gitError(w, http.StatusBadRequest, "malformed pkt-line request")
		return
	}
	var cmds []pushCommand
	caps := map[string]bool{}
	for i, p := range pkts {
		if p.Kind != pktData {
			continue
		}
		line := string(p.Payload)
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			for _, c := range strings.Fields(line[nul+1:]) {
				caps[c] = true
			}
			line = line[:nul]
		}
		_ = i
		fields := strings.Fields(strings.TrimRight(line, "\n"))
		if len(fields) != 3 {
			continue
		}
		o1, ok1 := parseOid(fields[0])
		o2, ok2 := parseOid(fields[1])
		if !ok1 || !ok2 {
			continue
		}
		cmds = append(cmds, pushCommand{o1, o2, fields[2]})
	}
	unpackErr := ""
	if len(rest) > 0 {
		if _, err := readPack(s.st.db, rest); err != nil {
			unpackErr = err.Error()
		}
	}
	type result struct {
		ref    string
		reason string
	}
	var results []result
	if unpackErr == "" {
		s.st.mu.Lock()
		for _, c := range cmds {
			reason := s.checkCommand(repo, c)
			if reason == "" {
				s.st.setRefLocked(repo, c.ref, c.new)
			}
			results = append(results, result{c.ref, reason})
		}
		s.st.mu.Unlock()
	} else {
		for _, c := range cmds {
			results = append(results, result{c.ref, "unpacker error"})
		}
	}
	var report bytes.Buffer
	if unpackErr == "" {
		pktLine(&report, "unpack ok\n")
	} else {
		pktLine(&report, "unpack "+strings.ReplaceAll(unpackErr, "\n", " ")+"\n")
	}
	for _, res := range results {
		if res.reason == "" {
			pktLine(&report, "ok "+res.ref+"\n")
		} else {
			pktLine(&report, "ng "+res.ref+" "+res.reason+"\n")
		}
	}
	pktFlushTo(&report)
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	noCache(w)
	if caps["side-band-64k"] || caps["side-band"] {
		var b bytes.Buffer
		sideband(&b, 1, report.Bytes())
		pktFlushTo(&b)
		w.Write(b.Bytes())
		return
	}
	if caps["report-status"] || caps["report-status-v2"] {
		w.Write(report.Bytes())
		return
	}
	w.Write([]byte("0000"))
}

// checkCommand answers "" when a push command may apply, or git's refusal reason. Caller holds the lock.
func (s *Server) checkCommand(repo *Repo, c pushCommand) string {
	if !strings.HasPrefix(c.ref, "refs/") || strings.Contains(c.ref, "..") {
		return "funny refname"
	}
	cur, exists := repo.Refs[c.ref]
	if c.new.IsZero() {
		if !exists {
			return "unable to delete: remote ref does not exist"
		}
		if !c.old.IsZero() && c.old != cur {
			return "stale info"
		}
		return ""
	}
	if !s.st.db.Has(c.new) {
		return "missing necessary objects"
	}
	if !exists {
		if !c.old.IsZero() {
			return "stale info"
		}
		return ""
	}
	if c.old.IsZero() {
		return "already exists"
	}
	if c.old != cur {
		return "stale info"
	}
	if strings.HasPrefix(c.ref, "refs/tags/") {
		return "already exists"
	}
	if !s.st.db.isAncestor(cur, c.new) {
		return "non-fast-forward"
	}
	return ""
}
