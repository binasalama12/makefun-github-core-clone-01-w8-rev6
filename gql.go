// gql.go — the GraphQL v4 surface: a small parser and executor covering exactly the
// schema the suite speaks (viewer, repository, issues, pull requests, mutations, Relay
// node ids, __type introspection and operation batching). Global ids are opaque
// base64url and stable per object.

package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------------------------
// global ids
// ---------------------------------------------------------------------------------------------

func encodeID(kind string, parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v1:" + kind + ":" + strings.Join(parts, ":")))
}

var decodeIDRe = regexp.MustCompile(`^v1:([a-z]+):(.*)$`)

func decodeID(id string) (string, []string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(id)
		if err != nil {
			return "", nil, false
		}
	}
	m := decodeIDRe.FindStringSubmatch(string(raw))
	if m == nil {
		return "", nil, false
	}
	return m[1], strings.Split(m[2], ":"), true
}

func repoGlobalID(repo *RepoRec) string  { return encodeID("repo", strconv.Itoa(repo.ID)) }
func issueGlobalID(repo *RepoRec, issue *IssueRec) string {
	return encodeID("issue", strconv.Itoa(repo.ID), strconv.Itoa(issue.Number))
}
func pullGlobalID(repo *RepoRec, pull *PullRec) string {
	return encodeID("pr", strconv.Itoa(repo.ID), strconv.Itoa(pull.Number))
}
func commentGlobalID(id int) string { return encodeID("comment", strconv.Itoa(id)) }

// ---------------------------------------------------------------------------------------------
// schema (field lists drive both validation and __type introspection)
// ---------------------------------------------------------------------------------------------

