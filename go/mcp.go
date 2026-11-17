package main

// Model Context Protocol at /mcp: JSON-RPC 2.0 over Streamable HTTP (plain JSON answers). It speaks the
// 2026-07-28 revision (`server/discover`, no handshake) and the handshake revisions before it
// (`initialize`). Every tool runs through the REST handler, so an MCP object is the REST object.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
)

var mcpVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

var mcpServerInfo = map[string]any{"name": "forge-mcp", "title": "Forge MCP", "version": "1.0.0"}

type mcpProp struct {
	name, typ, desc string
	enum            []string
}

type mcpTool struct {
	name     string
	desc     string
	props    []mcpProp
	required []string
	readOnly bool
	run      func(s *Server, r *http.Request, a mcpArgs) (int, []byte)
}

type mcpArgs map[string]any

func (a mcpArgs) str(k string) string {
	switch v := a[k].(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func (a mcpArgs) has(k string) bool {
	v, ok := a[k]
	return ok && v != nil && v != ""
}

func seg(s string) string { return url.PathEscape(s) }

func repoPath(a mcpArgs) string { return "/repos/" + seg(a.str("owner")) + "/" + seg(a.str("repo")) }

// rest runs one REST call in-process and answers its status and body.
func (s *Server) rest(r *http.Request, method, path string, query url.Values, body any) (int, []byte) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	u := "/api/v3" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(r.Context(), method, u, rd)
	if err != nil {
		return 500, []byte(`{"message":"bad request"}`)
	}
	req.Host = r.Host
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.serveREST(rec, req, path)
	return rec.Code, rec.Body.Bytes()
}

func pageQuery(a mcpArgs) url.Values {
	q := url.Values{}
	if a.has("page") {
		q.Set("page", a.str("page"))
	}
	if a.has("per_page") {
		q.Set("per_page", a.str("per_page"))
	}
	return q
}

var ownerProp = mcpProp{"owner", "string", "Repository owner (the account login)", nil}
var repoProp = mcpProp{"repo", "string", "Repository name", nil}
var pageProps = []mcpProp{{"page", "number", "Page number for pagination (1-based)", nil}, {"per_page", "number", "Results per page (max 100)", nil}}

var mcpTools = []*mcpTool{
	{
		name: "create_repo", desc: "Create a new repository owned by the authenticated user.",
		props: []mcpProp{
			{"name", "string", "Repository name", nil},
			{"description", "string", "Repository description", nil},
			{"private", "boolean", "Whether the repository is private", nil},
			{"auto_init", "boolean", "Create an initial commit with a README", nil},
		},
		required: []string{"name"},
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			body := map[string]any{"name": a.str("name"), "description": a.str("description")}
			if v, ok := a["private"].(bool); ok {
				body["private"] = v
			}
			if v, ok := a["auto_init"].(bool); ok {
				body["auto_init"] = v
			}
			return s.rest(r, "POST", "/user/repos", nil, body)
		},
	},
	{
		name: "get_repo", desc: "Get a repository's details: name, full_name, description, visibility and default branch.",
		props: []mcpProp{ownerProp, repoProp}, required: []string{"owner", "repo"}, readOnly: true,
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			return s.rest(r, "GET", repoPath(a), nil, nil)
		},
	},
	{
		name: "list_repos", desc: "List the authenticated user's repositories.",
		props: pageProps, readOnly: true,
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			return s.rest(r, "GET", "/user/repos", pageQuery(a), nil)
		},
	},
	{
		name: "create_issue", desc: "Create a new issue in a repository.",
		props: []mcpProp{ownerProp, repoProp,
			{"title", "string", "Issue title", nil},
			{"body", "string", "Issue body (Markdown)", nil},
			{"labels", "array", "Label names to apply (each must exist in the repository)", nil},
		},
		required: []string{"owner", "repo", "title"},
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			body := map[string]any{"title": a.str("title"), "body": a.str("body")}
			if l, ok := a["labels"]; ok && l != nil {
				body["labels"] = l
			}
			return s.rest(r, "POST", repoPath(a)+"/issues", nil, body)
		},
	},
	{
		name: "get_issue", desc: "Get one issue by its number.",
		props: []mcpProp{ownerProp, repoProp, {"issue_number", "number", "Issue number", nil}},
		required: []string{"owner", "repo", "issue_number"}, readOnly: true,
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			return s.rest(r, "GET", repoPath(a)+"/issues/"+a.str("issue_number"), nil, nil)
		},
	},
	{
		name: "list_issues", desc: "List a repository's issues, filtered by state.",
		props: append([]mcpProp{ownerProp, repoProp,
			{"state", "string", "Filter by state", []string{"open", "closed", "all"}},
			{"labels", "string", "Comma-separated label names to filter by", nil},
			{"direction", "string", "Sort direction", []string{"asc", "desc"}},
		}, pageProps...),
		required: []string{"owner", "repo"}, readOnly: true,
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			q := pageQuery(a)
			for _, k := range []string{"state", "labels", "direction"} {
				if a.has(k) {
					q.Set(k, strings.ToLower(a.str(k)))
				}
			}
			return s.rest(r, "GET", repoPath(a)+"/issues", q, nil)
		},
	},
	{
		name: "update_issue", desc: "Update an issue's title, body or state.",
		props: []mcpProp{ownerProp, repoProp,
			{"issue_number", "number", "Issue number", nil},
			{"title", "string", "New title", nil},
			{"body", "string", "New body", nil},
			{"state", "string", "New state", []string{"open", "closed"}},
		},
		required: []string{"owner", "repo", "issue_number"},
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			body := map[string]any{}
			for _, k := range []string{"title", "body", "state"} {
				if a.has(k) {
					body[k] = a.str(k)
				}
			}
			return s.rest(r, "PATCH", repoPath(a)+"/issues/"+a.str("issue_number"), nil, body)
		},
	},
	{
		name: "add_issue_comment", desc: "Add a comment to an issue or pull request.",
		props: []mcpProp{ownerProp, repoProp, {"issue_number", "number", "Issue or pull request number", nil}, {"body", "string", "Comment text", nil}},
		required: []string{"owner", "repo", "issue_number", "body"},
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			return s.rest(r, "POST", repoPath(a)+"/issues/"+a.str("issue_number")+"/comments", nil, map[string]any{"body": a.str("body")})
		},
	},
	{
		name: "create_pull_request", desc: "Open a pull request from a head branch into a base branch.",
		props: []mcpProp{ownerProp, repoProp,
			{"title", "string", "Pull request title", nil},
			{"head", "string", "The branch holding the changes", nil},
			{"base", "string", "The branch to merge into", nil},
			{"body", "string", "Pull request description", nil},
		},
		required: []string{"owner", "repo", "title", "head", "base"},
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			return s.rest(r, "POST", repoPath(a)+"/pulls", nil, map[string]any{"title": a.str("title"), "head": a.str("head"), "base": a.str("base"), "body": a.str("body")})
		},
	},
	{
		name: "get_pull_request", desc: "Get one pull request by its number.",
		props: []mcpProp{ownerProp, repoProp, {"pull_number", "number", "Pull request number", nil}},
		required: []string{"owner", "repo", "pull_number"}, readOnly: true,
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			return s.rest(r, "GET", repoPath(a)+"/pulls/"+a.str("pull_number"), nil, nil)
		},
	},
	{
		name: "list_pull_requests", desc: "List a repository's pull requests, filtered by state.",
		props: append([]mcpProp{ownerProp, repoProp, {"state", "string", "Filter by state", []string{"open", "closed", "all"}}}, pageProps...),
		required: []string{"owner", "repo"}, readOnly: true,
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			q := pageQuery(a)
			if a.has("state") {
				q.Set("state", strings.ToLower(a.str("state")))
			}
			return s.rest(r, "GET", repoPath(a)+"/pulls", q, nil)
		},
	},
	{
		name: "get_pull_request_files", desc: "List the files a pull request changes.",
		props: []mcpProp{ownerProp, repoProp, {"pull_number", "number", "Pull request number", nil}},
		required: []string{"owner", "repo", "pull_number"}, readOnly: true,
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			return s.rest(r, "GET", repoPath(a)+"/pulls/"+a.str("pull_number")+"/files", nil, nil)
		},
	},
	{
		name: "merge_pull_request", desc: "Merge a pull request into its base branch.",
		props: []mcpProp{ownerProp, repoProp,
			{"pull_number", "number", "Pull request number", nil},
			{"merge_method", "string", "Merge method", []string{"merge", "squash", "rebase"}},
			{"commit_title", "string", "Title of the merge commit", nil},
			{"commit_message", "string", "Extra detail for the merge commit", nil},
		},
		required: []string{"owner", "repo", "pull_number"},
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			body := map[string]any{"merge_method": a.str("merge_method")}
			if a.has("commit_title") {
				body["commit_title"] = a.str("commit_title")
			}
			if a.has("commit_message") {
				body["commit_message"] = a.str("commit_message")
			}
			return s.rest(r, "PUT", repoPath(a)+"/pulls/"+a.str("pull_number")+"/merge", nil, body)
		},
	},
	{
		name: "get_file_contents", desc: "Get a file's contents (base64) or a directory listing at a ref.",
		props: []mcpProp{ownerProp, repoProp, {"path", "string", "Path in the repository", nil}, {"ref", "string", "Branch, tag or commit (default branch when omitted)", nil}},
		required: []string{"owner", "repo"}, readOnly: true,
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			q := url.Values{}
			if a.has("ref") {
				q.Set("ref", a.str("ref"))
			}
			return s.rest(r, "GET", repoPath(a)+"/contents/"+strings.TrimLeft(a.str("path"), "/"), q, nil)
		},
	},
	{
		name: "create_or_update_file", desc: "Create or update one file with a commit (content is plain text; pass sha to update).",
		props: []mcpProp{ownerProp, repoProp,
			{"path", "string", "Path of the file", nil},
			{"content", "string", "New file content (plain text)", nil},
			{"message", "string", "Commit message", nil},
			{"branch", "string", "Branch to commit to (created from the default branch when missing)", nil},
			{"sha", "string", "Blob sha of the file being replaced (required for an update)", nil},
		},
		required: []string{"owner", "repo", "path", "content", "message"},
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			body := map[string]any{"message": a.str("message"), "content": base64.StdEncoding.EncodeToString([]byte(a.str("content")))}
			if a.has("branch") {
				body["branch"] = a.str("branch")
			}
			if a.has("sha") {
				body["sha"] = a.str("sha")
			}
			return s.rest(r, "PUT", repoPath(a)+"/contents/"+strings.TrimLeft(a.str("path"), "/"), nil, body)
		},
	},
	{
		name: "create_branch", desc: "Create a branch from another branch (the default branch when from_branch is omitted).",
		props: []mcpProp{ownerProp, repoProp, {"branch", "string", "New branch name", nil}, {"from_branch", "string", "Branch to start from", nil}},
		required: []string{"owner", "repo", "branch"},
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			repo := s.st.repo(a.str("owner"), a.str("repo"))
			if repo == nil {
				return 404, []byte(`{"message":"Not Found"}`)
			}
			o, ok := s.st.resolveRev(repo, a.str("from_branch"))
			if !ok {
				return 404, []byte(`{"message":"The source branch was not found"}`)
			}
			return s.rest(r, "POST", repoPath(a)+"/git/refs", nil, map[string]any{"ref": "refs/heads/" + a.str("branch"), "sha": o.String()})
		},
	},
	{
		name: "list_branches", desc: "List a repository's branches.",
		props: append([]mcpProp{ownerProp, repoProp}, pageProps...), required: []string{"owner", "repo"}, readOnly: true,
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			return s.rest(r, "GET", repoPath(a)+"/branches", pageQuery(a), nil)
		},
	},
	{
		name: "list_commits", desc: "List commits reachable from a branch or sha, newest first.",
		props: append([]mcpProp{ownerProp, repoProp, {"sha", "string", "Branch name or commit sha (default branch when omitted)", nil}}, pageProps...),
		required: []string{"owner", "repo"}, readOnly: true,
		run: func(s *Server, r *http.Request, a mcpArgs) (int, []byte) {
			q := pageQuery(a)
			if a.has("sha") {
				q.Set("sha", a.str("sha"))
			}
			return s.rest(r, "GET", repoPath(a)+"/commits", q, nil)
		},
	},
}

