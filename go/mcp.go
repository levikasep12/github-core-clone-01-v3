// MCP over Streamable HTTP at POST /mcp.
//
// Two protocol eras are answered on the same endpoint, both with plain JSON
// bodies (the transport would also accept SSE, but JSON is the simpler half
// and the one the suite uses):
//
//   - the modern era, `server/discover`, protocol revision 2026-07-28, no
//     handshake, no session;
//   - the legacy era, `initialize` followed by `notifications/initialized`,
//     revision 2025-11-25.
//
// The six tools read and write the same in-memory store as REST and GraphQL,
// so a repository created through an MCP tool shows up on /api/v3 as the same
// object, and vice versa.

package main

import (
	"regexp"
	"sort"
	"strconv"
)

const mcpName = "github-core-clone-forge"
const mcpVersion = "1.0.0"
const newRevision = "2026-07-28"
const oldRevision = "2025-11-25"

// ---------------------------------------------------------------------------
// Tool definitions (key order is wire-visible; the suite compares two
// tools/list answers byte for byte)
// ---------------------------------------------------------------------------

type toolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type inputSchema struct {
	Type       string   `json:"type"`
	Properties *Obj     `json:"properties"`
	Required   []string `json:"required"`
}

// toolProp is one input-schema property: type, description, then an optional
// enum, in that order.
func toolProp(typ, description string, enumValues ...string) *Obj {
	prop := NewObj()
	prop.Set("type", typ)
	prop.Set("description", description)
	if len(enumValues) > 0 {
		prop.Set("enum", enumValues)
	}
	return prop
}

func defineTool(name, description string, props *Obj, required []string) toolDef {
	return toolDef{
		Name:        name,
		Description: description,
		InputSchema: inputSchema{Type: "object", Properties: props, Required: required},
	}
}

var toolDefs = []toolDef{
	defineTool("create_repo", "Create a new repository owned by the authenticated user.", func() *Obj {
		props := NewObj()
		props.Set("name", toolProp("string", "The repository name."))
		props.Set("description", toolProp("string", "A short description of the repository."))
		props.Set("private", toolProp("boolean", "Whether the repository is private."))
		return props
	}(), []string{"name"}),
	defineTool("get_repo", "Get a repository by owner and name.", func() *Obj {
		props := NewObj()
		props.Set("owner", toolProp("string", "The repository owner login."))
		props.Set("repo", toolProp("string", "The repository name."))
		return props
	}(), []string{"owner", "repo"}),
	defineTool("create_issue", "Create an issue in a repository.", func() *Obj {
		props := NewObj()
		props.Set("owner", toolProp("string", "The repository owner login."))
		props.Set("repo", toolProp("string", "The repository name."))
		props.Set("title", toolProp("string", "The issue title."))
		props.Set("body", toolProp("string", "The issue body."))
		return props
	}(), []string{"owner", "repo", "title"}),
	defineTool("list_issues", "List the issues of a repository.", func() *Obj {
		props := NewObj()
		props.Set("owner", toolProp("string", "The repository owner login."))
		props.Set("repo", toolProp("string", "The repository name."))
		props.Set("state", toolProp("string", "Filter by state: open, closed or all.", "open", "closed", "all"))
		return props
	}(), []string{"owner", "repo"}),
	defineTool("create_pull_request", "Open a pull request from a head branch into a base branch.", func() *Obj {
		props := NewObj()
		props.Set("owner", toolProp("string", "The repository owner login."))
		props.Set("repo", toolProp("string", "The repository name."))
		props.Set("title", toolProp("string", "The pull request title."))
		props.Set("body", toolProp("string", "The pull request body."))
		props.Set("head", toolProp("string", "The head branch name."))
		props.Set("base", toolProp("string", "The base branch name."))
		return props
	}(), []string{"owner", "repo", "title", "head", "base"}),
	defineTool("merge_pull_request", "Merge a pull request with a squash merge.", func() *Obj {
		props := NewObj()
		props.Set("owner", toolProp("string", "The repository owner login."))
		props.Set("repo", toolProp("string", "The repository name."))
		props.Set("pull_number", toolProp("number", "The pull request number."))
		return props
	}(), []string{"owner", "repo", "pull_number"}),
}

var toolNames = map[string]bool{
	"create_repo":         true,
	"get_repo":            true,
	"create_issue":        true,
	"list_issues":         true,
	"create_pull_request": true,
	"merge_pull_request":  true,
}

// ---------------------------------------------------------------------------
// JSON-RPC replies
// ---------------------------------------------------------------------------

type rpcResult struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result"`
}

type rpcErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcFault struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      any          `json:"id"`
	Error   rpcErrorBody `json:"error"`
}

// answer is a successful JSON-RPC reply.
func answer(id any, result any) Res {
	return jsonRes(200, rpcResult{JSONRPC: "2.0", ID: id, Result: result})
}

