package main

// GitHub GraphQL v4 at /api/graphql: a small parser and executor (queries, mutations, variables, aliases,
// fragments, inline fragments, @skip/@include, operation batching) over the forge's store, with the schema
// introspection gh's feature probes read.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------------------------
// global ids

func nodeID(prefix string, id int64) string {
	return prefix + "_kwDO" + base64.RawURLEncoding.EncodeToString([]byte("forge:"+strconv.FormatInt(id, 10)))
}

func decodeNodeID(s string) (string, int64, bool) {
	i := strings.Index(s, "_kwDO")
	if i <= 0 {
		return "", 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s[i+5:])
	if err != nil || !bytes.HasPrefix(raw, []byte("forge:")) {
		return "", 0, false
	}
	n, err := strconv.ParseInt(string(raw[6:]), 10, 64)
	if err != nil {
		return "", 0, false
	}
	return s[:i], n, true
}

func cursorFor(i int) string { return base64.StdEncoding.EncodeToString([]byte("cursor:v2:" + strconv.Itoa(i))) }

func cursorIndex(c string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(c)
	if err != nil || !bytes.HasPrefix(raw, []byte("cursor:v2:")) {
		return 0, false
	}
	n, err := strconv.Atoi(string(raw[10:]))
	return n, err == nil
}

// ---------------------------------------------------------------------------------------------
// lexer and parser

type gqlVar struct{ name string }
type gqlEnum string

type gqlDirective struct {
	name string
	args map[string]any
}

const (
	selField = iota
	selSpread
	selInline
)

type gqlSel struct {
	kind       int
	alias      string
	name       string
	args       map[string]any
	sel        []*gqlSel
	typeCond   string
	frag       string
	directives []gqlDirective
	line, col  int
}

func (f *gqlSel) key() string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

type gqlVarDef struct {
	name   string
	def    any
	hasDef bool
}

type gqlOp struct {
	kind string // query | mutation | subscription
	name string
	vars []gqlVarDef
	sel  []*gqlSel
}

type gqlFrag struct {
	name     string
	typeCond string
	sel      []*gqlSel
}

type gqlDoc struct {
	ops   []*gqlOp
	frags map[string]*gqlFrag
}

type gqlParser struct {
	src  string
	pos  int
	line int
	lineStart int
}

type gqlTok struct {
	kind string // name, int, float, string, punct, eof
	val  string
	line, col int
}

func (p *gqlParser) errf(format string, a ...any) error {
	return fmt.Errorf("Parse error on line %d: "+format, append([]any{p.line}, a...)...)
}

func (p *gqlParser) skip() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == '\n':
			p.pos++
			p.line++
			p.lineStart = p.pos
		case c == ' ' || c == '\t' || c == '\r' || c == ',' || c == 0xEF || c == 0xBB || c == 0xBF:
			p.pos++
		case c == '#':
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

func isNameStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isNameChar(c byte) bool  { return isNameStart(c) || c >= '0' && c <= '9' }

func (p *gqlParser) next() (gqlTok, error) {
	p.skip()
	t := gqlTok{line: p.line, col: p.pos - p.lineStart + 1}
	if p.pos >= len(p.src) {
		t.kind = "eof"
		return t, nil
	}
	c := p.src[p.pos]
	switch {
	case strings.IndexByte("!$&()[]{}:=@|", c) >= 0:
		p.pos++
		t.kind, t.val = "punct", string(c)
		return t, nil
	case c == '.':
		if strings.HasPrefix(p.src[p.pos:], "...") {
			p.pos += 3
			t.kind, t.val = "punct", "..."
			return t, nil
		}
		return t, p.errf("unexpected '.'")
	case isNameStart(c):
		start := p.pos
		for p.pos < len(p.src) && isNameChar(p.src[p.pos]) {
			p.pos++
		}
		t.kind, t.val = "name", p.src[start:p.pos]
		return t, nil
	case c == '-' || c >= '0' && c <= '9':
		start := p.pos
		p.pos++
		float := false
		for p.pos < len(p.src) {
			d := p.src[p.pos]
			if d >= '0' && d <= '9' {
				p.pos++
			} else if d == '.' || d == 'e' || d == 'E' || ((d == '+' || d == '-') && (p.src[p.pos-1] == 'e' || p.src[p.pos-1] == 'E')) {
				float = true
				p.pos++
			} else {
				break
			}
		}
		t.kind, t.val = "int", p.src[start:p.pos]
		if float {
			t.kind = "float"
		}
		return t, nil
	case c == '"':
		if strings.HasPrefix(p.src[p.pos:], `"""`) {
			end := strings.Index(p.src[p.pos+3:], `"""`)
			if end < 0 {
				return t, p.errf("unterminated block string")
			}
			raw := p.src[p.pos+3 : p.pos+3+end]
			p.line += strings.Count(raw, "\n")
			p.pos += 6 + end
			t.kind, t.val = "string", blockString(raw)
			return t, nil
		}
		p.pos++
		var b strings.Builder
		for {
			if p.pos >= len(p.src) || p.src[p.pos] == '\n' {
				return t, p.errf("unterminated string")
			}
			d := p.src[p.pos]
			if d == '"' {
				p.pos++
				break
			}
			if d == '\\' && p.pos+1 < len(p.src) {
				e := p.src[p.pos+1]
				p.pos += 2
				switch e {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				case 'b':
					b.WriteByte('\b')
				case 'f':
					b.WriteByte('\f')
				case 'u':
					if p.pos+4 <= len(p.src) {
						if n, err := strconv.ParseUint(p.src[p.pos:p.pos+4], 16, 32); err == nil {
							b.WriteRune(rune(n))
						}
						p.pos += 4
					}
				default:
					b.WriteByte(e)
				}
				continue
			}
			b.WriteByte(d)
			p.pos++
		}
		t.kind, t.val = "string", b.String()
		return t, nil
	}
	return t, p.errf("unexpected character %q", string(c))
}

