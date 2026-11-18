// rest.go — the REST v3 surface: the /api/v3 routes the suite reads and writes.
// Handlers are pure functions over the Forge store; app.go owns sockets, auth and framing.
// The three hot views (repo / issue / pull) are written with the hand-rolled JSON
// writer; cold shapes go through encoding/json.

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type restResult struct {
	status  int
	body    []byte
	headers map[string]string
}

func restJSON(status int, v any) restResult {
	data, _ := json.Marshal(v)
	return restResult{status: status, body: data}
}

func notFound() restResult {
	return restResult{status: 404, body: []byte(`{"message":"Not Found"}`)}
}

func unprocessable(message string) restResult {
	b := &jsonBuf{}
	b.raw("{").key("message").str(message).raw("}")
	return restResult{status: 422, body: b.bytes()}
}

// ---- views -------------------------------------------------------------------

func repoJSON(b *jsonBuf, repo *RepoRec) {
	b.raw("{")
	b.key("id").num(repo.ID).raw(",")
	b.key("name").str(repo.Name).raw(",")
	b.key("full_name").str(repo.Owner + "/" + repo.Name).raw(",")
	b.key("owner").raw("{")
	b.key("login").str(repo.Owner).raw(",")
	b.key("id").num(ViewerID).raw(",")
	b.key("type").str("User")
	b.raw("},")
	b.key("private").boolean(repo.Private).raw(",")
	b.key("description").str(repo.Description).raw(",")
	b.key("default_branch").str(repo.DefaultBranch)
	b.raw("}")
}

func repoJSONBytes(repo *RepoRec) []byte {
	b := &jsonBuf{}
	repoJSON(b, repo)
	return b.bytes()
}

func issueJSON(b *jsonBuf, repo *RepoRec, issue *IssueRec) {
	b.raw("{")
	b.key("id").num(issue.DBID).raw(",")
	b.key("number").num(issue.Number).raw(",")
	b.key("title").str(issue.Title).raw(",")
	b.key("body").str(issue.Body).raw(",")
	b.key("state").str(issue.State).raw(",")
	b.key("labels").raw("[")
	for i, name := range issue.Labels {
		if i > 0 {
			b.raw(",")
		}
		color := "ededed"
		if label, ok := repo.Labels[name]; ok {
			color = label.Color
		}
		b.raw("{")
		b.key("name").str(name).raw(",")
		b.key("color").str(color)
		b.raw("}")
	}
	b.raw("],")
	b.key("comments").num(len(issue.Comments)).raw(",")
	b.key("user").raw("{")
	b.key("login").str(ViewerLogin).raw(",")
	b.key("id").num(ViewerID)
	b.raw("}")
	b.raw("}")
}

func issueJSONBytes(repo *RepoRec, issue *IssueRec) []byte {
	b := &jsonBuf{}
	issueJSON(b, repo, issue)
	return b.bytes()
}

func pullJSON(b *jsonBuf, repo *RepoRec, pull *PullRec) {
	headSha, ok := repo.Refs["refs/heads/"+pull.HeadRef]
	if !ok {
		headSha = pull.HeadSha
	}
	baseSha, ok := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok {
		baseSha = pull.BaseSha
	}
	b.raw("{")
	b.key("id").num(pull.DBID).raw(",")
	b.key("number").num(pull.Number).raw(",")
	b.key("state").str(pull.State).raw(",")
	b.key("title").str(pull.Title).raw(",")
	b.key("body").str(pull.Body).raw(",")
	b.key("head").raw("{")
	b.key("ref").str(pull.HeadRef).raw(",")
	b.key("sha").str(headSha).raw(",")
	b.key("label").str(repo.Owner + ":" + pull.HeadRef)
	b.raw("},")
	b.key("base").raw("{")
	b.key("ref").str(pull.BaseRef).raw(",")
	b.key("sha").str(baseSha).raw(",")
	b.key("label").str(repo.Owner + ":" + pull.BaseRef)
	b.raw("},")
	b.key("merged").boolean(pull.Merged).raw(",")
	b.key("merged_at")
	if pull.MergedAt != nil {
		b.str(*pull.MergedAt)
	} else {
		b.null()
	}
	b.raw(",")
	b.key("merge_commit_sha")
	if pull.MergeCommitSha != nil {
		b.str(*pull.MergeCommitSha)
	} else {
		b.null()
	}
	b.raw(",")
	b.key("user").raw("{")
	b.key("login").str(ViewerLogin).raw(",")
	b.key("id").num(ViewerID)
	b.raw("}")
	b.raw("}")
}