// fault is a JSON-RPC error reply; the HTTP status is 200 unless the caller
// says otherwise.
func fault(id any, code int, message string, status ...int) Res {
	code200 := 200
	if len(status) > 0 {
		code200 = status[0]
	}
	return jsonRes(code200, rpcFault{JSONRPC: "2.0", ID: id, Error: rpcErrorBody{Code: code, Message: message}})
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolOkResult struct {
	Content           []textContent `json:"content"`
	StructuredContent any           `json:"structuredContent"`
}

type toolFailResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
}

// okResult is a tool's success payload: human-readable text plus the
// structured value.
func okResult(value any) toolOkResult {
	text, ok := value.(string)
	if !ok {
		text = string(marshalJSON(value))
	}
	return toolOkResult{Content: []textContent{{Type: "text", Text: text}}, StructuredContent: value}
}

// failResult is a tool's failure payload: text only, marked as an error.
func failResult(message string) toolFailResult {
	return toolFailResult{Content: []textContent{{Type: "text", Text: message}}, IsError: true}
}

// ---------------------------------------------------------------------------
// Argument picking
// ---------------------------------------------------------------------------

// pickString is the first of keys whose argument is a non-empty string.
func pickString(args map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := args[key].(string); ok && value != "" {
			return value, true
		}
	}
	return "", false
}

var digitsPattern = regexp.MustCompile(`^\d+$`)

// pickNumber is the first of keys whose argument is an integer (a numeric
// string counts).
func pickNumber(args map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		value, ok := args[key]
		if !ok {
			continue
		}
		switch v := value.(type) {
		case float64:
			if v == float64(int64(v)) {
				return int(v), true
			}
		case string:
			if digitsPattern.MatchString(v) {
				n, err := strconv.Atoi(v)
				if err == nil {
					return n, true
				}
			}
		}
	}
	return 0, false
}

// lookupRepo looks a repository up by the caller's owner/repo arguments.
// missingMessage is the exact text this tool answers when owner or repo is
// absent (each tool phrases it differently); a repository that does not exist
// is always `Repository <owner>/<name> not found`.
func lookupRepo(forge *Forge, args map[string]any, missingMessage string) (*Repo, string, string, string) {
	owner, hasOwner := pickString(args, "owner", "owner_login", "login")
	name, hasName := pickString(args, "repo", "repository", "name")
	if !hasOwner || !hasName {
		return nil, "", "", missingMessage
	}
	repo := forge.GetRepo(owner, name)
	if repo == nil {
		return nil, "", "", "Repository " + owner + "/" + name + " not found"
	}
	return repo, owner, name, ""
}

func runTool(forge *Forge, name string, args map[string]any) any {
	switch name {
	case "create_repo":
		{
			repoName, ok := pickString(args, "name", "repo", "repository")
			if !ok {
				return failResult("create_repo: name is required")
			}
			if forge.GetRepo(forge.Viewer.Login, repoName) != nil {
				return failResult("Repository " + forge.Viewer.Login + "/" + repoName + " already exists")
			}
			description, _ := pickString(args, "description", "about")
			private, _ := args["private"].(bool)
			repo := forge.CreateRepo(repoName, description, private)
			return okResult(RepoJSON(forge, repo))
		}
	case "get_repo":
		{
			repo, _, _, errMsg := lookupRepo(forge, args, "get_repo: owner and repo are required")
			if errMsg != "" {
				return failResult(errMsg)
			}
			return okResult(RepoJSON(forge, repo))
		}
	case "create_issue":
		{
			repo, _, _, errMsg := lookupRepo(forge, args, "create_issue: owner, repo and title are required")
			if errMsg != "" {
				return failResult(errMsg)
			}
			title, ok := pickString(args, "title", "issue_title")
			if !ok {
				return failResult("create_issue: owner, repo and title are required")
			}
			body, _ := pickString(args, "body", "issue_body", "description")
			created := CreateIssue(forge, repo, title, body, nil)
			if !created.OK {
				return failResult(created.Message)
			}
			return okResult(IssueJSON(forge, repo, created.Issue))
		}
	case "list_issues":
		{
			repo, _, _, errMsg := lookupRepo(forge, args, "list_issues: owner and repo are required")
			if errMsg != "" {
				return failResult(errMsg)
			}
			state, _ := pickString(args, "state", "status")
			if state == "" {
				state = "open"
			}
			issues := make([]*Issue, 0, len(repo.Issues))
			for _, issue := range repo.Issues {
				issues = append(issues, issue)
			}
			var filtered []*Issue
			for _, issue := range issues {
				if state == "open" && issue.State != "open" {
					continue
				}
				if state == "closed" && issue.State != "closed" {
					continue
				}
				filtered = append(filtered, issue)
			}
			sort.Slice(filtered, func(i, j int) bool { return filtered[i].Seq < filtered[j].Seq })
			list := make([]issueJSON, 0, len(filtered))
			for _, issue := range filtered {
				list = append(list, IssueJSON(forge, repo, issue))
			}
			issuesObj := NewObj()
			issuesObj.Set("issues", list)
			return okResult(issuesObj)
		}
	case "create_pull_request":
		{
			repo, _, _, errMsg := lookupRepo(forge, args, "create_pull_request: owner, repo, title, head and base are required")
			if errMsg != "" {
				return failResult(errMsg)
			}
			title, hasTitle := pickString(args, "title", "pr_title")
			head, hasHead := pickString(args, "head", "head_ref", "head_branch", "source_branch")
			base, hasBase := pickString(args, "base", "base_ref", "base_branch", "target_branch")
			if !hasTitle || !hasHead || !hasBase {
				return failResult("create_pull_request: owner, repo, title, head and base are required")
			}
			body, _ := pickString(args, "body", "description")
			created := CreatePull(forge, repo, title, body, head, base)
			if !created.OK {
				return failResult(created.Message)
			}
			return okResult(PullJSON(forge, repo, created.Pull))
		}
	case "merge_pull_request":
		{
			repo, _, _, errMsg := lookupRepo(forge, args, "merge_pull_request: owner, repo and pull_number are required")
			if errMsg != "" {
				return failResult(errMsg)
			}
			number, ok := pickNumber(args, "pull_number", "pull_request_number", "pr_number", "number")
			if !ok {
				return failResult("merge_pull_request: owner, repo and pull_number are required")
			}
			pull := repo.Pulls[number]
			if pull == nil {
				return failResult("Pull request #" + itoa(number) + " not found")
			}
			merged := MergePull(repo, pull)
			if !merged.OK {
				return failResult(merged.Message)
			}
			// {merged, sha, number, ...pullJson}: the spread overwrites merged and
			// number with the same values, so the order is merged, sha, number,
			// then the pull's fields.
			pj := PullJSON(forge, repo, pull)
			out := NewObj()
			out.Set("merged", true)
			out.Set("sha", merged.SHA)
			out.Set("number", pull.Number)
			out.Set("id", pj.ID)
			out.Set("title", pj.Title)
			out.Set("body", pj.Body)
			out.Set("state", pj.State)
			out.Set("mergeable", pj.Mergeable)
			out.Set("merge_commit_sha", pj.MergeCommitSHA)
			out.Set("merged_at", pj.MergedAt)
			out.Set("head", pj.Head)
			out.Set("base", pj.Base)
			out.Set("user", pj.User)
			out.Set("created_at", pj.CreatedAt)
			out.Set("updated_at", pj.UpdatedAt)
			return okResult(out)
		}
	default:
		return failResult("Unknown tool: " + name)
	}
}

var revisionPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// HandleMcp handles one POST /mcp request (bearer auth is enforced by the
// router, not here).
func HandleMcp(forge *Forge, req *Req) Res {
	if req.JSONParseError {
		return fault(nil, -32700, "Parse error", 400)
	}
	body, ok := req.JSON.(map[string]any)
	if !ok {
		return fault(nil, -32600, "Invalid Request")
	}
	method, isString := body["method"].(string)
	var id any
	if raw, present := body["id"]; present {
		switch v := raw.(type) {
		case string, float64:
			id = v
		default:
			id = nil
		}
	}
	if !isString {
		return fault(id, -32600, "Invalid Request: method is required")
	}

	// Notifications carry no id and get no JSON-RPC answer — 202 either way.
	if _, present := body["id"]; !present {
		return Res{Status: 202}
	}

	switch method {
	case "server/discover":
		{
			serverInfo := NewObj()
			serverInfo.Set("name", mcpName)
			serverInfo.Set("version", mcpVersion)
			metaInfo := NewObj()
			metaInfo.Set("name", mcpName)
			metaInfo.Set("version", mcpVersion)
			meta := NewObj()
			meta.Set("io.modelcontextprotocol/serverInfo", metaInfo)
			capabilities := NewObj()
			capabilities.Set("tools", NewObj())
			result := NewObj()
			result.Set("resultType", "discover")
			result.Set("supportedVersions", []string{newRevision})
			result.Set("capabilities", capabilities)
			result.Set("serverInfo", serverInfo)
			result.Set("_meta", meta)
			return answer(id, result)
		}
	case "initialize":
		{
			params, _ := body["params"].(map[string]any)
			offered, _ := params["protocolVersion"].(string)
			if offered == "" {
				offered = oldRevision
			}
			negotiated := offered
			if !revisionPattern.MatchString(offered) {
				negotiated = oldRevision
			}
			serverInfo := NewObj()
			serverInfo.Set("name", mcpName)
			serverInfo.Set("version", mcpVersion)
			capabilities := NewObj()
			capabilities.Set("tools", NewObj())
			result := NewObj()
			result.Set("protocolVersion", negotiated)
			result.Set("capabilities", capabilities)
			result.Set("serverInfo", serverInfo)
			return answer(id, result)
		}
	case "ping":
		{
			return answer(id, NewObj())
		}
	case "tools/list":
		{
			result := NewObj()
			result.Set("tools", toolDefs)
			return answer(id, result)
		}
	case "tools/call":
		{
			params, _ := body["params"].(map[string]any)
			toolName, _ := params["name"].(string)
			if !toolNames[toolName] {
				return fault(id, -32601, "Unknown tool: "+toolName)
			}
			args, _ := params["arguments"].(map[string]any)
			if args == nil {
				args = map[string]any{}
			}
			return answer(id, runTool(forge, toolName, args))
		}
	default:
		return fault(id, -32601, "Method not found: "+method)
	}
}
