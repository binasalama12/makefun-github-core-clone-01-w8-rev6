// forge.go — the in-memory domain store: repositories, refs, issues, pull requests,
// comments and labels, plus the commit-graph walks the git transport and merge logic share.
// A from-scratch Go implementation of the forge's domain model.

package main

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ViewerLogin = "adaiversen"
	ViewerID    = 1000
	ViewerName  = "ada iversen"
	ViewerEmail = "adaiversen@example.invalid"
)

type LabelRec struct {
	Name  string
	Color string
}

type CommentRec struct {
	ID   int
	Body string
}

type IssueRec struct {
	Number   int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Comments []CommentRec
	DBID     int
}

type PullRec struct {
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
	DBID           int
	Comments       []CommentRec // PR comments attached by the GraphQL addComment mutation
}

type StoredObject struct {
	Type ObjType
	Data []byte
}

type RepoRec struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Refs          map[string]string
	Issues        map[int]*IssueRec
	Pulls         map[int]*PullRec
	Labels        map[string]*LabelRec
	LabelOrder    []string
	NextNumber    int
	PackCache     map[string][]byte
}

type Forge struct {
	mu         sync.RWMutex
	cacheMu    sync.Mutex
	Objects    map[string]*StoredObject
	Repos      map[string]*RepoRec
	repoSeq    int
	dbSeq      int
	commentSeq int
}

func NewForge() *Forge {
	return &Forge{
		Objects:    map[string]*StoredObject{},
		Repos:      map[string]*RepoRec{},
		repoSeq:    5000,
		dbSeq:      9000,
		commentSeq: 1,
	}
}

// ---- objects -------------------------------------------------------------

func (f *Forge) PutObject(t ObjType, data []byte) string {
	oid := hashObject(t, data)
	if _, ok := f.Objects[oid]; !ok {
		f.Objects[oid] = &StoredObject{Type: t, Data: data}
	}
	return oid
}

func (f *Forge) GetObject(oid string) *StoredObject {
	return f.Objects[oid]
}

func (f *Forge) LookupPack(oid string) *PackObject {
	found := f.Objects[oid]
	if found == nil {
		return nil
	}
	return &PackObject{OID: oid, Type: found.Type, Data: found.Data}
}

// Follow tag objects down to the commit they name; "" when the chain breaks.
func (f *Forge) PeelToCommit(oid string) string {
	current := oid
	for hops := 0; hops < 16; hops++ {
		object := f.Objects[current]
		if object == nil {
			return ""
		}
		if object.Type == ObjCommit {
			return current
		}
		if object.Type != ObjTag {
			return ""
		}
		current = parseTag(object.Data).Object
	}
	return ""
}

// One tag hop: the oid a tag object points at, or "" for non-tags and missing objects.
func (f *Forge) PeelStep(oid string) string {
	object := f.Objects[oid]
	if object == nil || object.Type != ObjTag {
		return ""
	}
	return parseTag(object.Data).Object
}

func (f *Forge) CommitInfo(oid string) *CommitInfo {
	object := f.Objects[oid]
	if object == nil || object.Type != ObjCommit {
		return nil
	}
	info := parseCommit(object.Data)
	return &info
}

// Every commit reachable from `tip` (inclusive), following parents.
func (f *Forge) AncestorsOf(tip string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[current] {
			continue
		}
		info := f.CommitInfo(current)
		if info == nil {
			continue
		}
		seen[current] = true
		stack = append(stack, info.Parents...)
	}
	return seen
}

// True when `ancestor` is `tip` itself or one of its ancestors. DFS with early exit.
func (f *Forge) IsAncestor(ancestor, tip string) bool {
	if ancestor == tip {
		return true
	}
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current == ancestor {
			return true
		}
		if seen[current] {
			continue
		}
		seen[current] = true
		info := f.CommitInfo(current)
		if info == nil {
			continue
		}
		stack = append(stack, info.Parents...)
	}
	return false
}

// The best common ancestor of two commits (first hit of a BFS from `b` over ancestors of `a`).
func (f *Forge) MergeBase(a, b string) string {
	if a == b {
		return a
	}
	ancestors := f.AncestorsOf(a)
	seen := map[string]bool{}
	queue := []string{b}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		if ancestors[current] {
			return current
		}
		info := f.CommitInfo(current)
		if info == nil {
			continue
		}
		queue = append(queue, info.Parents...)
	}
	return ""
}