var objectFields = map[string][]string{
	"Query":    {"viewer", "repository", "node", "__type", "__schema"},
	"Mutation": {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":     {"login", "id", "name", "email"},
	"Repository": {
		"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission",
		"hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests",
	},
	"Ref": {"name"},
	"Issue": {
		"id", "number", "title", "state", "body", "url", "createdAt", "updatedAt", "closedAt", "repository", "comments",
	},
	"IssueConnection":               {"nodes", "pageInfo", "totalCount"},
	"IssueCommentConnection":        {"nodes", "totalCount"},
	"PullRequestConnection":         {"nodes", "pageInfo", "totalCount"},
	"PageInfo":                      {"hasNextPage", "endCursor", "hasPreviousPage", "startCursor"},
	"PullRequest": {
		"id", "number", "title", "body", "state", "merged", "mergedAt", "headRefName", "baseRefName",
		"mergeable", "files", "repository",
	},
	"PullRequestChangedFileConnection": {"nodes", "totalCount"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changeType"},
	"CreateIssuePayload":               {"issue", "clientMutationId"},
	"CloseIssuePayload":                {"issue", "clientMutationId"},
	"CreatePullRequestPayload":         {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":          {"pullRequest", "clientMutationId"},
	"AddCommentPayload":                {"commentEdge", "clientMutationId"},
	"CommentEdge":                      {"node", "cursor"},
	"IssueComment":                     {"id", "body"},
	"__Type":                           {"name", "kind", "fields", "enumValues", "description"},
	"__Field":                          {"name", "description"},
	"__EnumValue":                      {"name", "description"},
	"__Schema":                         {"types", "queryType", "mutationType", "description"},
}

// Schema type names in declaration order (for __schema.types).
var schemaTypeOrder = []string{
	"Query", "Mutation", "User", "Repository", "Ref", "Issue", "IssueConnection",
	"IssueCommentConnection", "PullRequestConnection", "PageInfo", "PullRequest",
	"PullRequestChangedFileConnection", "PullRequestChangedFile", "CreateIssuePayload",
	"CloseIssuePayload", "CreatePullRequestPayload", "MergePullRequestPayload",
	"AddCommentPayload", "CommentEdge", "IssueComment", "__Type", "__Field", "__EnumValue", "__Schema",
}

var enumValues = map[string][]string{
	"IssueState":              {"OPEN", "CLOSED"},
	"PullRequestState":        {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":          {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestMergeMethod":  {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission":    {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"SearchType":              {"CODE", "COMMIT", "ISSUE", "PR", "REPO", "USER"},
	"IssueOrderField":         {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":          {"ASC", "DESC"},
}

func fieldsOf(typeName string) map[string]bool {
	fields, ok := objectFields[typeName]
	if !ok {
		return nil
	}
	set := map[string]bool{"__typename": true}
	for _, f := range fields {
		set[f] = true
	}
	return set
}

// ---------------------------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------------------------

type gqlValue struct {
	kind   string // var | string | number | bool | null | enum | list | object
	name   string
	str    string
	num    float64
	b      bool
	items  []*gqlValue
	fields []gqlObjField
}

type gqlObjField struct {
	name  string
	value *gqlValue
}

type gqlField struct {
	alias     string
	hasAlias  bool
	name      string
	args      []gqlObjField
	selection []gqlSelection
}

type gqlSelection struct {
	kind          string // field | fragment | spread
	field         *gqlField
	typeCondition string
	selection     []gqlSelection
	spreadName    string
}

type gqlOperation struct {
	typ       string // query | mutation
	name      string
	selection []gqlSelection
}

type gqlToken struct {
	kind string // name | string | punct
	text string
}

var gqlNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+`)

func gqlTokenize(input string) []gqlToken {
	var tokens []gqlToken
	i := 0
	for i < len(input) {
		ch := input[i]
		switch {
		case ch == '#':
			for i < len(input) && input[i] != '\n' {
				i++
			}
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',':
			i++
		case ch == '"':
			j := i + 1
			var out strings.Builder
			for j < len(input) && input[j] != '"' {
				if input[j] == '\\' && j+1 < len(input) {
					esc := input[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(input[j])
					j++
				}
			}
			tokens = append(tokens, gqlToken{kind: "string", text: out.String()})
			i = j + 1
		case strings.ContainsRune("{}()[]:!=$@", rune(ch)):
			tokens = append(tokens, gqlToken{kind: "punct", text: string(ch)})
			i++
		case ch == '.' && input[i:min(i+3, len(input))] == "...":
			tokens = append(tokens, gqlToken{kind: "punct", text: "..."})
			i += 3
		default:
			if m := gqlNameRe.FindString(input[i:]); m != "" {
				tokens = append(tokens, gqlToken{kind: "name", text: m})
				i += len(m)
			} else {
				i++
			}
		}
	}
	return tokens
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() (gqlToken, bool) {
	if p.pos >= len(p.tokens) {
		return gqlToken{}, false
	}
	return p.tokens[p.pos], true
}

func (p *gqlParser) next() (gqlToken, error) {
	if p.pos >= len(p.tokens) {
		return gqlToken{}, errors.New("unexpected end of document")
	}
	tok := p.tokens[p.pos]
	p.pos++
	return tok, nil
}

func (p *gqlParser) expect(text string) error {
	tok, err := p.next()
	if err != nil {
		return err
	}
	if tok.text != text {
		return fmt.Errorf("expected %s, got %s", text, tok.text)
	}
	return nil
}

func (p *gqlParser) parseDocument() ([]gqlOperation, map[string][]gqlSelection, error) {
	var operations []gqlOperation
	fragments := map[string][]gqlSelection{}
	for p.pos < len(p.tokens) {
		tok, _ := p.peek()
		switch {
		case tok.text == "{":
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			operations = append(operations, gqlOperation{typ: "query", selection: sel})
		case tok.kind == "name" && (tok.text == "query" || tok.text == "mutation"):
			typ := tok.text
			p.next()
			name := ""
			if next, ok := p.peek(); ok && next.kind == "name" && gqlNameStartRe.MatchString(next.text) {
				name = next.text
				p.next()
			}
			if next, ok := p.peek(); ok && next.text == "(" {
				if err := p.skipVariableDefinitions(); err != nil {
					return nil, nil, err
				}
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			operations = append(operations, gqlOperation{typ: typ, name: name, selection: sel})
		case tok.kind == "name" && tok.text == "fragment":
			p.next()
			nameTok, err := p.next()
			if err != nil {
				return nil, nil, err
			}
			if err := p.expect("on"); err != nil {
				return nil, nil, err
			}
			p.next() // type condition (not retained, mirroring the reference parser)
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			fragments[nameTok.text] = sel
		default:
			p.next()
		}
	}
	return operations, fragments, nil
}

func (p *gqlParser) skipVariableDefinitions() error {
	if err := p.expect("("); err != nil {
		return err
	}
	depth := 1
	for depth > 0 {
		tok, err := p.next()
		if err != nil {
			return err
		}
		switch tok.text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
		}
	}
	return nil
}

var gqlNameStartRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (p *gqlParser) parseSelectionSet() ([]gqlSelection, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var selections []gqlSelection
	for {
		tok, ok := p.peek()
		if !ok {
			return nil, errors.New("unexpected end of document")
		}
		if tok.text == "}" {
			break
		}
		if tok.text == "..." {
			p.next()
			if next, ok := p.peek(); ok && next.text == "on" {
				p.next()
				condTok, err := p.next()
				if err != nil {
					return nil, err
				}
				sel, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				selections = append(selections, gqlSelection{kind: "fragment", typeCondition: condTok.text, selection: sel})
			} else {
				nameTok, err := p.next()
				if err != nil {
					return nil, err
				}
				selections = append(selections, gqlSelection{kind: "spread", spreadName: nameTok.text})
			}
			continue
		}
		nameTok, err := p.next()
		if err != nil {
			return nil, err
		}
		field := &gqlField{name: nameTok.text}
		if next, ok := p.peek(); ok && next.text == ":" {
			p.next()
			realTok, err := p.next()
			if err != nil {
				return nil, err
			}
			field.alias = field.name
			field.hasAlias = true
			field.name = realTok.text
		}
		if next, ok := p.peek(); ok && next.text == "(" {
			p.next()
			for {
				t, ok := p.peek()
				if !ok {
					return nil, errors.New("unexpected end of document")
				}
				if t.text == ")" {
					break
				}
				argTok, err := p.next()
				if err != nil {
					return nil, err
				}
				if err := p.expect(":"); err != nil {
					return nil, err
				}
				val, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				field.args = append(field.args, gqlObjField{name: argTok.text, value: val})
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
		}
		if next, ok := p.peek(); ok && next.text == "{" {
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			field.selection = sel
		}
		selections = append(selections, gqlSelection{kind: "field", field: field})
	}
	if err := p.expect("}"); err != nil {
		return nil, err
	}
	return selections, nil
}

var (
	gqlIntRe   = regexp.MustCompile(`^-?\d+$`)
	gqlFloatRe = regexp.MustCompile(`^-?\d*\.\d+$`)
)

func (p *gqlParser) parseValue() (*gqlValue, error) {
	tok, err := p.next()
	if err != nil {
		return nil, err
	}
	switch {
	case tok.text == "$":
		nameTok, err := p.next()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "var", name: nameTok.text}, nil
	case strings.HasPrefix(tok.text, "$"):
		return &gqlValue{kind: "var", name: tok.text[1:]}, nil
	case tok.kind == "string":
		return &gqlValue{kind: "string", str: tok.text}, nil
	case tok.text == "[":
		var items []*gqlValue
		for {
			t, ok := p.peek()
			if !ok {
				return nil, errors.New("unexpected end of document")
			}
			if t.text == "]" {
				break
			}
			item, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		return &gqlValue{kind: "list", items: items}, nil
	case tok.text == "{":
		var fields []gqlObjField
		for {
			t, ok := p.peek()
			if !ok {
				return nil, errors.New("unexpected end of document")
			}
			if t.text == "}" {
				break
			}
			nameTok, err := p.next()
			if err != nil {
				return nil, err
			}
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			fields = append(fields, gqlObjField{name: nameTok.text, value: val})
		}
		if err := p.expect("}"); err != nil {
			return nil, err
		}
		return &gqlValue{kind: "object", fields: fields}, nil
	case tok.text == "true" || tok.text == "false":
		return &gqlValue{kind: "bool", b: tok.text == "true"}, nil
	case tok.text == "null":
		return &gqlValue{kind: "null"}, nil
	case gqlIntRe.MatchString(tok.text):
		n, _ := strconv.ParseInt(tok.text, 10, 64)
		return &gqlValue{kind: "number", num: float64(n)}, nil
	case gqlFloatRe.MatchString(tok.text):
		n, _ := strconv.ParseFloat(tok.text, 64)
		return &gqlValue{kind: "number", num: n}, nil
	}
	return &gqlValue{kind: "enum", str: tok.text}, nil
}

// ---------------------------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------------------------

type wrapped struct {
	typ   string
	props map[string]any
	repo  *RepoRec
	issue *IssueRec
	pull  *PullRec
	name  string
}

func wrap(typ string, props map[string]any) *wrapped {
	if props == nil {
		props = map[string]any{}
	}
	return &wrapped{typ: typ, props: props}
}

type gqlExecutor struct {
	forge     *Forge
	variables map[string]any
	fragments map[string][]gqlSelection
	errors    []string
}

func (e *gqlExecutor) evalValue(v *gqlValue) any {
	switch v.kind {
	case "var":
		return e.variables[v.name]
	case "string", "enum":
		return v.str
	case "number":
		return v.num
	case "bool":
		return v.b
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.items))
		for _, item := range v.items {
			out = append(out, e.evalValue(item))
		}
		return out
	case "object":
		out := map[string]any{}
		for _, f := range v.fields {
			out[f.name] = e.evalValue(f.value)
		}
		return out
	}
	return nil
}

func (e *gqlExecutor) run(op gqlOperation) map[string]any {
	rootType := "Query"
	if op.typ == "mutation" {
		rootType = "Mutation"
	}
	return e.execSet(wrap(rootType, nil), op.selection)
}

func (e *gqlExecutor) execSet(source *wrapped, selections []gqlSelection) map[string]any {
	out := map[string]any{}
	for _, selection := range selections {
		switch selection.kind {
		case "fragment":
			if selection.typeCondition == "" || selection.typeCondition == source.typ {
				for k, v := range e.execSet(source, selection.selection) {
					out[k] = v
				}
			}
		case "spread":
			if fragment, ok := e.fragments[selection.spreadName]; ok {
				for k, v := range e.execSet(source, fragment) {
					out[k] = v
				}
			}
		case "field":
			field := selection.field
			key := field.name
			if field.hasAlias {
				key = field.alias
			}
			if field.name == "__typename" {
				out[key] = source.typ
				continue
			}
			known := fieldsOf(source.typ)
			if known == nil || !known[field.name] {
				e.errors = append(e.errors, fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field.name, source.typ))
				out[key] = nil
				continue
			}
			args := map[string]any{}
			for _, arg := range field.args {
				args[arg.name] = e.evalValue(arg.value)
			}
			value, err := e.resolve(source, field.name, args)
			if err != nil {
				e.errors = append(e.errors, err.Error())
				value = nil
			}
			out[key] = e.complete(value, field.selection)
		}
	}
	return out
}

func (e *gqlExecutor) complete(value any, selection []gqlSelection) any {
	if value == nil {
		return nil
	}
	if list, ok := value.([]any); ok {
		out := make([]any, 0, len(list))
		for _, item := range list {
			out = append(out, e.complete(item, selection))
		}
		return out
	}
	if w, ok := value.(*wrapped); ok {
		if w == nil {
			return nil
		}
		if selection == nil {
			return map[string]any{}
		}
		return e.execSet(w, selection)
	}
	return value
}

// ---- argument coercions ---------------------------------------------------------

func argString(args map[string]any, name string) string {
	if s, ok := args[name].(string); ok {
		return s
	}
	return ""
}

func argNumber(args map[string]any, name string) (float64, bool) {
	if n, ok := args[name].(float64); ok {
		return n, true
	}
	return 0, false
}

// ---- resolvers ---------------------------------------------------------------

func (e *gqlExecutor) resolve(source *wrapped, field string, args map[string]any) (any, error) {
	forge := e.forge
	switch source.typ {
	case "Query":
		switch field {
		case "viewer":
			return wrap("User", map[string]any{"login": ViewerLogin}), nil
		case "repository":
			repo := forge.GetRepo(argString(args, "owner"), argString(args, "name"))
			if repo == nil {
				return nil, nil
			}
			return e.repoWrap(repo), nil
		case "node":
			return e.nodeByID(argString(args, "id")), nil
		case "__type":
			return e.typeWrap(argString(args, "name")), nil
		case "__schema":
			return wrap("__Schema", nil), nil
		}
	case "Mutation":
		switch field {
		case "createIssue":
			return e.mutateCreateIssue(args)
		case "closeIssue":
			return e.mutateCloseIssue(args)
		case "createPullRequest":
			return e.mutateCreatePull(args)
		case "mergePullRequest":
			return e.mutateMergePull(args)
		case "addComment":
			return e.mutateAddComment(args)
		}
	case "User":
		switch field {
		case "login":
			return source.props["login"], nil
		case "id":
			return encodeID("user", strconv.Itoa(ViewerID)), nil
		case "name":
			return ViewerName, nil
		case "email":
			return ViewerEmail, nil
		}
	case "Repository":
		repo := source.repo
		switch field {
		case "id":
			return repoGlobalID(repo), nil
		case "databaseId":
			return float64(repo.ID), nil
		case "name":
			return repo.Name, nil
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name, nil
		case "owner":
			return wrap("User", map[string]any{"login": repo.Owner}), nil
		case "defaultBranchRef":
			if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
				return wrap("Ref", map[string]any{"name": repo.DefaultBranch}), nil
			}
			return nil, nil
		case "viewerPermission":
			return "ADMIN", nil
		case "hasIssuesEnabled":
			return true, nil
		case "issue":
			if n, ok := argNumber(args, "number"); ok {
				if issue := repo.Issues[int(n)]; issue != nil {
					return e.issueWrap(repo, issue), nil
				}
			}
			return nil, nil
		case "issues":
			return e.issuesConnection(repo, args), nil
		case "pullRequest":
			if n, ok := argNumber(args, "number"); ok {
				if pull := repo.Pulls[int(n)]; pull != nil {
					return e.pullWrap(repo, pull), nil
				}
			}
			return nil, nil
		case "pullRequests":
			return e.pullsConnection(repo, args), nil
		}
	case "Ref":
		return source.props[field], nil
	case "Issue":
		repo, issue := source.repo, source.issue
		switch field {
		case "id":
			return issueGlobalID(repo, issue), nil
		case "number":
			return float64(issue.Number), nil
		case "title":
			return issue.Title, nil
		case "state":
			return strings.ToUpper(issue.State), nil
		case "body":
			return issue.Body, nil
		case "url":
			return fmt.Sprintf("https://forge.invalid/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number), nil
		case "createdAt", "updatedAt":
			return isoFromMillis(1735689600000 + int64(issue.DBID)*1000), nil
		case "closedAt":
			if issue.State == "closed" {
				return isoFromMillis(1735689600000 + int64(issue.DBID)*1000), nil
			}
			return nil, nil
		case "repository":
			return e.repoWrap(repo), nil
		case "comments":
			nodes := make([]any, 0, len(issue.Comments))
			for _, comment := range issue.Comments {
				nodes = append(nodes, wrap("IssueComment", map[string]any{"id": commentGlobalID(comment.ID), "body": comment.Body}))
			}
			return wrap("IssueCommentConnection", map[string]any{"nodes": nodes, "totalCount": float64(len(issue.Comments))}), nil
		}
	case "PullRequest":
		repo, pull := source.repo, source.pull
		switch field {
		case "id":
			return pullGlobalID(repo, pull), nil
		case "number":
			return float64(pull.Number), nil
		case "title":
			return pull.Title, nil
		case "body":
			return pull.Body, nil
		case "state":
			if pull.Merged {
				return "MERGED", nil
			}
			return strings.ToUpper(pull.State), nil
		case "merged":
			return pull.Merged, nil
		case "mergedAt":
			if pull.MergedAt != nil {
				return *pull.MergedAt, nil
			}
			return nil, nil
		case "headRefName":
			return pull.HeadRef, nil
		case "baseRefName":
			return pull.BaseRef, nil
		case "mergeable":
			if pull.Merged || pull.State != "open" {
				return "UNKNOWN", nil
			}
			if forge.MergeAnalysis(repo, pull).Conflict {
				return "CONFLICTING", nil
			}
			return "MERGEABLE", nil
		case "files":
			baseTip, ok := repo.Refs["refs/heads/"+pull.BaseRef]
			if !ok {
				baseTip = pull.BaseSha
			}
			headTip, ok := repo.Refs["refs/heads/"+pull.HeadRef]
			if !ok {
				headTip = pull.HeadSha
			}
			diff := forge.DiffFlat(forge.TreeOfCommit(baseTip), forge.TreeOfCommit(headTip))
			first := len(diff)
			if n, ok := argNumber(args, "first"); ok {
				first = int(n)
			}
			if first > len(diff) {
				first = len(diff)
			}
			nodes := make([]any, 0, first)
			for _, entry := range diff[:first] {
				nodes = append(nodes, wrap("PullRequestChangedFile", map[string]any{
					"path": entry.Filename, "additions": float64(entry.Additions),
					"deletions": float64(entry.Deletions), "changeType": strings.ToUpper(entry.Status),
				}))
			}
			return wrap("PullRequestChangedFileConnection", map[string]any{"nodes": nodes, "totalCount": float64(len(diff))}), nil
		case "repository":
			return e.repoWrap(repo), nil
		}
	case "__Type":
		name := source.name
		switch field {
		case "name":
			return name, nil
		case "kind":
			if _, ok := enumValues[name]; ok {
				return "ENUM", nil
			}
			return "OBJECT", nil
		case "description":
			return nil, nil
		case "fields":
			fields, ok := objectFields[name]
			if !ok {
				return nil, nil
			}
			out := make([]any, 0, len(fields))
			for _, fieldName := range fields {
				out = append(out, wrap("__Field", map[string]any{"name": fieldName}))
			}
			return out, nil
		case "enumValues":
			values, ok := enumValues[name]
			if !ok {
				return nil, nil
			}
			out := make([]any, 0, len(values))
			for _, valueName := range values {
				out = append(out, wrap("__EnumValue", map[string]any{"name": valueName}))
			}
			return out, nil
		}
	case "__Field", "__EnumValue":
		switch field {
		case "name":
			return source.props["name"], nil
		case "description":
			return nil, nil
		}
	case "__Schema":
		switch field {
		case "types":
			out := make([]any, 0, len(schemaTypeOrder))
			for _, name := range schemaTypeOrder {
				out = append(out, e.typeWrap(name))
			}
			return out, nil
		case "queryType":
			return e.typeWrap("Query"), nil
		case "mutationType":
			return e.typeWrap("Mutation"), nil
		case "description":
			return nil, nil
		}
	default:
		// Connections and payloads carry their values as plain properties.
		if v, ok := source.props[field]; ok {
			return v, nil
		}
	}
	return nil, nil
}

func (e *gqlExecutor) repoWrap(repo *RepoRec) *wrapped {
	w := wrap("Repository", nil)
	w.repo = repo
	return w
}

func (e *gqlExecutor) issueWrap(repo *RepoRec, issue *IssueRec) *wrapped {
	w := wrap("Issue", nil)
	w.repo = repo
	w.issue = issue
	return w
}

func (e *gqlExecutor) pullWrap(repo *RepoRec, pull *PullRec) *wrapped {
	w := wrap("PullRequest", nil)
	w.repo = repo
	w.pull = pull
	return w
}

func (e *gqlExecutor) typeWrap(name string) *wrapped {
	_, isObject := objectFields[name]
	_, isEnum := enumValues[name]
	if !isObject && !isEnum {
		return nil
	}
	w := wrap("__Type", nil)
	w.name = name
	return w
}

func (e *gqlExecutor) repoByID(id int) *RepoRec {
	for _, repo := range e.forge.Repos {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

func (e *gqlExecutor) nodeByID(id string) *wrapped {
	kind, parts, ok := decodeID(id)
	if !ok {
		return nil
	}
	switch kind {
	case "repo":
		n, _ := strconv.Atoi(parts[0])
		if repo := e.repoByID(n); repo != nil {
			return e.repoWrap(repo)
		}
	case "issue", "pr":
		n, _ := strconv.Atoi(parts[0])
		repo := e.repoByID(n)
		if repo == nil {
			return nil
		}
		number, _ := strconv.Atoi(parts[1])
		if kind == "issue" {
			if issue := repo.Issues[number]; issue != nil {
				return e.issueWrap(repo, issue)
			}
			return nil
		}
		if pull := repo.Pulls[number]; pull != nil {
			return e.pullWrap(repo, pull)
		}
		return nil
	case "comment":
		commentID, _ := strconv.Atoi(parts[0])
		for _, repo := range e.forge.Repos {
			for _, issue := range repo.Issues {
				for _, comment := range issue.Comments {
					if comment.ID == commentID {
						return wrap("IssueComment", map[string]any{"id": commentGlobalID(comment.ID), "body": comment.Body})
					}
				}
			}
		}
	case "user":
		return wrap("User", map[string]any{"login": ViewerLogin})
	}
	return nil
}

func (e *gqlExecutor) issuesConnection(repo *RepoRec, args map[string]any) *wrapped {
	var list []*IssueRec
	for _, issue := range repo.Issues {
		list = append(list, issue)
	}
	if states, ok := args["states"].([]any); ok {
		wanted := map[string]bool{}
		for _, state := range states {
			if s, ok := state.(string); ok {
				wanted[strings.ToLower(s)] = true
			}
		}
		filtered := list[:0]
		for _, issue := range list {
			if wanted[issue.State] {
				filtered = append(filtered, issue)
			}
		}
		list = filtered
	}
	direction := "ASC"
	if orderBy, ok := args["orderBy"].(map[string]any); ok {
		if d, ok := orderBy["direction"].(string); ok {
			direction = strings.ToUpper(d)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if direction == "DESC" {
			return list[i].Number > list[j].Number
		}
		return list[i].Number < list[j].Number
	})
	items := make([]any, 0, len(list))
	for _, issue := range list {
		items = append(items, e.issueWrap(repo, issue))
	}
	return e.paginate("IssueConnection", items, args)
}

func (e *gqlExecutor) pullsConnection(repo *RepoRec, args map[string]any) *wrapped {
	var list []*PullRec
	for _, pull := range repo.Pulls {
		list = append(list, pull)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
	items := make([]any, 0, len(list))
	for _, pull := range list {
		items = append(items, e.pullWrap(repo, pull))
	}
	return e.paginate("PullRequestConnection", items, args)
}

// Cursor pagination: a cursor is the base64 of the last index served; `after` resumes past it.
func (e *gqlExecutor) paginate(typ string, items []any, args map[string]any) *wrapped {
	start := 0
	if after, ok := args["after"].(string); ok && after != "" {
		raw, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil {
			raw, err = base64.URLEncoding.DecodeString(after)
		}
		if err == nil {
			if n, err := strconv.Atoi(string(raw)); err == nil && n >= 0 {
				start = n + 1
			}
		}
	}
	first := len(items)
	if n, ok := argNumber(args, "first"); ok {
		first = int(n)
		if first < 0 {
			first = 0
		}
	}
	if start > len(items) {
		start = len(items)
	}
	end := start + first
	if end > len(items) {
		end = len(items)
	}
	slice := items[start:end]
	hasNext := start+len(slice) < len(items)
	var endCursor, startCursor any
	if len(slice) > 0 {
		endCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(start + len(slice) - 1)))
		startCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(start)))
	}
	return wrap(typ, map[string]any{
		"nodes":      slice,
		"totalCount": float64(len(items)),
		"pageInfo": wrap("PageInfo", map[string]any{
			"hasNextPage": hasNext, "endCursor": endCursor,
			"hasPreviousPage": start > 0, "startCursor": startCursor,
		}),
	})
}

// ---- mutations -----------------------------------------------------------------

func (e *gqlExecutor) repoByGlobalID(id any) (*RepoRec, error) {
	s, ok := id.(string)
	if !ok {
		return nil, errors.New("unknown repository id")
	}
	kind, parts, ok := decodeID(s)
	if !ok || kind != "repo" {
		return nil, errors.New("unknown repository id")
	}
	n, _ := strconv.Atoi(parts[0])
	repo := e.repoByID(n)
	if repo == nil {
		return nil, errors.New("unknown repository id")
	}
	return repo, nil
}

func inputOf(args map[string]any) map[string]any {
	if input, ok := args["input"].(map[string]any); ok {
		return input
	}
	return map[string]any{}
}

func (e *gqlExecutor) mutateCreateIssue(args map[string]any) (any, error) {
	input := inputOf(args)
	repo, err := e.repoByGlobalID(input["repositoryId"])
	if err != nil {
		return nil, err
	}
	title, _ := input["title"].(string)
	if title == "" {
		return nil, errors.New("title is required")
	}
	body, _ := input["body"].(string)
	issue := e.forge.CreateIssue(repo, title, body, nil)
	return wrap("CreateIssuePayload", map[string]any{
		"issue": e.issueWrap(repo, issue), "clientMutationId": input["clientMutationId"],
	}), nil
}

func (e *gqlExecutor) mutateCloseIssue(args map[string]any) (any, error) {
	input := inputOf(args)
	s, _ := input["issueId"].(string)
	kind, parts, ok := decodeID(s)
	if !ok || kind != "issue" {
		return nil, errors.New("unknown issue id")
	}
	repoID, _ := strconv.Atoi(parts[0])
	number, _ := strconv.Atoi(parts[1])
	repo := e.repoByID(repoID)
	if repo == nil {
		return nil, errors.New("unknown issue id")
	}
	issue := repo.Issues[number]
	if issue == nil {
		return nil, errors.New("unknown issue id")
	}
	issue.State = "closed"
	return wrap("CloseIssuePayload", map[string]any{
		"issue": e.issueWrap(repo, issue), "clientMutationId": input["clientMutationId"],
	}), nil
}

func (e *gqlExecutor) mutateCreatePull(args map[string]any) (any, error) {
	input := inputOf(args)
	repo, err := e.repoByGlobalID(input["repositoryId"])
	if err != nil {
		return nil, err
	}
	head := argString(input, "headRefName")
	base := argString(input, "baseRefName")
	title, _ := input["title"].(string)
	headSha, hasHead := repo.Refs["refs/heads/"+head]
	baseSha, hasBase := repo.Refs["refs/heads/"+base]
	if !hasHead || !hasBase {
		return nil, errors.New("head or base branch does not exist")
	}
	headCommit := e.forge.PeelToCommit(headSha)
	baseCommit := e.forge.PeelToCommit(baseSha)
	if headCommit == "" || baseCommit == "" || headCommit == baseCommit || e.forge.IsAncestor(headCommit, baseCommit) {
		return nil, errors.New("No commits between the base and the head")
	}
	for _, pull := range repo.Pulls {
		if pull.State == "open" && pull.HeadRef == head && pull.BaseRef == base {
			return nil, errors.New("A pull request already exists for this head and base")
		}
	}
	body, _ := input["body"].(string)
	pull := e.forge.CreatePull(repo, title, body, head, base, headSha, baseSha)
	return wrap("CreatePullRequestPayload", map[string]any{
		"pullRequest": e.pullWrap(repo, pull), "clientMutationId": input["clientMutationId"],
	}), nil
}

func (e *gqlExecutor) mutateMergePull(args map[string]any) (any, error) {
	input := inputOf(args)
	s, _ := input["pullRequestId"].(string)
	kind, parts, ok := decodeID(s)
	if !ok || kind != "pr" {
		return nil, errors.New("unknown pull request id")
	}
	repoID, _ := strconv.Atoi(parts[0])
	number, _ := strconv.Atoi(parts[1])
	repo := e.repoByID(repoID)
	if repo == nil {
		return nil, errors.New("unknown pull request id")
	}
	pull := repo.Pulls[number]
	if pull == nil {
		return nil, errors.New("unknown pull request id")
	}
	method := strings.ToUpper(argString(input, "mergeMethod"))
	if method == "" {
		method = "MERGE"
	}
	mergeMethod := "merge"
	if method == "SQUASH" {
		mergeMethod = "squash"
	}
	sha := e.forge.MergePull(repo, pull, mergeMethod)
	if sha == "" {
		return nil, errors.New("Pull Request is not mergeable")
	}
	return wrap("MergePullRequestPayload", map[string]any{
		"pullRequest": e.pullWrap(repo, pull), "clientMutationId": input["clientMutationId"],
	}), nil
}

func (e *gqlExecutor) mutateAddComment(args map[string]any) (any, error) {
	input := inputOf(args)
	s, _ := input["subjectId"].(string)
	kind, parts, ok := decodeID(s)
	if !ok || (kind != "issue" && kind != "pr") {
		return nil, errors.New("unknown subject id")
	}
	repoID, _ := strconv.Atoi(parts[0])
	number, _ := strconv.Atoi(parts[1])
	repo := e.repoByID(repoID)
	if repo == nil {
		return nil, errors.New("unknown subject id")
	}
	body, _ := input["body"].(string)
	if kind == "issue" {
		issue := repo.Issues[number]
		if issue == nil {
			return nil, errors.New("unknown subject id")
		}
		comment := e.forge.AddComment(repo, issue, body)
		return wrap("AddCommentPayload", map[string]any{
			"commentEdge": wrap("CommentEdge", map[string]any{
				"node":   wrap("IssueComment", map[string]any{"id": commentGlobalID(comment.ID), "body": comment.Body}),
				"cursor": nil,
			}),
			"clientMutationId": input["clientMutationId"],
		}), nil
	}
	pull := repo.Pulls[number]
	if pull == nil {
		return nil, errors.New("unknown subject id")
	}
	comment := CommentRec{ID: 1000000 + len(pull.Comments) + 1, Body: body}
	pull.Comments = append(pull.Comments, comment)
	return wrap("AddCommentPayload", map[string]any{
		"commentEdge": wrap("CommentEdge", map[string]any{
			"node":   wrap("IssueComment", map[string]any{"id": commentGlobalID(comment.ID), "body": comment.Body}),
			"cursor": nil,
		}),
		"clientMutationId": input["clientMutationId"],
	}), nil
}

// ---------------------------------------------------------------------------------------------
// entry point
// ---------------------------------------------------------------------------------------------

type gqlRequest struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

func executeGraphQL(forge *Forge, request gqlRequest) map[string]any {
	parser := &gqlParser{tokens: gqlTokenize(request.Query)}
	operations, fragments, err := parser.parseDocument()
	if err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": err.Error()}}}
	}
	var operation *gqlOperation
	if request.OperationName != "" {
		for i := range operations {
			if operations[i].name == request.OperationName {
				operation = &operations[i]
				break
			}
		}
	}
	if operation == nil && len(operations) > 0 {
		operation = &operations[0]
	}
	if operation == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "no operation in document"}}}
	}
	variables := request.Variables
	if variables == nil {
		variables = map[string]any{}
	}
	executor := &gqlExecutor{forge: forge, variables: variables, fragments: fragments}
	data := executor.run(*operation)
	out := map[string]any{"data": data}
	if len(executor.errors) > 0 {
		errs := make([]any, 0, len(executor.errors))
		for _, message := range executor.errors {
			errs = append(errs, map[string]any{"message": message})
		}
		out["errors"] = errs
	}
	return out
}
