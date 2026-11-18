// mcp.go — the Model Context Protocol endpoint: JSON-RPC over POST /mcp, speaking the
// modern server/discover revision and the legacy initialize handshake, with the six
// forge tools the suite drives. Tool calls share the exact store operations REST uses.

package main

import (
	"encoding/json"
	"strconv"
	"sort"
	"strings"
)

var mcpServerInfo = map[string]any{"name": "swiftforge", "version": "1.0.0"}
var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

type mcpToolDef struct {
	name        string
	description string
	properties  []mcpPropDef
	required    []string
}

type mcpPropDef struct {
	name        string
	typ         string
	description string
}

var mcpTools = []mcpToolDef{
	{
		name: "create_repo", description: "Create a new repository for the authenticated user.",
		properties: []mcpPropDef{
			{"name", "string", "The repository name."},
			{"description", "string", "A short description of the repository."},
			{"private", "boolean", "Whether the repository is private."},
		},
		required: []string{"name"},
	},
	{
		name: "get_repo", description: "Read one repository by owner and name.",
		properties: []mcpPropDef{
			{"owner", "string", "The repository owner login."},
			{"repo", "string", "The repository name."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name: "create_issue", description: "Open an issue in a repository.",
		properties: []mcpPropDef{
			{"owner", "string", "The repository owner login."},
			{"repo", "string", "The repository name."},
			{"title", "string", "The issue title."},
			{"body", "string", "The issue body."},
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name: "list_issues", description: "List issues in a repository, optionally filtered by state.",
		properties: []mcpPropDef{
			{"owner", "string", "The repository owner login."},
			{"repo", "string", "The repository name."},
			{"state", "string", "open, closed or all (default open)."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name: "create_pull_request", description: "Open a pull request from a head branch into a base branch.",
		properties: []mcpPropDef{
			{"owner", "string", "The repository owner login."},
			{"repo", "string", "The repository name."},
			{"title", "string", "The pull request title."},
			{"body", "string", "The pull request body."},
			{"head", "string", "The head branch name."},
			{"base", "string", "The base branch name."},
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name: "merge_pull_request", description: "Merge an open pull request.",
		properties: []mcpPropDef{
			{"owner", "string", "The repository owner login."},
			{"repo", "string", "The repository name."},
			{"pull_number", "number", "The pull request number."},
		},
		required: []string{"owner", "repo", "pull_number"},
	},
}

func mcpToolsList() map[string]any {
	tools := make([]any, 0, len(mcpTools))
	for _, tool := range mcpTools {
		props := &jsonBuf{}
		props.raw("{")
		for i, prop := range tool.properties {
			if i > 0 {
				props.raw(",")
			}
			props.key(prop.name).raw("{")
			props.key("type").str(prop.typ).raw(",")
			props.key("description").str(prop.description)
			props.raw("}")
		}
		props.raw("}")
		var propsValue any
		json.Unmarshal(props.bytes(), &propsValue)
		required := make([]any, 0, len(tool.required))
		for _, r := range tool.required {
			required = append(required, r)
		}
		tools = append(tools, map[string]any{
			"name":        tool.name,
			"description": tool.description,
			"inputSchema": map[string]any{"type": "object", "properties": propsValue, "required": required},
		})
	}
	return map[string]any{"tools": tools}
}

type mcpReply struct {
	status int
	body   map[string]any
}

func mcpRPCError(id any, code int, message string) *mcpReply {
	return &mcpReply{status: 200, body: map[string]any{
		"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": float64(code), "message": message},
	}}
}

func mcpRPCResult(id any, result any) *mcpReply {
	return &mcpReply{status: 200, body: map[string]any{"jsonrpc": "2.0", "id": id, "result": result}}
}

func mcpToolResult(id any, valueJSON []byte) *mcpReply {
	var value any
	json.Unmarshal(valueJSON, &value)
	return mcpRPCResult(id, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(valueJSON)}},
		"structuredContent": value,
		"isError":           false,
	})
}

func mcpToolFailure(id any, message string) *mcpReply {
	return mcpRPCResult(id, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": message}},
		"isError": true,
	})
}

func mcpRequireRepo(forge *Forge, args map[string]any) (*RepoRec, error) {
	owner, _ := args["owner"].(string)
	name, _ := args["repo"].(string)
	repo := forge.GetRepo(owner, name)
	if repo == nil {
		return nil, &mcpError{msg: "repository " + owner + "/" + name + " not found"}
	}
	return repo, nil
}

type mcpError struct{ msg string }

func (e *mcpError) Error() string { return e.msg }

func mcpCallTool(forge *Forge, id any, name string, args map[string]any) *mcpReply {
	var tool *mcpToolDef
	for i := range mcpTools {
		if mcpTools[i].name == name {
			tool = &mcpTools[i]
			break
		}
	}
	if tool == nil {
		return mcpRPCError(id, -32601, "tool not found: "+name)
	}
	for _, required := range tool.required {
		v, present := args[required]
		if !present || v == nil || v == "" {
			return mcpRPCError(id, -32602, "missing required argument: "+required)
		}
	}
	fail := func(err error) *mcpReply { return mcpToolFailure(id, err.Error()) }
	switch tool.name {
	case "create_repo":
		repoName, _ := args["name"].(string)
		if forge.GetRepo(ViewerLogin, repoName) != nil {
			return mcpToolFailure(id, "repository "+repoName+" already exists")
		}
		description, _ := args["description"].(string)
		isPrivate := false
		if raw, present := args["private"]; present {
			isPrivate = jsTruthy(raw)
		}
		repo := forge.CreateRepo(ViewerLogin, repoName, description, isPrivate)
		return mcpToolResult(id, repoJSONBytes(repo))
	case "get_repo":
		repo, err := mcpRequireRepo(forge, args)
		if err != nil {
			return fail(err)
		}
		return mcpToolResult(id, repoJSONBytes(repo))
	case "create_issue":
		repo, err := mcpRequireRepo(forge, args)
		if err != nil {
			return fail(err)
		}
		title, _ := args["title"].(string)
		body, _ := args["body"].(string)
		issue := forge.CreateIssue(repo, title, body, nil)
		return mcpToolResult(id, issueJSONBytes(repo, issue))
	case "list_issues":
		repo, err := mcpRequireRepo(forge, args)
		if err != nil {
			return fail(err)
		}
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		var issues []*IssueRec
		for _, issue := range repo.Issues {
			issues = append(issues, issue)
		}
		if state == "open" || state == "closed" {
			filtered := issues[:0]
			for _, issue := range issues {
				if issue.State == state {
					filtered = append(filtered, issue)
				}
			}
			issues = filtered
		}
		sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
		b := &jsonBuf{}
		b.raw("[")
		for i, issue := range issues {
			if i > 0 {
				b.raw(",")
			}
			issueJSON(b, repo, issue)
		}
		b.raw("]")
		return mcpToolResult(id, b.bytes())
	case "create_pull_request":
		repo, err := mcpRequireRepo(forge, args)
		if err != nil {
			return fail(err)
		}
		headRaw, _ := args["head"].(string)
		baseRaw, _ := args["base"].(string)
		head := headRaw[strings.LastIndex(headRaw, ":")+1:]
		base := baseRaw[strings.LastIndex(baseRaw, ":")+1:]
		headSha, hasHead := repo.Refs["refs/heads/"+head]
		baseSha, hasBase := repo.Refs["refs/heads/"+base]
		if !hasHead || !hasBase {
			return mcpToolFailure(id, "head or base branch does not exist")
		}
		headCommit := forge.PeelToCommit(headSha)
		baseCommit := forge.PeelToCommit(baseSha)
		if headCommit == "" || baseCommit == "" || headCommit == baseCommit || forge.IsAncestor(headCommit, baseCommit) {
			return mcpToolFailure(id, "No commits between the base and the head")
		}
		title, _ := args["title"].(string)
		body, _ := args["body"].(string)
		pull := forge.CreatePull(repo, title, body, head, base, headSha, baseSha)
		return mcpToolResult(id, pullJSONBytes(repo, pull))
	case "merge_pull_request":
		repo, err := mcpRequireRepo(forge, args)
		if err != nil {
			return fail(err)
		}
		number := 0
		if n, ok := args["pull_number"].(float64); ok {
			number = int(n)
		}
		pull := repo.Pulls[number]
		if pull == nil {
			return mcpToolFailure(id, "pull request "+jsonNumberString(args["pull_number"])+" not found")
		}
		sha := forge.MergePull(repo, pull, "squash")
		if sha == "" {
			return mcpToolFailure(id, "Pull Request is not mergeable")
		}
		// pullJson plus the merge sha, mirroring the reference tool's result shape
		b := &jsonBuf{}
		pullJSON(b, repo, pull)
		merged := b.bytes()
		merged = append(merged[:len(merged)-1], []byte(`,"merge_commit_sha":`+strconv2(sha)+`}`)...)
		return mcpToolResult(id, merged)
	}
	return mcpRPCError(id, -32601, "tool not found: "+name)
}

func strconv2(s string) string {
	enc, _ := json.Marshal(s)
	return string(enc)
}

func jsonNumberString(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case string:
		return n
	}
	return ""
}