type FlatEntry struct {
	Mode string
	OID  string
}

// Flatten a tree (recursively) into path -> {mode, oid}.
func (f *Forge) FlattenTree(treeOID string) map[string]FlatEntry {
	out := map[string]FlatEntry{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		object := f.Objects[oid]
		if object == nil || object.Type != ObjTree {
			return
		}
		for _, entry := range parseTree(object.Data) {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.OID, path)
			} else {
				out[path] = FlatEntry{Mode: entry.Mode, OID: entry.OID}
			}
		}
	}
	walk(treeOID, "")
	return out
}

func (f *Forge) TreeOfCommit(commitOID string) map[string]FlatEntry {
	info := f.CommitInfo(commitOID)
	if info == nil {
		return map[string]FlatEntry{}
	}
	return f.FlattenTree(info.Tree)
}

type treeNode struct {
	files map[string]FlatEntry
	dirs  map[string]*treeNode
}

// Build (and store) nested trees from a flat path map; answers the root tree oid.
func (f *Forge) TreeFromFlat(flat map[string]FlatEntry) string {
	newNode := func() *treeNode { return &treeNode{files: map[string]FlatEntry{}, dirs: map[string]*treeNode{}} }
	root := newNode()
	for path, entry := range flat {
		segments := strings.Split(path, "/")
		node := root
		for i := 0; i < len(segments)-1; i++ {
			seg := segments[i]
			if _, ok := node.dirs[seg]; !ok {
				node.dirs[seg] = newNode()
			}
			node = node.dirs[seg]
		}
		node.files[segments[len(segments)-1]] = entry
	}
	var build func(node *treeNode) string
	build = func(node *treeNode) string {
		var entries []TreeEntry
		for name, entry := range node.files {
			entries = append(entries, TreeEntry{Mode: entry.Mode, Name: name, OID: entry.OID})
		}
		for name, child := range node.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return f.PutObject(ObjTree, buildTree(entries))
	}
	return build(root)
}

// Store a commit object with the forge identity; answers its oid.
func (f *Forge) MakeCommit(treeOID string, parents []string, message string, timeSec ...int64) string {
	var t int64
	if len(timeSec) > 0 {
		t = timeSec[0]
	} else {
		t = time.Now().Unix()
	}
	identity := ViewerName + " <" + ViewerEmail + "> " + strconv.FormatInt(t, 10) + " +0000"
	lines := []string{"tree " + treeOID}
	for _, parent := range parents {
		lines = append(lines, "parent "+parent)
	}
	lines = append(lines, "author "+identity, "committer "+identity, "", message)
	return f.PutObject(ObjCommit, []byte(strings.Join(lines, "\n")))
}

// ---- repositories ----------------------------------------------------------

func RepoKey(owner, name string) string { return owner + "/" + name }

func (f *Forge) GetRepo(owner, name string) *RepoRec {
	return f.Repos[RepoKey(owner, name)]
}

func (f *Forge) CreateRepo(owner, name, description string, isPrivate bool) *RepoRec {
	repo := &RepoRec{
		ID:            f.repoSeq,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       isPrivate,
		DefaultBranch: "main",
		Refs:          map[string]string{},
		Issues:        map[int]*IssueRec{},
		Pulls:         map[int]*PullRec{},
		Labels:        map[string]*LabelRec{},
		NextNumber:    1,
		PackCache:     map[string][]byte{},
	}
	f.repoSeq++
	f.Repos[RepoKey(owner, name)] = repo
	return repo
}

func (f *Forge) DeleteRepo(owner, name string) bool {
	key := RepoKey(owner, name)
	if _, ok := f.Repos[key]; !ok {
		return false
	}
	delete(f.Repos, key)
	return true
}