func pullJSONBytes(repo *RepoRec, pull *PullRec) []byte {
	b := &jsonBuf{}
	pullJSON(b, repo, pull)
	return b.bytes()
}

// GitHub-style Link header for a paged list, or nil when there is nothing more to page.
func linkHeader(origin, path string, page, perPage, total int) map[string]string {
	pages := (total + perPage - 1) / perPage
	if pages < 1 {
		pages = 1
	}
	if pages <= 1 {
		return nil
	}
	var parts []string
	u := func(p int) string {
		return origin + path + "?page=" + strconv.Itoa(p) + "&per_page=" + strconv.Itoa(perPage)
	}
	if page < pages {
		parts = append(parts, "<"+u(page+1)+">; rel=\"next\"", "<"+u(pages)+">; rel=\"last\"")
	}
	if page > 1 {
		parts = append(parts, "<"+u(page-1)+">; rel=\"prev\"", "<"+u(1)+">; rel=\"first\"")
	}
	if len(parts) == 0 {
		return nil
	}
	return map[string]string{"link": strings.Join(parts, ", ")}
}

func pageParams(query url.Values) (int, int) {
	page := 1
	if raw := query.Get("page"); raw != "" {
		if n, ok := jsNumber(raw); ok && n != 0 {
			page = int(n)
		}
	}
	if page < 1 {
		page = 1
	}
	perPage := 30
	if raw := query.Get("per_page"); raw != "" {
		if n, ok := jsNumber(raw); ok && n != 0 {
			perPage = int(n)
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	return page, perPage
}

type restCtx struct {
	forge    *Forge
	method   string
	segments []string
	query    url.Values
	body     map[string]any
	origin   string
	path     string
}

func routeRest(ctx *restCtx) restResult {
	segments := ctx.segments
	if len(segments) == 0 {
		if ctx.method != "GET" {
			return notFound()
		}
		return restResult{
			status:  200,
			body:    []byte(`{"current_user_url":"/api/v3/user","rate_limit_url":"/api/v3/rate_limit"}`),
			headers: map[string]string{"x-oauth-scopes": "repo, read:org"},
		}
	}
	if segments[0] == "meta" && len(segments) == 1 {
		if ctx.method != "GET" {
			return notFound()
		}
		return restResult{status: 200, body: []byte(`{"installed_version":"1.0.0","git_sha":"swiftforge","scalable":true}`)}
	}
	if segments[0] == "user" && len(segments) == 1 {
		if ctx.method != "GET" {
			return notFound()
		}
		b := &jsonBuf{}
		b.raw("{")
		b.key("login").str(ViewerLogin).raw(",")
		b.key("id").num(ViewerID).raw(",")
		b.key("name").str(ViewerName).raw(",")
		b.key("email").str(ViewerEmail).raw(",")
		b.key("type").str("User")
		b.raw("}")
		return restResult{status: 200, body: b.bytes()}
	}
	if segments[0] == "user" && len(segments) == 2 && segments[1] == "repos" {
		if ctx.method == "GET" {
			return listUserRepos(ctx)
		}
		if ctx.method == "POST" {
			return createRepo(ctx)
		}
		return notFound()
	}
	if segments[0] == "repos" && len(segments) >= 3 {
		owner, name := segments[1], segments[2]
		if owner != ViewerLogin {
			return notFound()
		}
		repo := ctx.forge.GetRepo(owner, name)
		if repo == nil {
			return notFound()
		}
		return routeRepo(ctx, repo, segments[3:])
	}
	return notFound()
}

func listUserRepos(ctx *restCtx) restResult {
	page, perPage := pageParams(ctx.query)
	all := ctx.forge.ReposOf(ViewerLogin)
	start := (page - 1) * perPage
	end := page * perPage
	if start > len(all) {
		start = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	b := &jsonBuf{}
	b.raw("[")
	for i, repo := range all[start:end] {
		if i > 0 {
			b.raw(",")
		}
		repoJSON(b, repo)
	}
	b.raw("]")
	return restResult{status: 200, body: b.bytes(), headers: linkHeader(ctx.origin, "/api/v3/user/repos", page, perPage, len(all))}
}

func createRepo(ctx *restCtx) restResult {
	body := ctx.body
	name, _ := asString(body["name"])
	if name == "" {
		return unprocessable("name is required")
	}
	if ctx.forge.GetRepo(ViewerLogin, name) != nil {
		return unprocessable("name already exists on this account")
	}
	description, _ := asString(body["description"])
	isPrivate := false
	if raw, present := body["private"]; present {
		isPrivate = jsTruthy(raw)
	}
	repo := ctx.forge.CreateRepo(ViewerLogin, name, description, isPrivate)
	return restResult{status: 201, body: repoJSONBytes(repo)}
}

// JS Boolean(v) for JSON values.
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	}
	return true
}

func routeRepo(ctx *restCtx, repo *RepoRec, rest []string) restResult {
	forge := ctx.forge
	method := ctx.method
	if len(rest) == 0 {
		switch method {
		case "GET":
			return restResult{status: 200, body: repoJSONBytes(repo)}
		case "PATCH":
			body := ctx.body
			if s, ok := asString(body["description"]); ok {
				repo.Description = s
			}
			if bv, ok := asBool(body["private"]); ok {
				repo.Private = bv
			}
			if s, ok := asString(body["name"]); ok && s != "" && s != repo.Name {
				if forge.GetRepo(repo.Owner, s) != nil {
					return unprocessable("name already exists on this account")
				}
				delete(forge.Repos, RepoKey(repo.Owner, repo.Name))
				repo.Name = s
				forge.Repos[RepoKey(repo.Owner, repo.Name)] = repo
			}
			return restResult{status: 200, body: repoJSONBytes(repo)}
		case "DELETE":
			forge.DeleteRepo(repo.Owner, repo.Name)
			return restResult{status: 204}
		}
		return notFound()
	}
	if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" {
		if len(rest) == 2 && method == "POST" {
			return createRef(ctx, repo)
		}
		if len(rest) >= 3 && method == "GET" {
			ref := "refs/" + strings.Join(rest[2:], "/")
			oid, ok := repo.Refs[ref]
			if !ok {
				return notFound()
			}
			typ := "commit"
			if object := forge.GetObject(oid); object != nil {
				typ = string(object.Type)
			}
			b := &jsonBuf{}
			b.raw("{")
			b.key("ref").str(ref).raw(",")
			b.key("object").raw("{")
			b.key("sha").str(oid).raw(",")
			b.key("type").str(typ)
			b.raw("}}")
			return restResult{status: 200, body: b.bytes()}
		}
		return notFound()
	}
	if rest[0] == "branches" {
		if len(rest) == 1 && method == "GET" {
			type branch struct {
				name string
				oid  string
			}
			var branches []branch
			for ref, oid := range repo.Refs {
				if strings.HasPrefix(ref, "refs/heads/") {
					branches = append(branches, branch{name: ref[len("refs/heads/"):], oid: oid})
				}
			}
			sort.Slice(branches, func(i, j int) bool { return branches[i].name < branches[j].name })
			b := &jsonBuf{}
			b.raw("[")
			for i, br := range branches {
				if i > 0 {
					b.raw(",")
				}
				b.raw("{")
				b.key("name").str(br.name).raw(",")
				b.key("commit").raw("{").key("sha").str(br.oid).raw("},")
				b.key("protected").boolean(false)
				b.raw("}")
			}
			b.raw("]")
			return restResult{status: 200, body: b.bytes()}
		}
		if len(rest) >= 2 && method == "GET" {
			branchName := strings.Join(rest[1:], "/")
			oid, ok := repo.Refs["refs/heads/"+branchName]
			if !ok {
				return notFound()
			}
			b := &jsonBuf{}
			b.raw("{")
			b.key("name").str(branchName).raw(",")
			b.key("commit").raw("{").key("sha").str(oid).raw("},")
			b.key("protected").boolean(false)
			b.raw("}")
			return restResult{status: 200, body: b.bytes()}
		}
		return notFound()
	}
	if rest[0] == "commits" && len(rest) == 1 && method == "GET" {
		return listCommits(ctx, repo)
	}
	if rest[0] == "contents" {
		return routeContents(ctx, repo, rest[1:])
	}
	if rest[0] == "labels" {
		if len(rest) == 1 && method == "GET" {
			b := &jsonBuf{}
			b.raw("[")
			for i, name := range repo.LabelOrder {
				if i > 0 {
					b.raw(",")
				}
				label := repo.Labels[name]
				b.raw("{")
				b.key("name").str(label.Name).raw(",")
				b.key("color").str(label.Color)
				b.raw("}")
			}
			b.raw("]")
			return restResult{status: 200, body: b.bytes()}
		}
		if len(rest) == 1 && method == "POST" {
			body := ctx.body
			name, _ := asString(body["name"])
			color := "ededed"
			if s, ok := asString(body["color"]); ok {
				color = strings.TrimPrefix(s, "#")
			}
			if name == "" {
				return unprocessable("name is required")
			}
			if _, ok := repo.Labels[name]; ok {
				return unprocessable("label already exists")
			}
			repo.AddLabel(name, color)
			b := &jsonBuf{}
			b.raw("{")
			b.key("name").str(name).raw(",")
			b.key("color").str(color)
			b.raw("}")
			return restResult{status: 201, body: b.bytes()}
		}
		return notFound()
	}
	if rest[0] == "issues" {
		return routeIssues(ctx, repo, rest[1:])
	}
	if rest[0] == "pulls" {
		return routePulls(ctx, repo, rest[1:])
	}
	return notFound()
}