// Handle one JSON-RPC message; notifications (no id) answer nil (HTTP 202 upstream).
func handleMcp(forge *Forge, message map[string]any) *mcpReply {
	id, hasID := message["id"]
	if !hasID {
		return nil
	}
	method, _ := message["method"].(string)
	switch method {
	case "server/discover":
		versions := make([]any, 0, len(mcpSupportedVersions))
		for _, v := range mcpSupportedVersions {
			versions = append(versions, v)
		}
		return mcpRPCResult(id, map[string]any{
			"resultType":        "complete",
			"supportedVersions": versions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        mcpServerInfo,
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": mcpServerInfo},
		})
	case "initialize":
		params, _ := message["params"].(map[string]any)
		requested, _ := params["protocolVersion"].(string)
		version := "2025-11-25"
		for _, v := range mcpSupportedVersions {
			if v == requested {
				version = requested
			}
		}
		return mcpRPCResult(id, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      mcpServerInfo,
		})
	case "ping":
		return mcpRPCResult(id, map[string]any{})
	case "tools/list":
		return mcpRPCResult(id, mcpToolsList())
	case "tools/call":
		params, _ := message["params"].(map[string]any)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		name, _ := params["name"].(string)
		return mcpCallTool(forge, id, name, args)
	}
	return mcpRPCError(id, -32601, "method not found: "+method)
}

func mcpParseError() *mcpReply {
	return &mcpReply{status: 400, body: map[string]any{
		"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": float64(-32700), "message": "Parse error"},
	}}
}
