package main

// Git objects: hashing, an in-memory object database, and the parsing the forge needs to walk
// commits and trees. Everything is SHA-1, the only object format the suite speaks.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Oid [20]byte

var zeroOid Oid

func (o Oid) String() string { return hex.EncodeToString(o[:]) }
func (o Oid) IsZero() bool   { return o == zeroOid }

func parseOid(s string) (Oid, bool) {
	var o Oid
	if len(s) != 40 {
		return o, false
	}
	if _, err := hex.Decode(o[:], []byte(s)); err != nil {
		return o, false
	}
	return o, true
}

// Object types as packfiles number them.
const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
)

func typeName(t int) string {
	switch t {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return "unknown"
}

type Object struct {
	Type int
	Data []byte

	once     sync.Once
	deflated []byte // zlib of Data, built once for packs
}

// Deflated is the object's zlib stream, computed once and shared by every pack that carries it.
func (o *Object) Deflated() []byte {
	o.once.Do(func() {
		var buf bytes.Buffer
		w, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
		w.Write(o.Data)
		w.Close()
		o.deflated = buf.Bytes()
	})
	return o.deflated
}

func hashObject(t int, data []byte) Oid {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typeName(t), len(data))
	h.Write(data)
	var o Oid
	copy(o[:], h.Sum(nil))
	return o
}

// ODB is the forge-wide object database: objects are content-addressed, so every repository shares it.
type ODB struct {
	mu   sync.RWMutex
	objs map[Oid]*Object
}

func newODB() *ODB { return &ODB{objs: make(map[Oid]*Object, 4096)} }

func (db *ODB) Get(o Oid) *Object {
	db.mu.RLock()
	obj := db.objs[o]
	db.mu.RUnlock()
	return obj
}

func (db *ODB) Has(o Oid) bool { return db.Get(o) != nil }

func (db *ODB) Put(t int, data []byte) Oid {
	o := hashObject(t, data)
	db.mu.Lock()
	if _, ok := db.objs[o]; !ok {
		db.objs[o] = &Object{Type: t, Data: data}
	}
	db.mu.Unlock()
	return o
}

func (db *ODB) PutKnown(o Oid, t int, data []byte) {
	db.mu.Lock()
	if _, ok := db.objs[o]; !ok {
		db.objs[o] = &Object{Type: t, Data: data}
	}
	db.mu.Unlock()
}

// ---------------------------------------------------------------------------------------------
// commits

type Commit struct {
	Tree      Oid
	Parents   []Oid
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) (*Commit, error) {
	c := &Commit{}
	rest := data
	for {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return nil, errors.New("commit header is not terminated")
		}
		line := rest[:nl]
		rest = rest[nl+1:]
		if len(line) == 0 {
			c.Message = string(rest)
			break
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key, val := string(line[:sp]), string(line[sp+1:])
		switch key {
		case "tree":
			o, ok := parseOid(val)
			if !ok {
				return nil, errors.New("bad tree id")
			}
			c.Tree = o
		case "parent":
			o, ok := parseOid(val)
			if !ok {
				return nil, errors.New("bad parent id")
			}
			c.Parents = append(c.Parents, o)
		case "author":
			c.Author = val
		case "committer":
			c.Committer = val
		}
	}
	return c, nil
}

// commitTime reads the unix seconds out of an author/committer line.
func identTime(ident string) int64 {
	gt := strings.LastIndexByte(ident, '>')
	if gt < 0 {
		return 0
	}
	fields := strings.Fields(ident[gt+1:])
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(fields[0], 10, 64)
	return n
}

func identNameEmail(ident string) (string, string) {
	lt := strings.IndexByte(ident, '<')
	gt := strings.IndexByte(ident, '>')
	if lt < 0 || gt < lt {
		return strings.TrimSpace(ident), ""
	}
	return strings.TrimSpace(ident[:lt]), ident[lt+1 : gt]
}

func buildCommit(tree Oid, parents []Oid, ident string, when int64, message string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s %d +0000\n", ident, when)
	fmt.Fprintf(&b, "committer %s %d +0000\n\n", ident, when)
	b.WriteString(message)
	if !strings.HasSuffix(message, "\n") {
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// ---------------------------------------------------------------------------------------------
// tags

func parseTagTarget(data []byte) (Oid, int, bool) {
	var target Oid
	t := 0
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "object ") {
			if o, ok := parseOid(line[7:]); ok {
				target, found = o, true
			}
		} else if strings.HasPrefix(line, "type ") {
			switch line[5:] {
			case "commit":
				t = objCommit
			case "tree":
				t = objTree
			case "blob":
				t = objBlob
			case "tag":
				t = objTag
			}
		}
	}
	return target, t, found
}

// ---------------------------------------------------------------------------------------------
// trees

type TreeEntry struct {
	Mode string
	Name string
	Oid  Oid
}

func (e TreeEntry) IsDir() bool { return e.Mode == "40000" || e.Mode == "040000" }

func parseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return nil, errors.New("tree entry has no mode")
		}
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		nameEnd := sp + 1 + nul
		if len(data) < nameEnd+21 {
			return nil, errors.New("tree entry is truncated")
		}
		var o Oid
		copy(o[:], data[nameEnd+1:nameEnd+21])
		out = append(out, TreeEntry{Mode: string(data[:sp]), Name: string(data[sp+1 : nameEnd]), Oid: o})
		data = data[nameEnd+21:]
	}
	return out, nil
}