func (f *Forge) ReposOf(owner string) []*RepoRec {
	var out []*RepoRec
	for _, repo := range f.Repos {
		if repo.Owner == owner {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Any ref change invalidates the repo's cached packs.
func (f *Forge) TouchRepo(repo *RepoRec) {
	f.cacheMu.Lock()
	repo.PackCache = map[string][]byte{}
	f.cacheMu.Unlock()
}

func (r *RepoRec) AddLabel(name, color string) {
	if _, ok := r.Labels[name]; !ok {
		r.LabelOrder = append(r.LabelOrder, name)
	}
	r.Labels[name] = &LabelRec{Name: name, Color: color}
}

// ---- issues & pulls ---------------------------------------------------------

func (f *Forge) CreateIssue(repo *RepoRec, title, body string, labels []string) *IssueRec {
	issue := &IssueRec{
		Number: repo.NextNumber,
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: append([]string{}, labels...),
		DBID:   f.dbSeq,
	}
	repo.NextNumber++
	f.dbSeq++
	repo.Issues[issue.Number] = issue
	return issue
}

func (f *Forge) AddComment(repo *RepoRec, issue *IssueRec, body string) *CommentRec {
	comment := &CommentRec{ID: f.commentSeq, Body: body}
	f.commentSeq++
	issue.Comments = append(issue.Comments, *comment)
	return comment
}

func (f *Forge) CreatePull(repo *RepoRec, title, body, headRef, baseRef, headSha, baseSha string) *PullRec {
	pull := &PullRec{
		Number:  repo.NextNumber,
		Title:   title,
		Body:    body,
		State:   "open",
		HeadRef: headRef,
		BaseRef: baseRef,
		HeadSha: headSha,
		BaseSha: baseSha,
		DBID:    f.dbSeq,
	}
	repo.NextNumber++
	f.dbSeq++
	repo.Pulls[pull.Number] = pull
	return pull
}

type DiffEntry struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

// Per-path diff between two flat trees: status plus a simple line-level add/delete count.
func (f *Forge) DiffFlat(base, head map[string]FlatEntry) []DiffEntry {
	pathSet := map[string]bool{}
	for p := range base {
		pathSet[p] = true
	}
	for p := range head {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	linesOf := func(oid string) []string {
		object := f.Objects[oid]
		if object == nil {
			return nil
		}
		return strings.Split(string(object.Data), "\n")
	}
	var out []DiffEntry
	for _, path := range paths {
		before, hasBefore := base[path]
		after, hasAfter := head[path]
		switch {
		case hasBefore && !hasAfter:
			deletions := len(linesOf(before.OID))
			out = append(out, DiffEntry{Filename: path, Status: "removed", Additions: 0, Deletions: deletions, Changes: deletions})
		case !hasBefore && hasAfter:
			additions := len(linesOf(after.OID))
			out = append(out, DiffEntry{Filename: path, Status: "added", Additions: additions, Deletions: 0, Changes: additions})
		case hasBefore && hasAfter && before.OID != after.OID:
			a := linesOf(before.OID)
			b := linesOf(after.OID)
			common := lcsLength(a, b)
			deletions := len(a) - common
			additions := len(b) - common
			out = append(out, DiffEntry{Filename: path, Status: "modified", Additions: additions, Deletions: deletions, Changes: additions + deletions})
		}
	}
	if out == nil {
		out = []DiffEntry{}
	}
	return out
}

type MergeAnalysis struct {
	Conflict  bool
	MergeBase string
}

// Mergeability of head into base: a conflict is a path both sides changed since the merge base
// with different outcomes.
func (f *Forge) MergeAnalysis(repo *RepoRec, pull *PullRec) MergeAnalysis {
	baseTip, ok := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok {
		baseTip = pull.BaseSha
	}
	headTip, ok := repo.Refs["refs/heads/"+pull.HeadRef]
	if !ok {
		headTip = pull.HeadSha
	}
	base := f.MergeBase(baseTip, headTip)
	if base == "" {
		return MergeAnalysis{Conflict: true, MergeBase: ""}
	}
	baseTree := f.TreeOfCommit(base)
	baseSide := f.TreeOfCommit(baseTip)
	headSide := f.TreeOfCommit(headTip)
	pathSet := map[string]bool{}
	for p := range baseTree {
		pathSet[p] = true
	}
	for p := range baseSide {
		pathSet[p] = true
	}
	for p := range headSide {
		pathSet[p] = true
	}
	for path := range pathSet {
		var origin, left, right string
		if e, ok := baseTree[path]; ok {
			origin = e.OID
		}
		if e, ok := baseSide[path]; ok {
			left = e.OID
		}
		if e, ok := headSide[path]; ok {
			right = e.OID
		}
		if left == right {
			continue
		}
		if left == origin || right == origin {
			continue
		}
		return MergeAnalysis{Conflict: true, MergeBase: base}
	}
	return MergeAnalysis{Conflict: false, MergeBase: base}
}

// Perform the merge of an open pull request. Squash lands the head tree on top of the base tip;
// a merge commit joins both parents with a three-way tree. Returns the new commit oid, or ""
// when the merge cannot run.
func (f *Forge) MergePull(repo *RepoRec, pull *PullRec, method string) string {
	if pull.State != "open" || pull.Merged {
		return ""
	}
	baseRef := "refs/heads/" + pull.BaseRef
	baseTip, ok := repo.Refs[baseRef]
	if !ok {
		return ""
	}
	headTip, ok := repo.Refs["refs/heads/"+pull.HeadRef]
	if !ok {
		return ""
	}
	analysis := f.MergeAnalysis(repo, pull)
	if analysis.Conflict || analysis.MergeBase == "" {
		return ""
	}
	var treeOID string
	var parents []string
	var message string
	if method == "squash" {
		headInfo := f.CommitInfo(headTip)
		if headInfo == nil {
			return ""
		}
		treeOID = headInfo.Tree
		parents = []string{baseTip}
		message = pull.Title + " (#" + strconv.Itoa(pull.Number) + ")\n\n" + pull.Body
	} else {
		origin := f.TreeOfCommit(analysis.MergeBase)
		baseSide := f.TreeOfCommit(baseTip)
		headSide := f.TreeOfCommit(headTip)
		merged := map[string]FlatEntry{}
		pathSet := map[string]bool{}
		for p := range origin {
			pathSet[p] = true
		}
		for p := range baseSide {
			pathSet[p] = true
		}
		for p := range headSide {
			pathSet[p] = true
		}
		for path := range pathSet {
			o, hasO := origin[path]
			l, hasL := baseSide[path]
			r, hasR := headSide[path]
			if hasL && hasR && l.OID == r.OID {
				merged[path] = l
			} else if hasO && hasL && o.OID == l.OID {
				if hasR {
					merged[path] = r
				}
			} else if hasO && hasR && o.OID == r.OID {
				if hasL {
					merged[path] = l
				}
			} else if !hasO && hasL && !hasR {
				merged[path] = l
			} else if !hasO && !hasL && hasR {
				merged[path] = r
			} else if !hasL && !hasR {
				// deleted on both sides
			} else if hasO && !hasL && hasR && o.OID == r.OID {
				// deleted on base side, unchanged on head side
			} else if hasO && hasL && !hasR && o.OID == l.OID {
				// deleted on head side, unchanged on base side
			} else {
				return "" // conflict (already excluded above, kept as a guard)
			}
			_ = hasO
		}
		treeOID = f.TreeFromFlat(merged)
		parents = []string{baseTip, headTip}
		message = "Merge pull request #" + strconv.Itoa(pull.Number) + " from " + pull.HeadRef + "\n\n" + pull.Title
	}
	sha := f.MakeCommit(treeOID, parents, message)
	repo.Refs[baseRef] = sha
	f.TouchRepo(repo)
	pull.Merged = true
	pull.State = "closed"
	now := isoNow()
	pull.MergedAt = &now
	pull.MergeCommitSha = &sha
	return sha
}

// Length of the longest common subsequence of two line arrays (files are small).
func lcsLength(a, b []string) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				current[j] = previous[j-1] + 1
			} else if previous[j] > current[j-1] {
				current[j] = previous[j]
			} else {
				current[j] = current[j-1]
			}
		}
		previous, current = current, previous
		for j := range current {
			current[j] = 0
		}
	}
	return previous[len(b)]
}

// ISO-8601 timestamps in the exact shape JS Date.toISOString() produces.
func isoFromMillis(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func isoNow() string {
	now := time.Now().UTC()
	return now.Format("2006-01-02T15:04:05.000Z")
}

func isoFromSec(sec int64) string {
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}