var mcpToolByName = map[string]*mcpTool{}
var mcpToolsJSON json.RawMessage

func init() {
	list := make([]any, 0, len(mcpTools))
	for _, t := range mcpTools {
		mcpToolByName[t.name] = t
		props := map[string]any{}
		for _, p := range t.props {
			d := map[string]any{"type": p.typ, "description": p.desc}
			if p.enum != nil {
				d["enum"] = p.enum
			}
			if p.typ == "array" {
				d["items"] = map[string]any{"type": "string"}
			}
			props[p.name] = d
		}
		schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(t.required) > 0 {
			schema["required"] = t.required
		}
		list = append(list, map[string]any{
			"name": t.name, "title": strings.ReplaceAll(t.name, "_", " "), "description": t.desc, "inputSchema": schema,
			"annotations": map[string]any{"readOnlyHint": t.readOnly, "openWorldHint": false},
		})
	}
	b, _ := json.Marshal(map[string]any{"tools": list})
	mcpToolsJSON = b
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	var idv any = json.RawMessage("null")
	if len(id) > 0 {
		idv = id
	}
	return map[string]any{"jsonrpc": "2.0", "id": idv, "error": map[string]any{"code": code, "message": message}}
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		writeRawJSON(w, http.StatusNoContent, nil)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, rpcError(nil, -32600, "only POST is served on this endpoint"))
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, rpcError(nil, -32700, "Parse error: the body could not be read"))
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var raws []json.RawMessage
		if err := json.Unmarshal(body, &raws); err != nil {
			writeJSON(w, http.StatusBadRequest, rpcError(nil, -32700, "Parse error: "+err.Error()))
			return
		}
		var out []any
		for _, raw := range raws {
			if resp := s.mcpOne(w, r, raw); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if !json.Valid(body) {
		writeJSON(w, http.StatusBadRequest, rpcError(nil, -32700, "Parse error: the body is not valid JSON"))
		return
	}
	resp := s.mcpOne(w, r, body)
	if resp == nil {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// mcpOne answers one JSON-RPC message (nil for a notification).
func (s *Server) mcpOne(w http.ResponseWriter, r *http.Request, raw json.RawMessage) map[string]any {
	var req rpcReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcError(nil, -32600, "Invalid Request: "+err.Error())
	}
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	if req.Method == "" {
		if isNotification {
			return nil // a response object sent to the server: nothing to answer
		}
		return rpcError(req.ID, -32600, "Invalid Request: method is missing")
	}
	if isNotification {
		return nil
	}
	params := map[string]any{}
	if len(req.Params) > 0 && string(req.Params) != "null" {
		dec := json.NewDecoder(bytes.NewReader(req.Params))
		dec.UseNumber()
		if err := dec.Decode(&params); err != nil {
			return rpcError(req.ID, -32602, "Invalid params: params must be an object")
		}
		normalize(params)
	}
	switch req.Method {
	case "server/discover":
		return rpcResult(req.ID, map[string]any{
			"supportedVersions": mcpVersions,
			"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":        mcpServerInfo,
			"instructions":      "A GitHub-compatible git forge: repositories, issues and pull requests.",
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": mcpServerInfo},
		})
	case "initialize":
		version := "2025-06-18"
		if pv, ok := params["protocolVersion"].(string); ok {
			for _, v := range mcpVersions {
				if v == pv {
					version = pv
				}
			}
		}
		w.Header().Set("Mcp-Session-Id", fmt.Sprintf("forge-%d", s.reqSeq.Load()))
		return rpcResult(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      mcpServerInfo,
			"instructions":    "A GitHub-compatible git forge: repositories, issues and pull requests.",
		})
	case "ping":
		return rpcResult(req.ID, map[string]any{})
	case "tools/list":
		return rpcResult(req.ID, mcpToolsJSON)
	case "resources/list":
		return rpcResult(req.ID, map[string]any{"resources": []any{}})
	case "prompts/list":
		return rpcResult(req.ID, map[string]any{"prompts": []any{}})
	case "tools/call":
		name, _ := params["name"].(string)
		tool := mcpToolByName[name]
		if tool == nil {
			return rpcError(req.ID, -32602, "Unknown tool: "+name)
		}
		args := mcpArgs{}
		if a, ok := params["arguments"].(map[string]any); ok {
			args = a
		}
		var missing []string
		for _, k := range tool.required {
			if !args.has(k) {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return rpcResult(req.ID, toolText(true, "missing required argument(s): "+strings.Join(missing, ", "), nil))
		}
		status, body := tool.run(s, r, args)
		var v any
		json.Unmarshal(body, &v)
		if status >= 400 {
			m := "HTTP " + strconv.Itoa(status)
			if obj, ok := v.(map[string]any); ok {
				if s, ok := obj["message"].(string); ok {
					m += ": " + s
				}
			}
			return rpcResult(req.ID, toolText(true, m, v))
		}
		return rpcResult(req.ID, toolText(false, "", v))
	}
	return rpcError(req.ID, -32601, "Method not found: "+req.Method)
}

// toolText shapes a tool result: the JSON as text, and the object as structuredContent (lists wrapped).
func toolText(isError bool, message string, v any) map[string]any {
	var text string
	var structured any
	if isError {
		text = message
		if v != nil {
			structured = map[string]any{"error": message, "details": v}
		}
	} else {
		b, _ := json.MarshalIndent(v, "", "  ")
		text = string(b)
		switch t := v.(type) {
		case map[string]any:
			structured = t
		case []any:
			structured = map[string]any{"items": t, "total_count": len(t)}
		case nil:
			structured = map[string]any{"ok": true}
			text = `{"ok": true}`
		}
	}
	out := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
	if structured != nil {
		out["structuredContent"] = structured
	}
	return out
}