func createRef(ctx *restCtx, repo *RepoRec) restResult {
	body := ctx.body
	ref, _ := asString(body["ref"])
	sha, _ := asString(body["sha"])
	object := ctx.forge.GetObject(sha)
	if !strings.HasPrefix(ref, "refs/") || object == nil {
		return unprocessable("invalid ref or sha")
	}
	if _, ok := repo.Refs[ref]; ok {
		return unprocessable("Reference already exists")
	}
	repo.Refs[ref] = sha
	ctx.forge.TouchRepo(repo)
	b := &jsonBuf{}
	b.raw("{")
	b.key("ref").str(ref).raw(",")
	b.key("object").raw("{")
	b.key("sha").str(sha).raw(",")
	b.key("type").str(string(object.Type))
	b.raw("}}")
	return restResult{status: 201, body: b.bytes()}
}

func listCommits(ctx *restCtx, repo *RepoRec) restResult {
	forge := ctx.forge
	shaParam := ctx.query.Get("sha")
	var tip string
	hasTip := false
	if shaParam != "" {
		if oid, ok := repo.Refs[shaParam]; ok && strings.HasPrefix(shaParam, "refs/") {
			tip, hasTip = oid, true
		} else if oid, ok := repo.Refs["refs/heads/"+shaParam]; ok {
			tip, hasTip = oid, true
		} else if object := forge.GetObject(shaParam); object != nil && object.Type == ObjCommit {
			tip, hasTip = shaParam, true
		}
	} else if oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		tip, hasTip = oid, true
	}
	if !hasTip {
		return restResult{status: 200, body: []byte("[]")}
	}
	var ordered []string
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		info := forge.CommitInfo(current)
		if info == nil {
			continue
		}
		ordered = append(ordered, current)
		queue = append(queue, info.Parents...)
	}
	page, perPage := pageParams(ctx.query)
	start := (page - 1) * perPage
	end := page * perPage
	if start > len(ordered) {
		start = len(ordered)
	}
	if end > len(ordered) {
		end = len(ordered)
	}
	type commitView struct {
		Sha    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Tree    struct {
				Sha string `json:"sha"`
			} `json:"tree"`
			Author struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"author"`
			Committer struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
		Parents []struct {
			Sha string `json:"sha"`
		} `json:"parents"`
	}
	views := []commitView{}
	for _, oid := range ordered[start:end] {
		object := forge.GetObject(oid)
		info := parseCommit(object.Data)
		stamp := isoFromSec(info.Time)
		var view commitView
		view.Sha = oid
		view.Commit.Message = info.Message
		view.Commit.Tree.Sha = info.Tree
		view.Commit.Author.Name = ViewerName
		view.Commit.Author.Email = ViewerEmail
		view.Commit.Author.Date = stamp
		view.Commit.Committer.Name = ViewerName
		view.Commit.Committer.Email = ViewerEmail
		view.Commit.Committer.Date = stamp
		view.Parents = []struct {
			Sha string `json:"sha"`
		}{}
		for _, parent := range info.Parents {
			view.Parents = append(view.Parents, struct {
				Sha string `json:"sha"`
			}{Sha: parent})
		}
		views = append(views, view)
	}
	return restJSON(200, views)
}

// ---------------------------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------------------------

// Resolve the branch a contents write targets, creating the ref when the suite's flow needs it.
func writableBranch(repo *RepoRec, requested string, hasRequested bool) string {
	branch := repo.DefaultBranch
	if hasRequested {
		branch = requested
	}
	ref := "refs/heads/" + branch
	if _, ok := repo.Refs[ref]; !ok && len(repo.Refs) > 0 {
		if baseTip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			repo.Refs[ref] = baseTip
		}
	}
	return branch
}

func commitFlat(ctx *restCtx, repo *RepoRec, branch string, flat map[string]FlatEntry, message string) string {
	forge := ctx.forge
	tip, hasTip := repo.Refs["refs/heads/"+branch]
	tree := forge.TreeFromFlat(flat)
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	sha := forge.MakeCommit(tree, parents, message)
	repo.Refs["refs/heads/"+branch] = sha
	forge.TouchRepo(repo)
	return sha
}

func routeContents(ctx *restCtx, repo *RepoRec, pathSegments []string) restResult {
	forge := ctx.forge
	path := strings.Join(pathSegments, "/")
	switch ctx.method {
	case "GET":
		refParam := ctx.query.Get("ref")
		if refParam == "" {
			refParam = repo.DefaultBranch
		}
		var tip string
		hasTip := false
		if oid, ok := repo.Refs["refs/heads/"+refParam]; ok {
			tip, hasTip = oid, true
		} else if object := forge.GetObject(refParam); object != nil && object.Type == ObjCommit {
			tip, hasTip = refParam, true
		}
		if !hasTip {
			return notFound()
		}
		info := forge.CommitInfo(tip)
		if info == nil {
			return notFound()
		}
		if path != "" {
			flat := forge.FlattenTree(info.Tree)
			if file, ok := flat[path]; ok {
				object := forge.GetObject(file.OID)
				if object == nil {
					return notFound()
				}
				b := &jsonBuf{}
				b.raw("{")
				b.key("type").str("file").raw(",")
				b.key("name").str(path[strings.LastIndex(path, "/")+1:]).raw(",")
				b.key("path").str(path).raw(",")
				b.key("sha").str(file.OID).raw(",")
				b.key("size").num(len(object.Data)).raw(",")
				b.key("encoding").str("base64").raw(",")
				b.key("content").str(base64.StdEncoding.EncodeToString(object.Data))
				b.raw("}")
				return restResult{status: 200, body: b.bytes()}
			}
		}
		treeOID := info.Tree
		if path != "" {
			for _, segment := range strings.Split(path, "/") {
				object := forge.GetObject(treeOID)
				if object == nil || object.Type != ObjTree {
					return notFound()
				}
				found := false
				for _, entry := range parseTree(object.Data) {
					if entry.Name == segment && entry.Mode == "40000" {
						treeOID = entry.OID
						found = true
						break
					}
				}
				if !found {
					return notFound()
				}
			}
		}
		treeObject := forge.GetObject(treeOID)
		if treeObject == nil || treeObject.Type != ObjTree {
			return notFound()
		}
		b := &jsonBuf{}
		b.raw("[")
		for i, entry := range parseTree(treeObject.Data) {
			if i > 0 {
				b.raw(",")
			}
			typ := "file"
			if entry.Mode == "40000" {
				typ = "dir"
			}
			entryPath := entry.Name
			if path != "" {
				entryPath = path + "/" + entry.Name
			}
			b.raw("{")
			b.key("type").str(typ).raw(",")
			b.key("name").str(entry.Name).raw(",")
			b.key("path").str(entryPath).raw(",")
			b.key("sha").str(entry.OID)
			b.raw("}")
		}
		b.raw("]")
		return restResult{status: 200, body: b.bytes()}
	case "PUT":
		body := ctx.body
		message, hasMessage := asString(body["message"])
		content, hasContent := asString(body["content"])
		if !hasMessage || !hasContent || path == "" {
			return unprocessable("message, content and a path are required")
		}
		branchName, hasBranch := asString(body["branch"])
		branch := writableBranch(repo, branchName, hasBranch)
		tip, hasTip := repo.Refs["refs/heads/"+branch]
		var flat map[string]FlatEntry
		if hasTip {
			flat = forge.TreeOfCommit(tip)
		} else {
			flat = map[string]FlatEntry{}
		}
		existing, hasExisting := flat[path]
		if hasExisting {
			if body["sha"] != existing.OID {
				return restResult{status: 409, body: []byte(`{"message":"the sha does not match the current blob"}`)}
			}
		} else if s, ok := asString(body["sha"]); ok && s != "" {
			return notFound()
		}
		data, _ := base64.StdEncoding.DecodeString(content)
		blobOID := forge.PutObject(ObjBlob, data)
		flat[path] = FlatEntry{Mode: "100644", OID: blobOID}
		commitSha := commitFlat(ctx, repo, branch, flat, message)
		status := 201
		if hasExisting {
			status = 200
		}
		b := &jsonBuf{}
		b.raw("{")
		b.key("content").raw("{")
		b.key("name").str(path[strings.LastIndex(path, "/")+1:]).raw(",")
		b.key("path").str(path).raw(",")
		b.key("sha").str(blobOID).raw(",")
		b.key("size").num(len(data)).raw(",")
		b.key("type").str("file").raw(",")
		b.key("encoding").str("base64")
		b.raw("},")
		b.key("commit").raw("{").key("sha").str(commitSha).raw("}")
		b.raw("}")
		return restResult{status: status, body: b.bytes()}
	case "DELETE":
		body := ctx.body
		message, hasMessage := asString(body["message"])
		sha, hasSha := asString(body["sha"])
		if !hasMessage || !hasSha || path == "" {
			return unprocessable("message and sha are required")
		}
		branchName, hasBranch := asString(body["branch"])
		branch := writableBranch(repo, branchName, hasBranch)
		tip, hasTip := repo.Refs["refs/heads/"+branch]
		if !hasTip {
			return notFound()
		}
		flat := forge.TreeOfCommit(tip)
		existing, hasExisting := flat[path]
		if !hasExisting {
			return notFound()
		}
		if sha != existing.OID {
			return restResult{status: 409, body: []byte(`{"message":"the sha does not match the current blob"}`)}
		}
		delete(flat, path)
		commitSha := commitFlat(ctx, repo, branch, flat, message)
		b := &jsonBuf{}
		b.raw("{")
		b.key("content").null().raw(",")
		b.key("commit").raw("{").key("sha").str(commitSha).raw("}")
		b.raw("}")
		return restResult{status: 200, body: b.bytes()}
	}
	return notFound()
}

// ---------------------------------------------------------------------------------------------
// issues
// ---------------------------------------------------------------------------------------------

// JS Number(x) restricted to the segment shapes routes see; ok=false mirrors NaN/non-integer.
func segmentNumber(s string) (int, bool) {
	f, ok := jsNumber(s)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

func routeIssues(ctx *restCtx, repo *RepoRec, rest []string) restResult {
	forge := ctx.forge
	if len(rest) == 0 {
		switch ctx.method {
		case "GET":
			state := ctx.query.Get("state")
			if state == "" {
				state = "open"
			}
			direction := strings.ToLower(ctx.query.Get("direction"))
			if direction == "" {
				direction = "desc"
			}
			var issues []*IssueRec
			for _, issue := range repo.Issues {
				issues = append(issues, issue)
			}
			if state == "open" {
				filtered := issues[:0]
				for _, issue := range issues {
					if issue.State == "open" {
						filtered = append(filtered, issue)
					}
				}
				issues = filtered
			} else if state == "closed" {
				filtered := issues[:0]
				for _, issue := range issues {
					if issue.State == "closed" {
						filtered = append(filtered, issue)
					}
				}
				issues = filtered
			}
			sort.Slice(issues, func(i, j int) bool {
				if direction == "asc" {
					return issues[i].Number < issues[j].Number
				}
				return issues[i].Number > issues[j].Number
			})
			page, perPage := pageParams(ctx.query)
			start := (page - 1) * perPage
			end := page * perPage
			if start > len(issues) {
				start = len(issues)
			}
			if end > len(issues) {
				end = len(issues)
			}
			b := &jsonBuf{}
			b.raw("[")
			for i, issue := range issues[start:end] {
				if i > 0 {
					b.raw(",")
				}
				issueJSON(b, repo, issue)
			}
			b.raw("]")
			return restResult{status: 200, body: b.bytes()}
		case "POST":
			body := ctx.body
			title, _ := asString(body["title"])
			if title == "" {
				return unprocessable("title is required")
			}
			var labels []string
			if rawLabels, ok := body["labels"].([]any); ok {
				for _, raw := range rawLabels {
					name := ""
					if s, ok := raw.(string); ok {
						name = s
					} else if obj, ok := raw.(map[string]any); ok {
						name, _ = asString(obj["name"])
					}
					if name == "" {
						continue
					}
					if _, ok := repo.Labels[name]; !ok {
						return unprocessable("unknown label: " + name)
					}
					labels = append(labels, name)
				}
			}
			bodyText, _ := asString(body["body"])
			issue := forge.CreateIssue(repo, title, bodyText, labels)
			return restResult{status: 201, body: issueJSONBytes(repo, issue)}
		}
		return notFound()
	}
	number, isNumber := segmentNumber(rest[0])
	var issue *IssueRec
	if isNumber {
		issue = repo.Issues[number]
	}
	if len(rest) == 1 {
		if issue == nil {
			return notFound()
		}
		switch ctx.method {
		case "GET":
			return restResult{status: 200, body: issueJSONBytes(repo, issue)}
		case "PATCH":
			body := ctx.body
			if s, ok := asString(body["state"]); ok && (s == "open" || s == "closed") {
				issue.State = s
			}
			if s, ok := asString(body["title"]); ok {
				issue.Title = s
			}
			if s, ok := asString(body["body"]); ok {
				issue.Body = s
			}
			return restResult{status: 200, body: issueJSONBytes(repo, issue)}
		}
		return notFound()
	}
	if len(rest) == 2 && rest[1] == "comments" {
		if issue == nil {
			return notFound()
		}
		switch ctx.method {
		case "GET":
			b := &jsonBuf{}
			b.raw("[")
			for i, comment := range issue.Comments {
				if i > 0 {
					b.raw(",")
				}
				b.raw("{")
				b.key("id").num(comment.ID).raw(",")
				b.key("body").str(comment.Body)
				b.raw("}")
			}
			b.raw("]")
			return restResult{status: 200, body: b.bytes()}
		case "POST":
			body := ctx.body
			text, _ := asString(body["body"])
			if text == "" {
				return unprocessable("body is required")
			}
			comment := forge.AddComment(repo, issue, text)
			b := &jsonBuf{}
			b.raw("{")
			b.key("id").num(comment.ID).raw(",")
			b.key("body").str(comment.Body)
			b.raw("}")
			return restResult{status: 201, body: b.bytes()}
		}
	}
	return notFound()
}

// ---------------------------------------------------------------------------------------------
// pulls
// ---------------------------------------------------------------------------------------------

func stripOwnerPrefix(ref string) string {
	if idx := strings.Index(ref, ":"); idx >= 0 {
		return ref[idx+1:]
	}
	return ref
}

func routePulls(ctx *restCtx, repo *RepoRec, rest []string) restResult {
	forge := ctx.forge
	if len(rest) == 0 {
		switch ctx.method {
		case "GET":
			state := ctx.query.Get("state")
			if state == "" {
				state = "open"
			}
			var pulls []*PullRec
			for _, pull := range repo.Pulls {
				pulls = append(pulls, pull)
			}
			if state == "open" || state == "closed" {
				filtered := pulls[:0]
				for _, pull := range pulls {
					if pull.State == state {
						filtered = append(filtered, pull)
					}
				}
				pulls = filtered
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
			page, perPage := pageParams(ctx.query)
			start := (page - 1) * perPage
			end := page * perPage
			if start > len(pulls) {
				start = len(pulls)
			}
			if end > len(pulls) {
				end = len(pulls)
			}
			b := &jsonBuf{}
			b.raw("[")
			for i, pull := range pulls[start:end] {
				if i > 0 {
					b.raw(",")
				}
				pullJSON(b, repo, pull)
			}
			b.raw("]")
			return restResult{status: 200, body: b.bytes()}
		case "POST":
			body := ctx.body
			title, _ := asString(body["title"])
			head := ""
			if s, ok := asString(body["head"]); ok {
				head = stripOwnerPrefix(s)
			}
			base := ""
			if s, ok := asString(body["base"]); ok {
				base = stripOwnerPrefix(s)
			}
			if title == "" || head == "" || base == "" {
				return unprocessable("title, head and base are required")
			}
			if head == base {
				return unprocessable("head and base must differ")
			}
			headSha, hasHead := repo.Refs["refs/heads/"+head]
			if !hasHead {
				return unprocessable("head ref " + head + " does not exist")
			}
			baseSha, hasBase := repo.Refs["refs/heads/"+base]
			if !hasBase {
				return unprocessable("base ref " + base + " does not exist")
			}
			headCommit := forge.PeelToCommit(headSha)
			baseCommit := forge.PeelToCommit(baseSha)
			if headCommit == "" || baseCommit == "" || headCommit == baseCommit || forge.IsAncestor(headCommit, baseCommit) {
				return unprocessable("No commits between the base and the head")
			}
			for _, pull := range repo.Pulls {
				if pull.State == "open" && pull.HeadRef == head && pull.BaseRef == base {
					return unprocessable("A pull request already exists for this head and base")
				}
			}
			bodyText, _ := asString(body["body"])
			pull := forge.CreatePull(repo, title, bodyText, head, base, headSha, baseSha)
			return restResult{status: 201, body: pullJSONBytes(repo, pull)}
		}
		return notFound()
	}
	number, isNumber := segmentNumber(rest[0])
	var pull *PullRec
	if isNumber {
		pull = repo.Pulls[number]
	}
	if pull == nil {
		return notFound()
	}
	if len(rest) == 1 && ctx.method == "GET" {
		return restResult{status: 200, body: pullJSONBytes(repo, pull)}
	}
	if len(rest) == 2 && rest[1] == "files" && ctx.method == "GET" {
		baseTip, ok := repo.Refs["refs/heads/"+pull.BaseRef]
		if !ok {
			baseTip = pull.BaseSha
		}
		headTip, ok := repo.Refs["refs/heads/"+pull.HeadRef]
		if !ok {
			headTip = pull.HeadSha
		}
		diff := forge.DiffFlat(forge.TreeOfCommit(baseTip), forge.TreeOfCommit(headTip))
		return restJSON(200, diff)
	}
	if len(rest) == 2 && rest[1] == "merge" && ctx.method == "PUT" {
		body := ctx.body
		methodName, _ := asString(body["merge_method"])
		if methodName == "" {
			methodName = "merge"
		}
		if pull.State != "open" || pull.Merged {
			return restResult{status: 405, body: []byte(`{"message":"Pull Request is not mergeable"}`)}
		}
		mergeMethod := "merge"
		if methodName == "squash" {
			mergeMethod = "squash"
		}
		sha := forge.MergePull(repo, pull, mergeMethod)
		if sha == "" {
			return restResult{status: 405, body: []byte(`{"message":"Pull Request is not mergeable: merge conflict"}`)}
		}
		b := &jsonBuf{}
		b.raw("{")
		b.key("sha").str(sha).raw(",")
		b.key("merged").boolean(true).raw(",")
		b.key("message").str("Pull Request successfully merged")
		b.raw("}")
		return restResult{status: 200, body: b.bytes()}
	}
	return notFound()
}