// buildTree writes entries in git's canonical order (directories sort as if their name had a '/').
func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	key := func(e TreeEntry) string {
		if e.IsDir() {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(sorted, func(i, j int) bool { return key(sorted[i]) < key(sorted[j]) })
	var b bytes.Buffer
	for _, e := range sorted {
		mode := e.Mode
		if mode == "040000" {
			mode = "40000"
		}
		b.WriteString(mode)
		b.WriteByte(' ')
		b.WriteString(e.Name)
		b.WriteByte(0)
		b.Write(e.Oid[:])
	}
	return b.Bytes()
}

// FileEntry is one path of a flattened tree.
type FileEntry struct {
	Mode string
	Oid  Oid
}

// flattenTree maps every file path under a tree to its mode and blob id.
func (db *ODB) flattenTree(tree Oid, prefix string, out map[string]FileEntry) error {
	obj := db.Get(tree)
	if obj == nil || obj.Type != objTree {
		return fmt.Errorf("tree %s is missing", tree)
	}
	entries, err := parseTree(obj.Data)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := prefix + e.Name
		if e.IsDir() {
			if err := db.flattenTree(e.Oid, p+"/", out); err != nil {
				return err
			}
		} else {
			out[p] = FileEntry{Mode: e.Mode, Oid: e.Oid}
		}
	}
	return nil
}

// writeFlatTree builds the nested trees for a flat path map and answers the root tree id.
func (db *ODB) writeFlatTree(files map[string]FileEntry) Oid {
	type node struct {
		files map[string]FileEntry
		dirs  map[string]*node
	}
	newNode := func() *node { return &node{files: map[string]FileEntry{}, dirs: map[string]*node{}} }
	root := newNode()
	for p, f := range files {
		parts := strings.Split(p, "/")
		n := root
		for _, d := range parts[:len(parts)-1] {
			child, ok := n.dirs[d]
			if !ok {
				child = newNode()
				n.dirs[d] = child
			}
			n = child
		}
		n.files[parts[len(parts)-1]] = f
	}
	var write func(n *node) Oid
	write = func(n *node) Oid {
		var entries []TreeEntry
		for name, f := range n.files {
			entries = append(entries, TreeEntry{Mode: f.Mode, Name: name, Oid: f.Oid})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, Oid: write(child)})
		}
		return db.Put(objTree, buildTree(entries))
	}
	return write(root)
}

// lookupPath walks a tree to a path: the entry found, and whether it exists. The empty path is the root.
func (db *ODB) lookupPath(tree Oid, path string) (TreeEntry, bool) {
	path = strings.Trim(path, "/")
	if path == "" {
		return TreeEntry{Mode: "40000", Name: "", Oid: tree}, true
	}
	cur := tree
	parts := strings.Split(path, "/")
	for i, part := range parts {
		obj := db.Get(cur)
		if obj == nil || obj.Type != objTree {
			return TreeEntry{}, false
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return TreeEntry{}, false
		}
		found := false
		for _, e := range entries {
			if e.Name == part {
				if i == len(parts)-1 {
					return e, true
				}
				if !e.IsDir() {
					return TreeEntry{}, false
				}
				cur = e.Oid
				found = true
				break
			}
		}
		if !found {
			return TreeEntry{}, false
		}
	}
	return TreeEntry{}, false
}

func (db *ODB) commit(o Oid) *Commit {
	obj := db.Get(o)
	if obj == nil || obj.Type != objCommit {
		return nil
	}
	c, err := parseCommit(obj.Data)
	if err != nil {
		return nil
	}
	return c
}

// peel follows tag objects to the first non-tag object.
func (db *ODB) peel(o Oid) Oid {
	for i := 0; i < 16; i++ {
		obj := db.Get(o)
		if obj == nil || obj.Type != objTag {
			return o
		}
		target, _, ok := parseTagTarget(obj.Data)
		if !ok {
			return o
		}
		o = target
	}
	return o
}

// isAncestor reports whether a is b or one of b's ancestors.
func (db *ODB) isAncestor(a, b Oid) bool {
	if a == b {
		return true
	}
	seen := map[Oid]bool{b: true}
	queue := []Oid{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		c := db.commit(cur)
		if c == nil {
			continue
		}
		for _, p := range c.Parents {
			if p == a {
				return true
			}
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return false
}

// ancestors is every commit reachable from tip, tip included.
func (db *ODB) ancestors(tip Oid) map[Oid]bool {
	seen := map[Oid]bool{}
	stack := []Oid{tip}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if c := db.commit(cur); c != nil {
			stack = append(stack, c.Parents...)
		}
	}
	return seen
}

// mergeBase is a common ancestor of a and b, nearest to b (breadth-first), or zero.
func (db *ODB) mergeBase(a, b Oid) Oid {
	inA := db.ancestors(a)
	seen := map[Oid]bool{b: true}
	queue := []Oid{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if inA[cur] {
			return cur
		}
		if c := db.commit(cur); c != nil {
			for _, p := range c.Parents {
				if !seen[p] {
					seen[p] = true
					queue = append(queue, p)
				}
			}
		}
	}
	return zeroOid
}

// commitLog lists commits reachable from tip, newest committer time first (ties: discovery order).
func (db *ODB) commitLog(tip Oid, limit int) []Oid {
	type item struct {
		oid  Oid
		when int64
		seq  int
	}
	seen := map[Oid]bool{}
	var all []item
	stack := []Oid{tip}
	seq := 0
	for len(stack) > 0 {
		cur := stack[0]
		stack = stack[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		c := db.commit(cur)
		if c == nil {
			continue
		}
		all = append(all, item{cur, identTime(c.Committer), seq})
		seq++
		stack = append(stack, c.Parents...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].when != all[j].when {
			return all[i].when > all[j].when
		}
		return all[i].seq < all[j].seq
	})
	var out []Oid
	for _, it := range all {
		out = append(out, it.oid)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