func blockString(raw string) string {
	lines := strings.Split(strings.ReplaceAll(raw, `\"""`, `"""`), "\n")
	indent := -1
	for i, l := range lines {
		if i == 0 {
			continue
		}
		trimmed := strings.TrimLeft(l, " \t")
		if trimmed == "" {
			continue
		}
		if n := len(l) - len(trimmed); indent < 0 || n < indent {
			indent = n
		}
	}
	for i := range lines {
		if i > 0 && indent > 0 && len(lines[i]) >= indent {
			lines[i] = lines[i][indent:]
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func (p *gqlParser) peek() (gqlTok, error) {
	pos, line, ls := p.pos, p.line, p.lineStart
	t, err := p.next()
	p.pos, p.line, p.lineStart = pos, line, ls
	return t, err
}

func (p *gqlParser) expect(val string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != "punct" || t.val != val {
		if t.kind == "eof" {
			return p.errf("expected %q, got end of document", val)
		}
		return p.errf("expected %q, got %q", val, t.val)
	}
	return nil
}

func (p *gqlParser) isPunct(val string) bool {
	t, err := p.peek()
	return err == nil && t.kind == "punct" && t.val == val
}

func (p *gqlParser) name() (string, error) {
	t, err := p.next()
	if err != nil {
		return "", err
	}
	if t.kind != "name" {
		return "", p.errf("expected a name, got %q", t.val)
	}
	return t.val, nil
}

func parseGraphQL(src string) (*gqlDoc, error) {
	p := &gqlParser{src: src, line: 1}
	doc := &gqlDoc{frags: map[string]*gqlFrag{}}
	for {
		t, err := p.peek()
		if err != nil {
			return nil, err
		}
		if t.kind == "eof" {
			break
		}
		if t.kind == "punct" && t.val == "{" {
			sel, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, &gqlOp{kind: "query", sel: sel})
			continue
		}
		if t.kind != "name" {
			return nil, p.errf("unexpected %q", t.val)
		}
		switch t.val {
		case "query", "mutation", "subscription":
			p.next()
			op := &gqlOp{kind: t.val}
			if nt, _ := p.peek(); nt.kind == "name" {
				op.name, _ = p.name()
			}
			if p.isPunct("(") {
				p.next()
				for !p.isPunct(")") {
					if err := p.expect("$"); err != nil {
						return nil, err
					}
					vn, err := p.name()
					if err != nil {
						return nil, err
					}
					if err := p.expect(":"); err != nil {
						return nil, err
					}
					if err := p.typeRef(); err != nil {
						return nil, err
					}
					vd := gqlVarDef{name: vn}
					if p.isPunct("=") {
						p.next()
						v, err := p.value(true)
						if err != nil {
							return nil, err
						}
						vd.def, vd.hasDef = v, true
					}
					if _, err := p.directives(); err != nil {
						return nil, err
					}
					op.vars = append(op.vars, vd)
				}
				p.next()
			}
			if _, err := p.directives(); err != nil {
				return nil, err
			}
			sel, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			op.sel = sel
			doc.ops = append(doc.ops, op)
		case "fragment":
			p.next()
			fn, err := p.name()
			if err != nil {
				return nil, err
			}
			on, err := p.name()
			if err != nil || on != "on" {
				return nil, p.errf("expected 'on' in fragment %s", fn)
			}
			tc, err := p.name()
			if err != nil {
				return nil, err
			}
			if _, err := p.directives(); err != nil {
				return nil, err
			}
			sel, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			doc.frags[fn] = &gqlFrag{name: fn, typeCond: tc, sel: sel}
		default:
			return nil, p.errf("unexpected %q", t.val)
		}
	}
	if len(doc.ops) == 0 {
		return nil, errors.New("the document holds no operation")
	}
	return doc, nil
}

func (p *gqlParser) typeRef() error {
	if p.isPunct("[") {
		p.next()
		if err := p.typeRef(); err != nil {
			return err
		}
		if err := p.expect("]"); err != nil {
			return err
		}
	} else if _, err := p.name(); err != nil {
		return err
	}
	if p.isPunct("!") {
		p.next()
	}
	return nil
}

func (p *gqlParser) directives() ([]gqlDirective, error) {
	var out []gqlDirective
	for p.isPunct("@") {
		p.next()
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		d := gqlDirective{name: n}
		if p.isPunct("(") {
			if d.args, err = p.arguments(); err != nil {
				return nil, err
			}
		}
		out = append(out, d)
	}
	return out, nil
}

func (p *gqlParser) arguments() (map[string]any, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	args := map[string]any{}
	for !p.isPunct(")") {
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		v, err := p.value(false)
		if err != nil {
			return nil, err
		}
		args[n] = v
	}
	p.next()
	return args, nil
}

func (p *gqlParser) value(constant bool) (any, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	switch t.kind {
	case "punct":
		switch t.val {
		case "$":
			if constant {
				return nil, p.errf("a variable is not allowed here")
			}
			n, err := p.name()
			if err != nil {
				return nil, err
			}
			return gqlVar{n}, nil
		case "[":
			list := []any{}
			for !p.isPunct("]") {
				v, err := p.value(constant)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			p.next()
			return list, nil
		case "{":
			obj := map[string]any{}
			for !p.isPunct("}") {
				n, err := p.name()
				if err != nil {
					return nil, err
				}
				if err := p.expect(":"); err != nil {
					return nil, err
				}
				v, err := p.value(constant)
				if err != nil {
					return nil, err
				}
				obj[n] = v
			}
			p.next()
			return obj, nil
		}
	case "int":
		n, err := strconv.ParseInt(t.val, 10, 64)
		if err != nil {
			return nil, p.errf("bad integer %s", t.val)
		}
		return int(n), nil
	case "float":
		f, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return nil, p.errf("bad float %s", t.val)
		}
		return f, nil
	case "string":
		return t.val, nil
	case "name":
		switch t.val {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		return gqlEnum(t.val), nil
	}
	return nil, p.errf("unexpected %q in a value", t.val)
}

func (p *gqlParser) selectionSet() ([]*gqlSel, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var out []*gqlSel
	for {
		t, err := p.peek()
		if err != nil {
			return nil, err
		}
		if t.kind == "eof" {
			return nil, p.errf("unexpected end of document in a selection set")
		}
		if t.kind == "punct" && t.val == "}" {
			p.next()
			break
		}
		if t.kind == "punct" && t.val == "..." {
			p.next()
			nt, err := p.peek()
			if err != nil {
				return nil, err
			}
			s := &gqlSel{line: t.line, col: t.col}
			if nt.kind == "name" && nt.val != "on" {
				p.next()
				s.kind, s.frag = selSpread, nt.val
				if s.directives, err = p.directives(); err != nil {
					return nil, err
				}
			} else {
				s.kind = selInline
				if nt.kind == "name" && nt.val == "on" {
					p.next()
					if s.typeCond, err = p.name(); err != nil {
						return nil, err
					}
				}
				if s.directives, err = p.directives(); err != nil {
					return nil, err
				}
				if s.sel, err = p.selectionSet(); err != nil {
					return nil, err
				}
			}
			out = append(out, s)
			continue
		}
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		f := &gqlSel{kind: selField, name: n, line: t.line, col: t.col}
		if p.isPunct(":") {
			p.next()
			f.alias = n
			if f.name, err = p.name(); err != nil {
				return nil, err
			}
		}
		if p.isPunct("(") {
			if f.args, err = p.arguments(); err != nil {
				return nil, err
			}
		}
		if f.directives, err = p.directives(); err != nil {
			return nil, err
		}
		if p.isPunct("{") {
			if f.sel, err = p.selectionSet(); err != nil {
				return nil, err
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// ---------------------------------------------------------------------------------------------
// schema (what introspection describes and what the executor accepts)

type gqlFieldDef struct{ Name, Type string }

type gqlTypeDef struct {
	Name       string
	Kind       string
	Fields     []gqlFieldDef
	fieldSet   map[string]bool
	Enum       []string
	Interfaces []string
	Possible   []string
}

var gqlTypes = map[string]*gqlTypeDef{}
var gqlTypeOrder []string

const gqlObjects = `
Query: viewer:User! repository:Repository node:Node nodes:[Node]! user:User repositoryOwner:RepositoryOwner rateLimit:RateLimit
Mutation: createIssue:CreateIssuePayload closeIssue:CloseIssuePayload reopenIssue:ReopenIssuePayload updateIssue:UpdateIssuePayload addComment:AddCommentPayload createPullRequest:CreatePullRequestPayload mergePullRequest:MergePullRequestPayload closePullRequest:ClosePullRequestPayload reopenPullRequest:ReopenPullRequestPayload updatePullRequest:UpdatePullRequestPayload createRepository:CreateRepositoryPayload deleteRepository:DeleteRepositoryPayload createRef:CreateRefPayload
User: id:ID! databaseId:Int login:String! name:String email:String! url:URI! avatarUrl:URI! bio:String company:String createdAt:DateTime! updatedAt:DateTime! isSiteAdmin:Boolean! repository:Repository repositories:RepositoryConnection!
Repository: id:ID! databaseId:Int name:String! nameWithOwner:String! owner:RepositoryOwner! description:String isPrivate:Boolean! visibility:RepositoryVisibility! url:URI! sshUrl:GitSSHRemote! homepageUrl:URI createdAt:DateTime! updatedAt:DateTime! pushedAt:DateTime defaultBranchRef:Ref viewerPermission:RepositoryPermission viewerCanAdminister:Boolean! hasIssuesEnabled:Boolean! hasWikiEnabled:Boolean! hasProjectsEnabled:Boolean! hasDiscussionsEnabled:Boolean! isFork:Boolean! isArchived:Boolean! isEmpty:Boolean! isTemplate:Boolean! isDisabled:Boolean! isLocked:Boolean! stargazerCount:Int! forkCount:Int! mergeCommitAllowed:Boolean! squashMergeAllowed:Boolean! rebaseMergeAllowed:Boolean! deleteBranchOnMerge:Boolean! issue:Issue issues:IssueConnection! issueOrPullRequest:IssueOrPullRequest pullRequest:PullRequest pullRequests:PullRequestConnection! labels:LabelConnection label:Label ref:Ref refs:RefConnection
Ref: id:ID! name:String! prefix:String! target:GitObject repository:Repository!
Commit: id:ID! oid:GitObjectID! abbreviatedOid:String! message:String! messageHeadline:String! messageBody:String! committedDate:DateTime! authoredDate:DateTime! url:URI! author:GitActor committer:GitActor
GitActor: name:String email:String date:GitTimestamp
Issue: id:ID! databaseId:Int number:Int! title:String! body:String! bodyText:String! state:IssueState! stateReason:IssueStateReason closed:Boolean! closedAt:DateTime createdAt:DateTime! updatedAt:DateTime! url:URI! author:Actor labels:LabelConnection comments:IssueCommentConnection! repository:Repository! assignees:UserConnection! locked:Boolean! viewerCanUpdate:Boolean! milestone:Milestone
PullRequest: id:ID! databaseId:Int number:Int! title:String! body:String! bodyText:String! state:PullRequestState! merged:Boolean! mergedAt:DateTime closed:Boolean! closedAt:DateTime createdAt:DateTime! updatedAt:DateTime! url:URI! isDraft:Boolean! mergeable:MergeableState! headRefName:String! headRefOid:GitObjectID! baseRefName:String! baseRefOid:GitObjectID! headRef:Ref baseRef:Ref mergeCommit:Commit additions:Int! deletions:Int! changedFiles:Int! files:PullRequestChangedFileConnection author:Actor repository:Repository! headRepository:Repository baseRepository:Repository comments:IssueCommentConnection! labels:LabelConnection assignees:UserConnection! isCrossRepository:Boolean! maintainerCanModify:Boolean! locked:Boolean! reviewDecision:PullRequestReviewDecision milestone:Milestone
PullRequestChangedFile: path:String! additions:Int! deletions:Int! changeType:PatchStatus!
IssueComment: id:ID! databaseId:Int body:String! bodyText:String! author:Actor createdAt:DateTime! updatedAt:DateTime! url:URI! issue:Issue!
Label: id:ID! name:String! color:String! description:String isDefault:Boolean!
Milestone: id:ID! number:Int! title:String!
PageInfo: hasNextPage:Boolean! hasPreviousPage:Boolean! startCursor:String endCursor:String
RateLimit: limit:Int! cost:Int! remaining:Int! resetAt:DateTime! used:Int! nodeCount:Int!
IssueConnection: nodes:[Issue] edges:[IssueEdge] pageInfo:PageInfo! totalCount:Int!
IssueEdge: cursor:String! node:Issue
PullRequestConnection: nodes:[PullRequest] edges:[PullRequestEdge] pageInfo:PageInfo! totalCount:Int!
PullRequestEdge: cursor:String! node:PullRequest
IssueCommentConnection: nodes:[IssueComment] edges:[IssueCommentEdge] pageInfo:PageInfo! totalCount:Int!
IssueCommentEdge: cursor:String! node:IssueComment
LabelConnection: nodes:[Label] edges:[LabelEdge] pageInfo:PageInfo! totalCount:Int!
LabelEdge: cursor:String! node:Label
UserConnection: nodes:[User] edges:[UserEdge] pageInfo:PageInfo! totalCount:Int!
UserEdge: cursor:String! node:User
RepositoryConnection: nodes:[Repository] edges:[RepositoryEdge] pageInfo:PageInfo! totalCount:Int!
RepositoryEdge: cursor:String! node:Repository
RefConnection: nodes:[Ref] edges:[RefEdge] pageInfo:PageInfo! totalCount:Int!
RefEdge: cursor:String! node:Ref
PullRequestChangedFileConnection: nodes:[PullRequestChangedFile] edges:[PullRequestChangedFileEdge] pageInfo:PageInfo! totalCount:Int!
PullRequestChangedFileEdge: cursor:String! node:PullRequestChangedFile
CreateIssuePayload: clientMutationId:String issue:Issue
CloseIssuePayload: clientMutationId:String issue:Issue
ReopenIssuePayload: clientMutationId:String issue:Issue
UpdateIssuePayload: clientMutationId:String issue:Issue
AddCommentPayload: clientMutationId:String commentEdge:IssueCommentEdge subject:Node
CreatePullRequestPayload: clientMutationId:String pullRequest:PullRequest
MergePullRequestPayload: clientMutationId:String pullRequest:PullRequest
ClosePullRequestPayload: clientMutationId:String pullRequest:PullRequest
ReopenPullRequestPayload: clientMutationId:String pullRequest:PullRequest
UpdatePullRequestPayload: clientMutationId:String pullRequest:PullRequest
CreateRepositoryPayload: clientMutationId:String repository:Repository
DeleteRepositoryPayload: clientMutationId:String
CreateRefPayload: clientMutationId:String ref:Ref
`

var gqlEnums = map[string][]string{
	"IssueState":                {"OPEN", "CLOSED"},
	"IssueStateReason":          {"COMPLETED", "NOT_PLANNED", "REOPENED", "DUPLICATE"},
	"PullRequestState":          {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":            {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"RepositoryPermission":      {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"RepositoryVisibility":      {"PRIVATE", "PUBLIC", "INTERNAL"},
	"SearchType":                {"ISSUE", "ISSUE_ADVANCED", "REPOSITORY", "USER", "DISCUSSION"},
	"IssueOrderField":           {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":            {"ASC", "DESC"},
	"PullRequestMergeMethod":    {"MERGE", "SQUASH", "REBASE"},
	"PatchStatus":               {"ADDED", "DELETED", "RENAMED", "COPIED", "MODIFIED", "CHANGED"},
	"PullRequestReviewDecision": {"CHANGES_REQUESTED", "APPROVED", "REVIEW_REQUIRED"},
}

var gqlScalars = []string{"ID", "String", "Int", "Float", "Boolean", "DateTime", "URI", "GitObjectID", "GitSSHRemote", "GitTimestamp"}

var gqlInterfaces = map[string][]string{
	"Node":                     {"User", "Repository", "Ref", "Commit", "Issue", "PullRequest", "IssueComment", "Label", "Milestone"},
	"Actor":                    {"User"},
	"RepositoryOwner":          {"User"},
	"GitObject":                {"Commit"},
	"Comment":                  {"Issue", "PullRequest", "IssueComment"},
	"Closable":                 {"Issue", "PullRequest"},
	"UniformResourceLocatable": {"User", "Repository", "Issue", "PullRequest", "IssueComment"},
	"RepositoryInfo":           {"Repository"},
}

var gqlUnions = map[string][]string{
	"IssueOrPullRequest": {"Issue", "PullRequest"},
	"SearchResultItem":   {"Issue", "PullRequest", "Repository", "User"},
}

// abstract type -> concrete type -> true
var gqlImplements = map[string]map[string]bool{}

func init() {
	add := func(d *gqlTypeDef) {
		gqlTypes[d.Name] = d
		gqlTypeOrder = append(gqlTypeOrder, d.Name)
	}
	for _, line := range strings.Split(strings.TrimSpace(gqlObjects), "\n") {
		name, rest, _ := strings.Cut(line, ":")
		d := &gqlTypeDef{Name: strings.TrimSpace(name), Kind: "OBJECT", fieldSet: map[string]bool{}}
		for _, f := range strings.Fields(rest) {
			fn, ft, _ := strings.Cut(f, ":")
			d.Fields = append(d.Fields, gqlFieldDef{fn, ft})
			d.fieldSet[fn] = true
		}
		add(d)
	}
	for name, vals := range gqlEnums {
		add(&gqlTypeDef{Name: name, Kind: "ENUM", Enum: vals})
	}
	for _, name := range gqlScalars {
		add(&gqlTypeDef{Name: name, Kind: "SCALAR"})
	}
	for name, members := range gqlInterfaces {
		gqlImplements[name] = map[string]bool{}
		for _, m := range members {
			gqlImplements[name][m] = true
			if d := gqlTypes[m]; d != nil {
				d.Interfaces = append(d.Interfaces, name)
			}
		}
		// An interface's fields are those its members share; introspection lists the common ones.
		d := &gqlTypeDef{Name: name, Kind: "INTERFACE", Possible: members, fieldSet: map[string]bool{}}
		if first := gqlTypes[members[0]]; first != nil {
			for _, f := range first.Fields {
				shared := true
				for _, m := range members[1:] {
					if md := gqlTypes[m]; md == nil || !md.fieldSet[f.Name] {
						shared = false
					}
				}
				if shared {
					d.Fields = append(d.Fields, f)
					d.fieldSet[f.Name] = true
				}
			}
		}
		add(d)
	}
	for name, members := range gqlUnions {
		gqlImplements[name] = map[string]bool{}
		for _, m := range members {
			gqlImplements[name][m] = true
		}
		add(&gqlTypeDef{Name: name, Kind: "UNION", Possible: members})
	}
}

func typeMatches(cond, concrete string) bool {
	if cond == "" || cond == concrete {
		return true
	}
	if set, ok := gqlImplements[cond]; ok {
		return set[concrete]
	}
	return false
}

// ---------------------------------------------------------------------------------------------
// values the executor completes

type gobj struct {
	T string
	V any
}

type issueV struct {
	repo *Repo
	is   *Issue
}

type pullV struct {
	repo *Repo
	p    *Pull
}

type commentV struct {
	c     *Comment
	issue *issueV
}

type refV struct {
	repo *Repo
	name string
	oid  Oid
}

type commitV struct {
	repo *Repo
	oid  Oid
}

type connV struct {
	T        string // node type
	items    []any  // completed node values for the page
	start    int    // index of items[0] in the whole list
	total    int
	hasNext  bool
	hasPrev  bool
}

type gqlError struct {
	msg  string
	code string
}

func (e *gqlError) Error() string { return e.msg }

func gerr(code, format string, a ...any) error { return &gqlError{msg: fmt.Sprintf(format, a...), code: code} }

type gqlExec struct {
	s       *Server
	r       *http.Request
	doc     *gqlDoc
	vars    map[string]any
	errors  []any
	invalid bool
}

func (e *gqlExec) addError(err error, path []any, f *gqlSel) {
	entry := map[string]any{"message": err.Error()}
	if len(path) > 0 {
		entry["path"] = append([]any(nil), path...)
	}
	if f != nil && f.line > 0 {
		entry["locations"] = []any{map[string]any{"line": f.line, "column": f.col}}
	}
	if ge, ok := err.(*gqlError); ok && ge.code != "" {
		entry["type"] = ge.code
		entry["extensions"] = map[string]any{"code": ge.code}
	}
	e.errors = append(e.errors, entry)
}

// resolveValue replaces variables (and enum markers) in an argument value.
func (e *gqlExec) resolveValue(v any) any {
	switch t := v.(type) {
	case gqlVar:
		return e.vars[t.name]
	case gqlEnum:
		return string(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = e.resolveValue(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = e.resolveValue(x)
		}
		return out
	}
	return v
}

func (e *gqlExec) args(f *gqlSel) map[string]any {
	out := make(map[string]any, len(f.args))
	for k, v := range f.args {
		out[k] = e.resolveValue(v)
	}
	return out
}

func (e *gqlExec) included(ds []gqlDirective) bool {
	for _, d := range ds {
		cond, _ := e.resolveValue(d.args["if"]).(bool)
		if d.name == "skip" && cond {
			return false
		}
		if d.name == "include" && !cond {
			return false
		}
	}
	return true
}

// collect flattens a selection set for a concrete type into response keys, merging repeated keys.
func (e *gqlExec) collect(T string, sel []*gqlSel, keys *[]string, fields map[string][]*gqlSel, visited map[string]bool) {
	for _, s := range sel {
		if !e.included(s.directives) {
			continue
		}
		switch s.kind {
		case selField:
			k := s.key()
			if _, seen := fields[k]; !seen {
				*keys = append(*keys, k)
			}
			fields[k] = append(fields[k], s)
		case selInline:
			if typeMatches(s.typeCond, T) {
				e.collect(T, s.sel, keys, fields, visited)
			}
		case selSpread:
			if visited[s.frag] {
				continue
			}
			fr := e.doc.frags[s.frag]
			if fr == nil {
				e.addError(gerr("", "Fragment %s was used, but not defined", s.frag), nil, s)
				e.invalid = true
				continue
			}
			visited[s.frag] = true
			if typeMatches(fr.typeCond, T) {
				e.collect(T, fr.sel, keys, fields, visited)
			}
			delete(visited, s.frag)
		}
	}
}

// orderedObject keeps response keys in selection order when encoded.
type orderedObject struct {
	keys []string
	vals map[string]any
}

func (o *orderedObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (e *gqlExec) selectObject(o gobj, sel []*gqlSel, path []any) any {
	var keys []string
	fields := map[string][]*gqlSel{}
	e.collect(o.T, sel, &keys, fields, map[string]bool{})
	out := &orderedObject{keys: keys, vals: make(map[string]any, len(keys))}
	def := gqlTypes[o.T]
	for _, k := range keys {
		group := fields[k]
		f := group[0]
		var sub []*gqlSel
		for _, g := range group {
			sub = append(sub, g.sel...)
		}
		p := append(path, k)
		if f.name == "__typename" {
			out.vals[k] = o.T
			continue
		}
		if !strings.HasPrefix(o.T, "__") && !(o.T == "Query" && (f.name == "__type" || f.name == "__schema")) {
			if def == nil || !def.fieldSet[f.name] {
				e.addError(gerr("undefinedField", "Field '%s' doesn't exist on type '%s'", f.name, o.T), p, f)
				e.invalid = true
				continue
			}
		}
		val, err := e.field(o, f.name, e.args(f))
		if err != nil {
			e.addError(err, p, f)
			out.vals[k] = nil
			continue
		}
		out.vals[k] = e.complete(val, sub, p)
	}
	return out
}

func (e *gqlExec) complete(v any, sel []*gqlSel, path []any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case gobj:
		if len(sel) == 0 {
			return map[string]any{}
		}
		return e.selectObject(t, sel, path)
	case []gobj:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = e.complete(x, sel, append(path, i))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = e.complete(x, sel, append(path, i))
		}
		return out
	}
	return v
}

// ---------------------------------------------------------------------------------------------
// argument helpers

func argInt(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false
	}
	return intOf(v)
}

func argStr(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func argList(args map[string]any, key string) []string {
	var out []string
	switch t := args[key].(type) {
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	case string:
		out = append(out, t)
	}
	return out
}

func argObj(args map[string]any, key string) map[string]any {
	m, _ := args[key].(map[string]any)
	return m
}

// page cuts a list into a connection page per first/last/after/before.
func page(T string, all []any, args map[string]any) (connV, error) {
	start, end := 0, len(all)
	if c, ok := argStr(args, "after"); ok && c != "" {
		i, ok := cursorIndex(c)
		if !ok {
			return connV{}, gerr("INVALID_CURSOR_ARGUMENTS", "`%s` does not appear to be a valid cursor.", c)
		}
		start = i + 1
	}
	if c, ok := argStr(args, "before"); ok && c != "" {
		i, ok := cursorIndex(c)
		if !ok {
			return connV{}, gerr("INVALID_CURSOR_ARGUMENTS", "`%s` does not appear to be a valid cursor.", c)
		}
		end = i
	}
	if start > len(all) {
		start = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	if end < start {
		end = start
	}
	first, hasFirst := argInt(args, "first")
	last, hasLast := argInt(args, "last")
	if (hasFirst && (first < 0 || first > 100)) || (hasLast && (last < 0 || last > 100)) {
		return connV{}, gerr("", "Requesting %d records on the connection exceeds the `first` limit of 100 records.", max(first, last))
	}
	if !hasFirst && !hasLast {
		first, hasFirst = 100, true
	}
	if hasFirst && end-start > first {
		end = start + first
	}
	if hasLast && end-start > last {
		start = end - last
	}
	return connV{T: T, items: all[start:end], start: start, total: len(all), hasNext: end < len(all), hasPrev: start > 0}, nil
}

// ---------------------------------------------------------------------------------------------
// objects

func (e *gqlExec) user() gobj {
	login := e.s.st.Viewer()
	base := e.s.baseURL(e.r)
	return gobj{"User", map[string]any{
		"id": nodeID("U", e.s.st.userID), "databaseId": e.s.st.userID, "login": login, "name": login,
		"email": login + "@users.noreply.example", "url": base + "/" + login, "avatarUrl": base + "/avatars/" + login,
		"bio": nil, "company": nil, "createdAt": iso(e.s.started), "updatedAt": iso(e.s.started), "isSiteAdmin": false,
	}}
}

func (e *gqlExec) nodeByID(id string) (gobj, bool) {
	prefix, n, ok := decodeNodeID(id)
	if !ok {
		return gobj{}, false
	}
	if prefix == "U" {
		if n == e.s.st.userID {
			return e.user(), true
		}
		return gobj{}, false
	}
	switch t := e.s.st.byDatabaseID(n).(type) {
	case *Repo:
		if prefix == "R" {
			return gobj{"Repository", t}, true
		}
	case *Issue:
		if prefix == "I" {
			if r := e.s.st.repoOf(t); r != nil {
				return gobj{"Issue", &issueV{r, t}}, true
			}
		}
	case *Pull:
		if prefix == "PR" {
			if r := e.s.st.repoOf(t); r != nil {
				return gobj{"PullRequest", &pullV{r, t}}, true
			}
		}
	case *Comment:
		if prefix == "IC" {
			return gobj{"IssueComment", &commentV{c: t}}, true
		}
	}
	return gobj{}, false
}

func notResolved(id string) error {
	return gerr("NOT_FOUND", "Could not resolve to a node with the global id of '%s'", id)
}

func (e *gqlExec) issueConn(repo *Repo, args map[string]any) (any, error) {
	state := "all"
	states := argList(args, "states")
	if fb := argObj(args, "filterBy"); fb != nil {
		if sts, ok := fb["states"].([]any); ok && len(states) == 0 {
			for _, x := range sts {
				if s, ok := x.(string); ok {
					states = append(states, s)
				}
			}
		}
	}
	if len(states) == 1 {
		state = strings.ToLower(states[0])
	}
	sortBy, direction := "created", "asc"
	if ob := argObj(args, "orderBy"); ob != nil {
		switch ob["field"] {
		case "UPDATED_AT":
			sortBy = "updated"
		case "COMMENTS":
			sortBy = "comments"
		}
		if ob["direction"] == "DESC" {
			direction = "desc"
		}
	}
	list := e.s.filteredIssues(repo, state, sortBy, direction, argList(args, "labels"))
	all := make([]any, len(list))
	for i, is := range list {
		all[i] = gobj{"Issue", &issueV{repo, is}}
	}
	c, err := page("Issue", all, args)
	if err != nil {
		return nil, err
	}
	return gobj{"IssueConnection", &c}, nil
}

func (e *gqlExec) pullConn(repo *Repo, args map[string]any) (any, error) {
	want := map[string]bool{}
	for _, s := range argList(args, "states") {
		want[s] = true
	}
	head, _ := argStr(args, "headRefName")
	base, _ := argStr(args, "baseRefName")
	list := e.s.pullList(repo, "all")
	var all []any
	for _, p := range list {
		e.s.st.mu.RLock()
		st := pullState(p)
		ph, pb := p.Head, p.Base
		e.s.st.mu.RUnlock()
		if len(want) > 0 && !want[st] {
			continue
		}
		if (head != "" && ph != head) || (base != "" && pb != base) {
			continue
		}
		all = append(all, gobj{"PullRequest", &pullV{repo, p}})
	}
	if ob := argObj(args, "orderBy"); ob != nil && ob["direction"] == "DESC" {
		for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
			all[i], all[j] = all[j], all[i]
		}
	}
	c, err := page("PullRequest", all, args)
	if err != nil {
		return nil, err
	}
	return gobj{"PullRequestConnection", &c}, nil
}

func pullState(p *Pull) string {
	if p.Merged {
		return "MERGED"
	}
	return strings.ToUpper(p.State)
}

func (e *gqlExec) labelConn(labels []*Label, args map[string]any) (any, error) {
	all := make([]any, len(labels))
	for i, l := range labels {
		all[i] = gobj{"Label", l}
	}
	c, err := page("Label", all, args)
	if err != nil {
		return nil, err
	}
	return gobj{"LabelConnection", &c}, nil
}

func (e *gqlExec) commentConn(comments []*Comment, owner *issueV, args map[string]any) (any, error) {
	all := make([]any, len(comments))
	for i, c := range comments {
		all[i] = gobj{"IssueComment", &commentV{c: c, issue: owner}}
	}
	c, err := page("IssueComment", all, args)
	if err != nil {
		return nil, err
	}
	return gobj{"IssueCommentConnection", &c}, nil
}

func emptyConn(T string) any {
	return gobj{T + "Connection", &connV{T: T, items: []any{}}}
}

func (e *gqlExec) defaultRef(repo *Repo) any {
	e.s.st.mu.RLock()
	def := repo.DefaultBranch
	o, ok := repo.Refs["refs/heads/"+def]
	e.s.st.mu.RUnlock()
	if !ok {
		return nil
	}
	return gobj{"Ref", &refV{repo, "refs/heads/" + def, o}}
}

func (e *gqlExec) field(o gobj, name string, args map[string]any) (any, error) {
	st := e.s.st
	base := e.s.baseURL(e.r)
	if m, ok := o.V.(map[string]any); ok && !(o.T == "User" && strings.HasPrefix(name, "repositor")) {
		return m[name], nil
	}
	switch o.T {
	case "User":
		if name == "repository" {
			nm, _ := argStr(args, "name")
			if r := st.repo(st.Viewer(), nm); r != nil {
				return gobj{"Repository", r}, nil
			}
			return nil, nil
		}
		repos := st.listRepos()
		all := make([]any, len(repos))
		for i, r := range repos {
			all[i] = gobj{"Repository", r}
		}
		c, err := page("Repository", all, args)
		if err != nil {
			return nil, err
		}
		return gobj{"RepositoryConnection", &c}, nil
	case "Query":
		switch name {
		case "viewer":
			return e.user(), nil
		case "user", "repositoryOwner":
			login, _ := argStr(args, "login")
			if st.ownerOK(login) {
				return e.user(), nil
			}
			return nil, gerr("NOT_FOUND", "Could not resolve to a User with the login of '%s'.", login)
		case "repository":
			owner, _ := argStr(args, "owner")
			nm, _ := argStr(args, "name")
			if r := st.repo(owner, nm); r != nil {
				return gobj{"Repository", r}, nil
			}
			return nil, nil
		case "node":
			id, _ := argStr(args, "id")
			if n, ok := e.nodeByID(id); ok {
				return n, nil
			}
			return nil, notResolved(id)
		case "nodes":
			var out []any
			for _, id := range argList(args, "ids") {
				if n, ok := e.nodeByID(id); ok {
					out = append(out, n)
				} else {
					out = append(out, nil)
				}
			}
			if out == nil {
				out = []any{}
			}
			return out, nil
		case "rateLimit":
			return gobj{"RateLimit", map[string]any{"limit": 5000, "cost": 1, "remaining": 4999, "used": 1, "nodeCount": 0,
				"resetAt": iso(time.Now().Add(time.Hour))}}, nil
		case "__type":
			n, _ := argStr(args, "name")
			if gqlTypes[n] == nil {
				return nil, nil
			}
			return gobj{"__Type", n}, nil
		case "__schema":
			return gobj{"__Schema", nil}, nil
		}
	case "Mutation":
		return e.mutation(name, args)
	case "Repository":
		r := o.V.(*Repo)
		st.mu.RLock()
		name2, desc, private, created, updated, pushed := r.Name, r.Description, r.Private, r.CreatedAt, r.UpdatedAt, r.PushedAt
		empty := len(r.Refs) == 0
		st.mu.RUnlock()
		full := st.Viewer() + "/" + name2
		switch name {
		case "id":
			return nodeID("R", r.ID), nil
		case "databaseId":
			return r.ID, nil
		case "name":
			return name2, nil
		case "nameWithOwner":
			return full, nil
		case "owner":
			return e.user(), nil
		case "description":
			if desc == "" {
				return nil, nil
			}
			return desc, nil
		case "isPrivate":
			return private, nil
		case "visibility":
			if private {
				return "PRIVATE", nil
			}
			return "PUBLIC", nil
		case "url":
			return base + "/" + full, nil
		case "sshUrl":
			return "git@" + e.r.Host + ":" + full + ".git", nil
		case "homepageUrl":
			return nil, nil
		case "createdAt":
			return iso(created), nil
		case "updatedAt":
			return iso(updated), nil
		case "pushedAt":
			return iso(pushed), nil
		case "defaultBranchRef":
			return e.defaultRef(r), nil
		case "viewerPermission":
			return "ADMIN", nil
		case "viewerCanAdminister", "hasIssuesEnabled", "mergeCommitAllowed", "squashMergeAllowed", "rebaseMergeAllowed":
			return true, nil
		case "hasWikiEnabled", "hasProjectsEnabled", "hasDiscussionsEnabled", "isFork", "isArchived", "isTemplate", "isDisabled", "isLocked", "deleteBranchOnMerge":
			return false, nil
		case "isEmpty":
			return empty, nil
		case "stargazerCount", "forkCount":
			return 0, nil
		case "issue":
			n, _ := argInt(args, "number")
			if is := st.issue(r, n); is != nil {
				return gobj{"Issue", &issueV{r, is}}, nil
			}
			return nil, gerr("NOT_FOUND", "Could not resolve to an Issue with the number of %d.", n)
		case "pullRequest":
			n, _ := argInt(args, "number")
			if p := st.pull(r, n); p != nil {
				return gobj{"PullRequest", &pullV{r, p}}, nil
			}
			return nil, gerr("NOT_FOUND", "Could not resolve to a PullRequest with the number of %d.", n)
		case "issueOrPullRequest":
			n, _ := argInt(args, "number")
			if is := st.issue(r, n); is != nil {
				return gobj{"Issue", &issueV{r, is}}, nil
			}
			if p := st.pull(r, n); p != nil {
				return gobj{"PullRequest", &pullV{r, p}}, nil
			}
			return nil, gerr("NOT_FOUND", "Could not resolve to an issue or pull request with the number of %d.", n)
		case "issues":
			return e.issueConn(r, args)
		case "pullRequests":
			return e.pullConn(r, args)
		case "labels":
			st.mu.RLock()
			ls := append([]*Label(nil), r.Labels...)
			st.mu.RUnlock()
			return e.labelConn(ls, args)
		case "label":
			n, _ := argStr(args, "name")
			st.mu.RLock()
			l := st.findLabel(r, n)
			st.mu.RUnlock()
			if l == nil {
				return nil, nil
			}
			return gobj{"Label", l}, nil
		case "ref":
			q, _ := argStr(args, "qualifiedName")
			if !strings.HasPrefix(q, "refs/") {
				q = "refs/heads/" + q
			}
			if oid, ok := st.ref(r, q); ok {
				return gobj{"Ref", &refV{r, q, oid}}, nil
			}
			return nil, nil
		case "refs":
			prefix, _ := argStr(args, "refPrefix")
			refs, _, _ := st.refsSnapshot(r)
			var all []any
			for _, n := range sortedRefNames(refs) {
				if strings.HasPrefix(n, prefix) {
					all = append(all, gobj{"Ref", &refV{r, n, refs[n]}})
				}
			}
			c, err := page("Ref", all, args)
			if err != nil {
				return nil, err
			}
			return gobj{"RefConnection", &c}, nil
		}
	case "Ref":
		ref := o.V.(*refV)
		switch name {
		case "id":
			return "REF_" + base64.RawURLEncoding.EncodeToString([]byte(ref.repo.Name+":"+ref.name)), nil
		case "name":
			if strings.HasPrefix(ref.name, "refs/heads/") {
				return strings.TrimPrefix(ref.name, "refs/heads/"), nil
			}
			return strings.TrimPrefix(ref.name, "refs/tags/"), nil
		case "prefix":
			if strings.HasPrefix(ref.name, "refs/tags/") {
				return "refs/tags/", nil
			}
			return "refs/heads/", nil
		case "target":
			return gobj{"Commit", &commitV{ref.repo, st.db.peel(ref.oid)}}, nil
		case "repository":
			return gobj{"Repository", ref.repo}, nil
		}
	case "Commit":
		cv := o.V.(*commitV)
		c := st.db.commit(cv.oid)
		if c == nil {
			return nil, nil
		}
		msg := strings.TrimSuffix(c.Message, "\n")
		headline, body, _ := strings.Cut(msg, "\n")
		actor := func(ident string) any {
			n, em := identNameEmail(ident)
			return gobj{"GitActor", map[string]any{"name": n, "email": em, "date": iso(time.Unix(identTime(ident), 0))}}
		}
		switch name {
		case "id":
			return "C_" + cv.oid.String(), nil
		case "oid":
			return cv.oid.String(), nil
		case "abbreviatedOid":
			return cv.oid.String()[:7], nil
		case "message":
			return msg, nil
		case "messageHeadline":
			return headline, nil
		case "messageBody":
			return strings.TrimLeft(body, "\n"), nil
		case "committedDate":
			return iso(time.Unix(identTime(c.Committer), 0)), nil
		case "authoredDate":
			return iso(time.Unix(identTime(c.Author), 0)), nil
		case "url":
			return base + "/" + st.Viewer() + "/" + cv.repo.Name + "/commit/" + cv.oid.String(), nil
		case "author":
			return actor(c.Author), nil
		case "committer":
			return actor(c.Committer), nil
		}
	case "Issue":
		iv := o.V.(*issueV)
		is := iv.is
		st.mu.RLock()
		title, body, state, created, updated, closedAt := is.Title, is.Body, is.State, is.CreatedAt, is.UpdatedAt, is.ClosedAt
		labels := append([]*Label(nil), is.Labels...)
		comments := append([]*Comment(nil), is.Comments...)
		st.mu.RUnlock()
		switch name {
		case "id":
			return nodeID("I", is.ID), nil
		case "databaseId":
			return is.ID, nil
		case "number":
			return is.Number, nil
		case "title":
			return title, nil
		case "body", "bodyText":
			return body, nil
		case "state":
			return strings.ToUpper(state), nil
		case "stateReason":
			if state == "closed" {
				return "COMPLETED", nil
			}
			return nil, nil
		case "closed":
			return state == "closed", nil
		case "closedAt":
			return isoPtr(closedAt), nil
		case "createdAt":
			return iso(created), nil
		case "updatedAt":
			return iso(updated), nil
		case "url":
			return base + "/" + st.Viewer() + "/" + iv.repo.Name + "/issues/" + itoa(is.Number), nil
		case "author":
			return e.user(), nil
		case "labels":
			return e.labelConn(labels, args)
		case "comments":
			return e.commentConn(comments, iv, args)
		case "repository":
			return gobj{"Repository", iv.repo}, nil
		case "assignees":
			return emptyConn("User"), nil
		case "locked":
			return false, nil
		case "viewerCanUpdate":
			return true, nil
		case "milestone":
			return nil, nil
		}
	case "PullRequest":
		pv := o.V.(*pullV)
		p := pv.p
		head, baseSha := st.pullShas(pv.repo, p)
		st.mu.RLock()
		title, body, state, merged, mergedAt, closedAt, created, updated, mcs := p.Title, p.Body, p.State, p.Merged, p.MergedAt, p.ClosedAt, p.CreatedAt, p.UpdatedAt, p.MergeCommitSha
		headName, baseName := p.Head, p.Base
		comments := append([]*Comment(nil), p.Comments...)
		gstate := pullState(p)
		st.mu.RUnlock()
		files := func() []FileChange { return st.pullFiles(pv.repo, p) }
		switch name {
		case "id":
			return nodeID("PR", p.ID), nil
		case "databaseId":
			return p.ID, nil
		case "number":
			return p.Number, nil
		case "title":
			return title, nil
		case "body", "bodyText":
			return body, nil
		case "state":
			return gstate, nil
		case "merged":
			return merged, nil
		case "mergedAt":
			return isoPtr(mergedAt), nil
		case "closed":
			return state == "closed", nil
		case "closedAt":
			return isoPtr(closedAt), nil
		case "createdAt":
			return iso(created), nil
		case "updatedAt":
			return iso(updated), nil
		case "url":
			return base + "/" + st.Viewer() + "/" + pv.repo.Name + "/pull/" + itoa(p.Number), nil
		case "isDraft", "isCrossRepository", "locked":
			return false, nil
		case "maintainerCanModify":
			return true, nil
		case "reviewDecision", "milestone":
			return nil, nil
		case "mergeable":
			if state != "open" {
				return "UNKNOWN", nil
			}
			if st.mergeable(pv.repo, p) {
				return "MERGEABLE", nil
			}
			return "CONFLICTING", nil
		case "headRefName":
			return headName, nil
		case "baseRefName":
			return baseName, nil
		case "headRefOid":
			return head.String(), nil
		case "baseRefOid":
			return baseSha.String(), nil
		case "headRef":
			if o, ok := st.ref(pv.repo, "refs/heads/"+headName); ok {
				return gobj{"Ref", &refV{pv.repo, "refs/heads/" + headName, o}}, nil
			}
			return nil, nil
		case "baseRef":
			if o, ok := st.ref(pv.repo, "refs/heads/"+baseName); ok {
				return gobj{"Ref", &refV{pv.repo, "refs/heads/" + baseName, o}}, nil
			}
			return nil, nil
		case "mergeCommit":
			if mcs.IsZero() {
				return nil, nil
			}
			return gobj{"Commit", &commitV{pv.repo, mcs}}, nil
		case "additions", "deletions", "changedFiles":
			n := 0
			for _, f := range files() {
				switch name {
				case "additions":
					n += f.Additions
				case "deletions":
					n += f.Deletions
				default:
					n++
				}
			}
			return n, nil
		case "files":
			fs := files()
			all := make([]any, len(fs))
			for i, f := range fs {
				ct := "MODIFIED"
				switch f.Status {
				case "added":
					ct = "ADDED"
				case "removed":
					ct = "DELETED"
				}
				all[i] = gobj{"PullRequestChangedFile", map[string]any{"path": f.Path, "additions": f.Additions, "deletions": f.Deletions, "changeType": ct}}
			}
			c, err := page("PullRequestChangedFile", all, args)
			if err != nil {
				return nil, err
			}
			return gobj{"PullRequestChangedFileConnection", &c}, nil
		case "author":
			return e.user(), nil
		case "repository", "headRepository", "baseRepository":
			return gobj{"Repository", pv.repo}, nil
		case "comments":
			return e.commentConn(comments, nil, args)
		case "labels":
			return e.labelConn(nil, args)
		case "assignees":
			return emptyConn("User"), nil
		}
	case "IssueComment":
		cv := o.V.(*commentV)
		switch name {
		case "id":
			return nodeID("IC", cv.c.ID), nil
		case "databaseId":
			return cv.c.ID, nil
		case "body", "bodyText":
			return cv.c.Body, nil
		case "author":
			return e.user(), nil
		case "createdAt", "updatedAt":
			return iso(cv.c.CreatedAt), nil
		case "url":
			if cv.issue != nil {
				return base + "/" + st.Viewer() + "/" + cv.issue.repo.Name + "/issues/" + itoa(cv.issue.is.Number) + "#issuecomment-" + strconv.FormatInt(cv.c.ID, 10), nil
			}
			return base + "/comments/" + strconv.FormatInt(cv.c.ID, 10), nil
		case "issue":
			if cv.issue != nil {
				return gobj{"Issue", cv.issue}, nil
			}
			return nil, nil
		}
	case "Label":
		l := o.V.(*Label)
		switch name {
		case "id":
			return nodeID("LA", l.ID), nil
		case "name":
			return l.Name, nil
		case "color":
			return l.Color, nil
		case "description":
			if l.Description == "" {
				return nil, nil
			}
			return l.Description, nil
		case "isDefault":
			return false, nil
		}
	case "__Type":
		return e.typeField(o.V.(string), name, args)
	case "__Field":
		fd := o.V.(gqlFieldDef)
		switch name {
		case "name":
			return fd.Name, nil
		case "description", "deprecationReason":
			return nil, nil
		case "args":
			return []any{}, nil
		case "type":
			return gobj{"__Type", fd.Type}, nil
		case "isDeprecated":
			return false, nil
		}
		return nil, nil
	case "__EnumValue":
		switch name {
		case "name":
			return o.V.(string), nil
		case "isDeprecated":
			return false, nil
		}
		return nil, nil
	case "__Schema":
		switch name {
		case "queryType":
			return gobj{"__Type", "Query"}, nil
		case "mutationType":
			return gobj{"__Type", "Mutation"}, nil
		case "types":
			out := make([]any, 0, len(gqlTypeOrder))
			for _, n := range gqlTypeOrder {
				out = append(out, gobj{"__Type", n})
			}
			return out, nil
		case "directives":
			return []any{}, nil
		}
		return nil, nil
	default:
		if strings.HasSuffix(o.T, "Connection") {
			c := o.V.(*connV)
			switch name {
			case "nodes":
				return c.items, nil
			case "edges":
				out := make([]any, len(c.items))
				for i, n := range c.items {
					out[i] = gobj{c.T + "Edge", map[string]any{"cursor": cursorFor(c.start + i), "node": n}}
				}
				return out, nil
			case "totalCount":
				return c.total, nil
			case "pageInfo":
				var sc, ec any
				if len(c.items) > 0 {
					sc, ec = cursorFor(c.start), cursorFor(c.start+len(c.items)-1)
				}
				return gobj{"PageInfo", map[string]any{"hasNextPage": c.hasNext, "hasPreviousPage": c.hasPrev, "startCursor": sc, "endCursor": ec}}, nil
			}
		}
	}
	return nil, nil
}

// typeField answers a __Type field; the value is a type expression such as "Issue", "[Label]" or "ID!".
func (e *gqlExec) typeField(expr, name string, args map[string]any) (any, error) {
	if strings.HasSuffix(expr, "!") {
		switch name {
		case "kind":
			return "NON_NULL", nil
		case "ofType":
			return gobj{"__Type", strings.TrimSuffix(expr, "!")}, nil
		}
		return nil, nil
	}
	if strings.HasPrefix(expr, "[") {
		switch name {
		case "kind":
			return "LIST", nil
		case "ofType":
			return gobj{"__Type", expr[1 : len(expr)-1]}, nil
		}
		return nil, nil
	}
	d := gqlTypes[expr]
	if d == nil {
		return nil, nil
	}
	switch name {
	case "name":
		return d.Name, nil
	case "kind":
		return d.Kind, nil
	case "description", "ofType", "specifiedByURL":
		return nil, nil
	case "fields":
		if d.Kind != "OBJECT" && d.Kind != "INTERFACE" {
			return nil, nil
		}
		out := make([]any, len(d.Fields))
		for i, f := range d.Fields {
			out[i] = gobj{"__Field", f}
		}
		return out, nil
	case "enumValues":
		if d.Kind != "ENUM" {
			return nil, nil
		}
		out := make([]any, len(d.Enum))
		for i, v := range d.Enum {
			out[i] = gobj{"__EnumValue", v}
		}
		return out, nil
	case "interfaces":
		if d.Kind != "OBJECT" {
			return nil, nil
		}
		out := []any{}
		for _, n := range d.Interfaces {
			out = append(out, gobj{"__Type", n})
		}
		return out, nil
	case "possibleTypes":
		if d.Kind != "INTERFACE" && d.Kind != "UNION" {
			return nil, nil
		}
		out := []any{}
		for _, n := range d.Possible {
			out = append(out, gobj{"__Type", n})
		}
		return out, nil
	case "inputFields":
		return nil, nil
	}
	return nil, nil
}

// ---------------------------------------------------------------------------------------------
// mutations

func (e *gqlExec) mutation(name string, args map[string]any) (any, error) {
	st := e.s.st
	in := argObj(args, "input")
	if in == nil {
		return nil, gerr("", "Argument 'input' on Field '%s' has an invalid value. Expected type '%sInput!'.", name, strings.ToUpper(name[:1])+name[1:])
	}
	cmid := in["clientMutationId"]
	payload := func(T string, fields map[string]any) any {
		fields["clientMutationId"] = cmid
		return gobj{T, fields}
	}
	nodeArg := func(key, want string) (gobj, error) {
		id, _ := in[key].(string)
		n, ok := e.nodeByID(id)
		if !ok || (want != "" && n.T != want) {
			return gobj{}, notResolved(id)
		}
		return n, nil
	}
	switch name {
	case "createIssue":
		ro, err := nodeArg("repositoryId", "Repository")
		if err != nil {
			return nil, err
		}
		repo := ro.V.(*Repo)
		title, _ := in["title"].(string)
		if strings.TrimSpace(title) == "" {
			return nil, gerr("UNPROCESSABLE", "Title can't be blank")
		}
		body, _ := in["body"].(string)
		var labels []string
		if ids, ok := in["labelIds"].([]any); ok {
			st.mu.RLock()
			for _, x := range ids {
				id, _ := x.(string)
				_, n, ok := decodeNodeID(id)
				found := false
				for _, l := range repo.Labels {
					if ok && l.ID == n {
						labels = append(labels, l.Name)
						found = true
					}
				}
				if !found {
					st.mu.RUnlock()
					return nil, notResolved(id)
				}
			}
			st.mu.RUnlock()
		}
		is, err := st.createIssue(repo, title, body, labels)
		if err != nil {
			return nil, gerr("UNPROCESSABLE", "%s", err.Error())
		}
		return payload("CreateIssuePayload", map[string]any{"issue": gobj{"Issue", &issueV{repo, is}}}), nil
	case "closeIssue", "reopenIssue":
		io, err := nodeArg("issueId", "Issue")
		if err != nil {
			return nil, err
		}
		state, T := "closed", "CloseIssuePayload"
		if name == "reopenIssue" {
			state, T = "open", "ReopenIssuePayload"
		}
		st.setIssueState(io.V.(*issueV).is, state)
		return payload(T, map[string]any{"issue": io}), nil
	case "updateIssue":
		io, err := nodeArg("id", "Issue")
		if err != nil {
			return nil, err
		}
		is := io.V.(*issueV).is
		st.mu.Lock()
		if t, ok := in["title"].(string); ok && t != "" {
			is.Title = t
		}
		if b, ok := in["body"].(string); ok {
			is.Body = b
		}
		is.UpdatedAt = time.Now().UTC()
		st.mu.Unlock()
		if s, ok := in["state"].(string); ok {
			st.setIssueState(is, strings.ToLower(s))
		}
		return payload("UpdateIssuePayload", map[string]any{"issue": io}), nil
	case "addComment":
		so, err := nodeArg("subjectId", "")
		if err != nil {
			return nil, err
		}
		body, _ := in["body"].(string)
		if body == "" {
			return nil, gerr("UNPROCESSABLE", "Body can't be blank")
		}
		var c *Comment
		var owner *issueV
		switch v := so.V.(type) {
		case *issueV:
			c = st.addComment(&v.is.Comments, body)
			owner = v
		case *pullV:
			c = st.addComment(&v.p.Comments, body)
		default:
			return nil, notResolved(fmt.Sprint(in["subjectId"]))
		}
		node := gobj{"IssueComment", &commentV{c: c, issue: owner}}
		return payload("AddCommentPayload", map[string]any{
			"commentEdge": gobj{"IssueCommentEdge", map[string]any{"cursor": cursorFor(0), "node": node}},
			"subject":     so,
		}), nil
	case "createPullRequest":
		ro, err := nodeArg("repositoryId", "Repository")
		if err != nil {
			return nil, err
		}
		repo := ro.V.(*Repo)
		title, _ := in["title"].(string)
		body, _ := in["body"].(string)
		head, _ := in["headRefName"].(string)
		baseRef, _ := in["baseRefName"].(string)
		if title == "" {
			title = head
		}
		p, err := st.createPull(repo, title, body, head, baseRef)
		if err != nil {
			return nil, gerr("UNPROCESSABLE", "%s", err.Error())
		}
		return payload("CreatePullRequestPayload", map[string]any{"pullRequest": gobj{"PullRequest", &pullV{repo, p}}}), nil
	case "mergePullRequest":
		po, err := nodeArg("pullRequestId", "PullRequest")
		if err != nil {
			return nil, err
		}
		pv := po.V.(*pullV)
		title, _ := in["commitHeadline"].(string)
		body, _ := in["commitBody"].(string)
		if _, err := st.mergePull(pv.repo, pv.p, title, body); err != nil {
			return nil, gerr("UNPROCESSABLE", "Pull Request is not mergeable")
		}
		return payload("MergePullRequestPayload", map[string]any{"pullRequest": po}), nil
	case "closePullRequest", "reopenPullRequest":
		po, err := nodeArg("pullRequestId", "PullRequest")
		if err != nil {
			return nil, err
		}
		pv := po.V.(*pullV)
		state, T := "closed", "ClosePullRequestPayload"
		if name == "reopenPullRequest" {
			state, T = "open", "ReopenPullRequestPayload"
		}
		if !pv.p.Merged {
			st.setPullState(pv.repo, pv.p, state)
		}
		return payload(T, map[string]any{"pullRequest": po}), nil
	case "updatePullRequest":
		po, err := nodeArg("pullRequestId", "PullRequest")
		if err != nil {
			return nil, err
		}
		pv := po.V.(*pullV)
		st.mu.Lock()
		if t, ok := in["title"].(string); ok && t != "" {
			pv.p.Title = t
		}
		if b, ok := in["body"].(string); ok {
			pv.p.Body = b
		}
		st.mu.Unlock()
		return payload("UpdatePullRequestPayload", map[string]any{"pullRequest": po}), nil
	case "createRepository":
		nm, _ := in["name"].(string)
		if !validRepoName(nm) {
			return nil, gerr("UNPROCESSABLE", "Name is invalid")
		}
		desc, _ := in["description"].(string)
		vis, _ := in["visibility"].(string)
		repo, err := st.createRepo(nm, desc, vis != "PUBLIC")
		if err != nil {
			return nil, gerr("UNPROCESSABLE", "Name already exists on this account")
		}
		return payload("CreateRepositoryPayload", map[string]any{"repository": gobj{"Repository", repo}}), nil
	case "deleteRepository":
		ro, err := nodeArg("repositoryId", "Repository")
		if err != nil {
			return nil, err
		}
		st.deleteRepo(ro.V.(*Repo))
		return payload("DeleteRepositoryPayload", map[string]any{}), nil
	case "createRef":
		ro, err := nodeArg("repositoryId", "Repository")
		if err != nil {
			return nil, err
		}
		repo := ro.V.(*Repo)
		nm, _ := in["name"].(string)
		oidStr, _ := in["oid"].(string)
		o, ok := parseOid(strings.ToLower(oidStr))
		if !ok || !st.db.Has(o) || !strings.HasPrefix(nm, "refs/") {
			return nil, gerr("UNPROCESSABLE", "Invalid ref or object")
		}
		st.mu.Lock()
		if _, exists := repo.Refs[nm]; exists {
			st.mu.Unlock()
			return nil, gerr("UNPROCESSABLE", "A ref named \"%s\" already exists in the repository.", nm)
		}
		st.setRefLocked(repo, nm, o)
		st.mu.Unlock()
		return payload("CreateRefPayload", map[string]any{"ref": gobj{"Ref", &refV{repo, nm, o}}}), nil
	}
	return nil, nil
}

// ---------------------------------------------------------------------------------------------
// HTTP

type gqlRequest struct {
	Query         string          `json:"query"`
	Variables     json.RawMessage `json:"variables"`
	OperationName string          `json:"operationName"`
}

// normalize turns json.Number values into ints (or floats) throughout a decoded value.
func normalize(v any) any {
	switch t := v.(type) {
	case json.Number:
		if n, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return int(n)
		}
		f, _ := t.Float64()
		return f
	case []any:
		for i := range t {
			t[i] = normalize(t[i])
		}
	case map[string]any:
		for k := range t {
			t[k] = normalize(t[k])
		}
	}
	return v
}

func decodeVariables(raw json.RawMessage) (map[string]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		if strings.TrimSpace(s) == "" {
			return map[string]any{}, nil
		}
		raw = []byte(s)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	normalize(m)
	return m, nil
}

func (s *Server) runGraphQL(r *http.Request, req gqlRequest) map[string]any {
	if strings.TrimSpace(req.Query) == "" {
		return map[string]any{"errors": []any{map[string]any{"message": "A query attribute must be specified and must be a string."}}}
	}
	vars, err := decodeVariables(req.Variables)
	if err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": "Variables are invalid JSON."}}}
	}
	doc, err := parseGraphQL(req.Query)
	if err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": err.Error()}}}
	}
	var op *gqlOp
	if req.OperationName != "" {
		for _, o := range doc.ops {
			if o.name == req.OperationName {
				op = o
			}
		}
		if op == nil {
			return map[string]any{"errors": []any{map[string]any{"message": "No operation named \"" + req.OperationName + "\""}}}
		}
	} else if len(doc.ops) == 1 {
		op = doc.ops[0]
	} else {
		return map[string]any{"errors": []any{map[string]any{"message": "An operation name is required when the document holds several operations"}}}
	}
	for _, vd := range op.vars {
		if v, ok := vars[vd.name]; !ok || v == nil {
			if vd.hasDef {
				ex := &gqlExec{}
				vars[vd.name] = ex.resolveValue(vd.def)
			}
		}
	}
	e := &gqlExec{s: s, r: r, doc: doc, vars: vars}
	root := "Query"
	switch op.kind {
	case "mutation":
		root = "Mutation"
	case "subscription":
		return map[string]any{"errors": []any{map[string]any{"message": "Subscriptions are not supported"}}}
	}
	data := e.selectObject(gobj{root, nil}, op.sel, nil)
	out := map[string]any{}
	if e.invalid {
		out["errors"] = e.errors
		return out
	}
	out["data"] = data
	if len(e.errors) > 0 {
		out["errors"] = e.errors
	}
	return out
}

func (s *Server) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	var body []byte
	switch r.Method {
	case http.MethodPost:
		b, err := readBody(r)
		if err != nil {
			msg(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		body = bytes.TrimSpace(b)
	case http.MethodGet:
		q := r.URL.Query()
		req := gqlRequest{Query: q.Get("query"), OperationName: q.Get("operationName")}
		if v := q.Get("variables"); v != "" {
			req.Variables = json.RawMessage(v)
		}
		writeJSON(w, http.StatusOK, s.runGraphQL(r, req))
		return
	default:
		msg(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(body) > 0 && body[0] == '[' {
		var reqs []gqlRequest
		if err := json.Unmarshal(body, &reqs); err != nil {
			msg(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		out := make([]any, len(reqs))
		for i, req := range reqs {
			out[i] = s.runGraphQL(r, req)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var req gqlRequest
	if err := json.Unmarshal(body, &req); err != nil {
		msg(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	writeJSON(w, http.StatusOK, s.runGraphQL(r, req))
}
