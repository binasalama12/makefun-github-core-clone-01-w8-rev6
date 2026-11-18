// gitproto.go — the git smart-HTTP transport: ref advertisement (v0 and v2), ls-refs,
// upload-pack (with shallow/deepen/filter planning and a per-repo pack cache) and
// receive-pack (with report-status). One inflate per pack entry on the way in, one
// hash per object, ancestor walks that stop as early as they can.

package main

import (
	"bytes"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const agentString = "swiftforge/1.0"

// ---------------------------------------------------------------------------------------------
// advertisement
// ---------------------------------------------------------------------------------------------

func sortedRefs(repo *RepoRec) [][2]string {
	refs := make([][2]string, 0, len(repo.Refs))
	for ref, oid := range repo.Refs {
		refs = append(refs, [2]string{ref, oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i][0] < refs[j][0] })
	return refs
}

func uploadCaps(repo *RepoRec) string {
	return strings.Join([]string{
		"multi_ack_detailed",
		"side-band-64k",
		"thin-pack",
		"ofs-delta",
		"shallow",
		"deepen-since",
		"deepen-not",
		"filter",
		"agent=" + agentString,
		"symref=HEAD:refs/heads/" + repo.DefaultBranch,
		"object-format=sha1",
	}, " ")
}

func receiveCaps() string {
	return strings.Join([]string{"report-status", "delete-refs", "side-band-64k", "ofs-delta", "agent=" + agentString, "object-format=sha1"}, " ")
}

// The v0 ref advertisement body (after the service preamble): refs, peeled tags, HEAD first.
func v0RefLines(forge *Forge, repo *RepoRec, caps string) []byte {
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		return pktLineStr(ZeroOID + " capabilities^{}\x00" + caps)
	}
	var buf bytes.Buffer
	headTarget := "refs/heads/" + repo.DefaultBranch
	headOID, hasHead := repo.Refs[headTarget]
	first := true
	if hasHead {
		buf.Write(pktLineStr(headOID + " HEAD\x00" + caps))
		first = false
	}
	for _, pair := range refs {
		ref, oid := pair[0], pair[1]
		if first {
			buf.Write(pktLineStr(oid + " " + ref + "\x00" + caps))
		} else {
			buf.Write(pktLineStr(oid + " " + ref))
		}
		first = false
		if object := forge.GetObject(oid); object != nil && object.Type == ObjTag {
			if peeled := forge.PeelToCommit(oid); peeled != "" {
				buf.Write(pktLineStr(peeled + " " + ref + "^{}"))
			}
		}
	}
	return buf.Bytes()
}

func advertisement(forge *Forge, repo *RepoRec, service string, version2 bool) []byte {
	var buf bytes.Buffer
	buf.Write(pktLineStr("# service=" + service + "\n"))
	buf.Write(pktFlushBytes)
	if service == "git-receive-pack" || !version2 {
		caps := uploadCaps(repo)
		if service == "git-receive-pack" {
			caps = receiveCaps()
		}
		buf.Write(v0RefLines(forge, repo, caps))
		buf.Write(pktFlushBytes)
		return buf.Bytes()
	}
	for _, line := range []string{
		"version 2\n",
		"agent=" + agentString + "\n",
		"ls-refs=unborn\n",
		"fetch=shallow filter wait-for-done\n",
		"object-format=sha1\n",
	} {
		buf.Write(pktLineStr(line))
	}
	buf.Write(pktFlushBytes)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------------------------
// ls-refs (protocol v2)
// ---------------------------------------------------------------------------------------------

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

func lsRefs(forge *Forge, repo *RepoRec, args lsRefsArgs) []byte {
	var buf bytes.Buffer
	headTarget := "refs/heads/" + repo.DefaultBranch
	headOID, hasHead := repo.Refs[headTarget]
	headIncluded := len(args.prefixes) == 0
	for _, prefix := range args.prefixes {
		if strings.HasPrefix("HEAD", prefix) || strings.HasPrefix(prefix, "HEAD") {
			headIncluded = true
		}
	}
	if headIncluded {
		attrs := ""
		if args.symrefs {
			attrs = " symref-target:" + headTarget
		}
		if hasHead {
			buf.Write(pktLineStr(headOID + " HEAD" + attrs + "\n"))
		} else if args.unborn {
			buf.Write(pktLineStr("unborn HEAD" + attrs + "\n"))
		}
	}
	for _, pair := range sortedRefs(repo) {
		ref, oid := pair[0], pair[1]
		if len(args.prefixes) > 0 {
			match := false
			for _, prefix := range args.prefixes {
				if strings.HasPrefix(ref, prefix) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		line := oid + " " + ref
		if args.peel {
			if object := forge.GetObject(oid); object != nil && object.Type == ObjTag {
				if peeled := forge.PeelToCommit(oid); peeled != "" {
					line += " peeled:" + peeled
				}
			}
		}
		buf.Write(pktLineStr(line + "\n"))
	}
	buf.Write(pktFlushBytes)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------------------------
// fetch planning
// ---------------------------------------------------------------------------------------------

type fetchArgs struct {
	wants          []string
	haves          []string
	shallow        map[string]bool
	depth          *int
	deepenRelative bool
	since          *int64
	notRefs        []string
	filter         string
	hasFilter      bool
	done           bool
}

type fetchPlan struct {
	pack        []byte
	shallow     []string
	unshallow   []string
	commonHaves []string
}

// Parse a partial-clone filter spec into a blob predicate kind:
// 0 = keep everything, 1 = keep none (blob:none), 2 = size limit, 3 = commits only (tree:0).
type blobFilterSpec struct {
	kind  int
	limit int
}

func blobFilter(spec string, has bool) blobFilterSpec {
	if !has {
		return blobFilterSpec{kind: 0}
	}
	if spec == "blob:none" {
		return blobFilterSpec{kind: 1}
	}
	if strings.HasPrefix(spec, "blob:limit=") {
		if n, err := strconv.Atoi(spec[len("blob:limit="):]); err == nil {
			return blobFilterSpec{kind: 2, limit: n}
		}
		return blobFilterSpec{kind: 0}
	}
	if spec == "tree:0" {
		return blobFilterSpec{kind: 3}
	}
	return blobFilterSpec{kind: 0}
}

func (bf blobFilterSpec) keep(size int) bool {
	switch bf.kind {
	case 1:
		return false
	case 2:
		return size <= bf.limit
	}
	return true
}

func planFetch(forge *Forge, repo *RepoRec, args *fetchArgs) *fetchPlan {
	var key strings.Builder
	writeList := func(items []string) {
		sorted := append([]string{}, items...)
		sort.Strings(sorted)
		for _, s := range sorted {
			key.WriteString(s)
			key.WriteByte(',')
		}
		key.WriteByte(';')
	}
	writeList(args.wants)
	writeList(args.haves)
	shallowList := make([]string, 0, len(args.shallow))
	for s := range args.shallow {
		shallowList = append(shallowList, s)
	}
	writeList(shallowList)
	if args.depth != nil {
		key.WriteString(strconv.Itoa(*args.depth))
	}
	key.WriteByte(';')
	if args.deepenRelative {
		key.WriteByte('1')
	} else {
		key.WriteByte('0')
	}
	key.WriteByte(';')
	if args.since != nil {
		key.WriteString(strconv.FormatInt(*args.since, 10))
	}
	key.WriteByte(';')
	for _, s := range args.notRefs {
		key.WriteString(s)
		key.WriteByte(',')
	}
	key.WriteByte(';')
	if args.hasFilter {
		key.WriteString(args.filter)
	}
	cacheKey := key.String()

	var commonHaves []string
	for _, oid := range args.haves {
		if object := forge.GetObject(oid); object != nil && object.Type == ObjCommit {
			commonHaves = append(commonHaves, oid)
		}
	}

	// ---- commit selection -----------------------------------------------------
	var wantCommits []string
	for _, want := range args.wants {
		if commit := forge.PeelToCommit(want); commit != "" {
			wantCommits = append(wantCommits, commit)
		}
	}
	hasDepthArgs := args.depth != nil || args.since != nil || len(args.notRefs) > 0
	included := map[string]bool{}
	switch {
	case args.depth != nil && args.deepenRelative:
		extra := *args.depth
		if extra < 0 {
			extra = 0
		}
		budget := map[string]int{}
		const inf = int(^uint(0) >> 1)
		var queue []string
		for _, commit := range wantCommits {
			if _, ok := budget[commit]; !ok {
				budget[commit] = inf
				queue = append(queue, commit)
			}
		}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			included[current] = true
			own := budget[current]
			effective := own
			if args.shallow[current] && extra < effective {
				effective = extra
			}
			if effective < 1 {
				continue
			}
			info := forge.CommitInfo(current)
			if info == nil {
				continue
			}
			for _, parent := range info.Parents {
				next := effective - 1
				if cur, ok := budget[parent]; !ok || cur < next {
					budget[parent] = next
					queue = append(queue, parent)
				}
			}
		}
	case args.depth != nil:
		limit := *args.depth
		if limit < 1 {
			limit = 1
		}
		dist := map[string]int{}
		var queue []string
		for _, commit := range wantCommits {
			if _, ok := dist[commit]; !ok {
				dist[commit] = 0
				queue = append(queue, commit)
			}
		}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			d := dist[current]
			if d <= limit-1 {
				included[current] = true
			}
			if d >= limit-1 {
				continue
			}
			info := forge.CommitInfo(current)
			if info == nil {
				continue
			}
			for _, parent := range info.Parents {
				if _, ok := dist[parent]; !ok {
					dist[parent] = d + 1
					queue = append(queue, parent)
				}
			}
		}
	case args.since != nil:
		since := *args.since
		stack := append([]string{}, wantCommits...)
		seen := map[string]bool{}
		for len(stack) > 0 {
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[current] {
				continue
			}
			seen[current] = true
			info := forge.CommitInfo(current)
			if info == nil {
				continue
			}
			if info.Time < since {
				continue
			}
			included[current] = true
			stack = append(stack, info.Parents...)
		}
	case len(args.notRefs) > 0:
		excluded := map[string]bool{}
		for _, ref := range args.notRefs {
			oid, ok := repo.Refs[ref]
			if !ok {
				if object := forge.GetObject(ref); object != nil && object.Type == ObjCommit {
					oid = ref
					ok = true
				}
			}
			if ok {
				for commit := range forge.AncestorsOf(oid) {
					excluded[commit] = true
				}
			}
		}
		for _, commit := range wantCommits {
			for ancestor := range forge.AncestorsOf(commit) {
				if !excluded[ancestor] {
					included[ancestor] = true
				}
			}
		}
	default:
		stack := append([]string{}, wantCommits...)
		seen := map[string]bool{}
		for len(stack) > 0 {
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[current] {
				continue
			}
			seen[current] = true
			included[current] = true
			if args.shallow[current] {
				continue // the client's boundary: do not expand past it
			}
			info := forge.CommitInfo(current)
			if info == nil {
				continue
			}
			stack = append(stack, info.Parents...)
		}
	}

	// ---- shallow bookkeeping ----------------------------------------------------
	var newShallow, unshallow []string
	if hasDepthArgs {
		for commit := range included {
			info := forge.CommitInfo(commit)
			if info == nil {
				continue
			}
			hasOutsideParent := false
			for _, parent := range info.Parents {
				if !included[parent] {
					hasOutsideParent = true
					break
				}
			}
			if hasOutsideParent {
				newShallow = append(newShallow, commit)
			} else if args.shallow[commit] {
				unshallow = append(unshallow, commit)
			}
		}
		for commit := range args.shallow {
			if !included[commit] {
				continue
			}
			already := false
			for _, s := range newShallow {
				if s == commit {
					already = true
				}
			}
			for _, s := range unshallow {
				if s == commit {
					already = true
				}
			}
			if already {
				continue
			}
			info := forge.CommitInfo(commit)
			if info == nil {
				continue
			}
			allIn := true
			for _, parent := range info.Parents {
				if !included[parent] {
					allIn = false
					break
				}
			}
			if allIn {
				unshallow = append(unshallow, commit)
			}
		}
	}

	// ---- exclusion (what the client already holds) ------------------------------
	exclude := map[string]bool{}
	{
		stack := append([]string{}, args.haves...)
		seenCommits := map[string]bool{}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if exclude[oid] {
				continue
			}
			object := forge.GetObject(oid)
			if object == nil {
				continue
			}
			exclude[oid] = true
			switch object.Type {
			case ObjCommit:
				if seenCommits[oid] {
					continue
				}
				seenCommits[oid] = true
				info := parseCommit(object.Data)
				stack = append(stack, info.Tree)
				if !args.shallow[oid] {
					stack = append(stack, info.Parents...)
				}
			case ObjTree:
				stack = append(stack, treeEntryOIDs(object.Data)...)
			case ObjTag:
				if target := forge.PeelStep(oid); target != "" {
					stack = append(stack, target)
				}
			}
		}
	}

	// ---- object collection --------------------------------------------------------
	keepBlob := blobFilter(args.filter, args.hasFilter)
	collected := map[string]PackObject{}
	var addTree func(treeOID string)
	addTree = func(treeOID string) {
		stack := []string{treeOID}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if _, ok := collected[oid]; ok || exclude[oid] {
				continue
			}
			object := forge.GetObject(oid)
			if object == nil {
				continue
			}
			if object.Type == ObjTree {
				collected[oid] = PackObject{OID: oid, Type: ObjTree, Data: object.Data}
				stack = append(stack, treeEntryOIDs(object.Data)...)
			} else if object.Type == ObjBlob {
				if !keepBlob.keep(len(object.Data)) {
					continue
				}
				collected[oid] = PackObject{OID: oid, Type: ObjBlob, Data: object.Data}
			}
		}
	}
	if keepBlob.kind != 3 {
		for commit := range included {
			if info := forge.CommitInfo(commit); info != nil {
				addTree(info.Tree)
			}
		}
	}
	for commit := range included {
		if exclude[commit] {
			continue
		}
		if object := forge.GetObject(commit); object != nil {
			collected[commit] = PackObject{OID: commit, Type: ObjCommit, Data: object.Data}
		}
	}
	for _, want := range args.wants {
		current := want
		for current != "" {
			object := forge.GetObject(current)
			if object == nil || object.Type != ObjTag {
				break
			}
			if _, ok := collected[current]; !ok && !exclude[current] {
				collected[current] = PackObject{OID: current, Type: ObjTag, Data: object.Data}
			}
			current = forge.PeelStep(current)
		}
	}

	forge.cacheMu.Lock()
	pack, cached := repo.PackCache[cacheKey]
	forge.cacheMu.Unlock()
	if !cached {
		objects := make([]PackObject, 0, len(collected))
		for _, object := range collected {
			objects = append(objects, object)
		}
		pack = writePack(objects)
		forge.cacheMu.Lock()
		repo.PackCache[cacheKey] = pack
		forge.cacheMu.Unlock()
	}
	sort.Strings(newShallow)
	sort.Strings(unshallow)
	return &fetchPlan{pack: pack, shallow: newShallow, unshallow: unshallow, commonHaves: commonHaves}
}

// Tree entry oids of one tree object.
func treeEntryOIDs(data []byte) []string {
	var oids []string
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			break
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			break
		}
		oids = append(oids, hex.EncodeToString(data[nul+1 : nul+21]))
		offset = nul + 21
	}
	return oids
}

// ---------------------------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------------------------

func sidebandChunks(pack []byte, progress bool) []byte {
	var buf bytes.Buffer
	if progress {
		msg := append([]byte{2}, []byte("Counting objects...\n")...)
		buf.Write(pktLine(msg))
	}
	const chunk = 65515
	for offset := 0; offset < len(pack); offset += chunk {
		end := offset + chunk
		if end > len(pack) {
			end = len(pack)
		}
		part := make([]byte, 0, end-offset+1)
		part = append(part, 1)
		part = append(part, pack[offset:end]...)
		buf.Write(pktLine(part))
	}
	if len(pack) == 0 {
		buf.Write(pktLine([]byte{1}))
	}
	return buf.Bytes()
}

func parseFetchLines(lines []string) *fetchArgs {
	args := &fetchArgs{shallow: map[string]bool{}}
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			rest := strings.TrimSpace(line[5:])
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				args.wants = append(args.wants, fields[0])
			}
		case strings.HasPrefix(line, "have "):
			args.haves = append(args.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			args.shallow[strings.TrimSpace(line[8:])] = true
		case strings.HasPrefix(line, "deepen-since "):
			if n, err := strconv.ParseInt(strings.TrimSpace(line[13:]), 10, 64); err == nil {
				args.since = &n
			}
		case strings.HasPrefix(line, "deepen-not "):
			args.notRefs = append(args.notRefs, strings.TrimSpace(line[11:]))
		case line == "deepen-relative":
			args.deepenRelative = true
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				args.depth = &n
			}
		case strings.HasPrefix(line, "filter "):
			args.filter = strings.TrimSpace(line[7:])
			args.hasFilter = true
		case line == "done":
			args.done = true
		}
	}
	return args
}

func dataLines(packets []pkt) []string {
	var lines []string
	for _, p := range packets {
		if p.kind == pktData {
			lines = append(lines, string(p.payload))
		}
	}
	return lines
}

func uploadPackV0(forge *Forge, repo *RepoRec, body []byte) ([]byte, error) {
	packets, err := decodePkts(body)
	if err != nil {
		return nil, err
	}
	lines := dataLines(packets)
	args := parseFetchLines(lines)
	firstWantCaps := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "want ") {
			fields := strings.Split(line, " ")
			if len(fields) > 2 {
				firstWantCaps = strings.Join(fields[2:], " ")
			}
			break
		}
	}
	plan := planFetch(forge, repo, args)
	var buf bytes.Buffer
	for _, oid := range plan.shallow {
		buf.Write(pktLineStr("shallow " + oid + "\n"))
	}
	for _, oid := range plan.unshallow {
		buf.Write(pktLineStr("unshallow " + oid + "\n"))
	}
	if len(args.haves) == 0 || len(plan.commonHaves) == 0 {
		buf.Write(pktLineStr("NAK\n"))
	} else if strings.Contains(firstWantCaps, "multi_ack_detailed") {
		for _, oid := range plan.commonHaves {
			buf.Write(pktLineStr("ACK " + oid + " common\n"))
		}
		buf.Write(pktLineStr("ACK " + plan.commonHaves[len(plan.commonHaves)-1] + " ready\n"))
	} else if strings.Contains(firstWantCaps, "multi_ack") {
		for _, oid := range plan.commonHaves {
			buf.Write(pktLineStr("ACK " + oid + "\n"))
		}
	} else {
		buf.Write(pktLineStr("ACK " + plan.commonHaves[0] + "\n"))
	}
	hasDone := false
	for _, line := range lines {
		if strings.TrimSuffix(line, "\n") == "done" {
			hasDone = true
		}
	}
	noProgress := hasDone && strings.Contains(firstWantCaps, "no-progress")
	buf.Write(sidebandChunks(plan.pack, !noProgress))
	buf.Write(pktFlushBytes)
	return buf.Bytes(), nil
}

func uploadPackV2(forge *Forge, repo *RepoRec, body []byte) ([]byte, error) {
	packets, err := decodePkts(body)
	if err != nil {
		return nil, err
	}
	var lines []string
	command := ""
	for _, p := range packets {
		if p.kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if strings.HasPrefix(line, "command=") {
			command = line[len("command="):]
			continue
		}
		lines = append(lines, line)
	}
	if command == "ls-refs" {
		lsArgs := lsRefsArgs{}
		for _, line := range lines {
			switch {
			case line == "peel":
				lsArgs.peel = true
			case line == "symrefs":
				lsArgs.symrefs = true
			case line == "unborn":
				lsArgs.unborn = true
			case strings.HasPrefix(line, "ref-prefix "):
				lsArgs.prefixes = append(lsArgs.prefixes, line[len("ref-prefix "):])
			}
		}
		return lsRefs(forge, repo, lsArgs), nil
	}
	args := parseFetchLines(lines)
	plan := planFetch(forge, repo, args)
	var buf bytes.Buffer
	if !args.done {
		buf.Write(pktLineStr("acknowledgments\n"))
		if len(plan.commonHaves) == 0 {
			buf.Write(pktLineStr("NAK\n"))
		} else {
			for _, oid := range plan.commonHaves {
				buf.Write(pktLineStr("ACK " + oid + "\n"))
			}
		}
		buf.Write(pktFlushBytes)
		return buf.Bytes(), nil
	}
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		buf.Write(pktLineStr("shallow-info\n"))
		for _, oid := range plan.shallow {
			buf.Write(pktLineStr("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			buf.Write(pktLineStr("unshallow " + oid + "\n"))
		}
		buf.Write(pktFlushBytes)
	}
	buf.Write(pktLineStr("packfile\n"))
	noProgress := false
	for _, line := range lines {
		if line == "no-progress" {
			noProgress = true
		}
	}
	buf.Write(sidebandChunks(plan.pack, !noProgress))
	buf.Write(pktFlushBytes)
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

var pushLineRe = regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$`)

func receivePack(forge *Forge, repo *RepoRec, body []byte) ([]byte, error) {
	packets, rest, err := splitAtFlush(body)
	if err != nil {
		return nil, err
	}
	var commands []pushCommand
	caps := ""
	for _, p := range packets {
		if p.kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			caps = line[nul+1:]
			line = line[:nul]
		}
		if m := pushLineRe.FindStringSubmatch(line); m != nil {
			commands = append(commands, pushCommand{oldOID: m[1], newOID: m[2], ref: m[3]})
		}
	}
	unpackOk := true
	unpackDetail := "ok"
	if len(rest) > 0 {
		read, err := readPack(rest, forge.LookupPack)
		if err != nil {
			unpackOk = false
			unpackDetail = strings.Join(strings.Fields(err.Error()), " ")
			if len(unpackDetail) > 80 {
				unpackDetail = unpackDetail[:80]
			}
		} else {
			for _, object := range read.objects {
				obj := object
				if _, ok := forge.Objects[obj.OID]; !ok {
					forge.Objects[obj.OID] = &StoredObject{Type: obj.Type, Data: obj.Data}
				}
			}
		}
	}
	type result struct {
		ref    string
		ok     bool
		reason string
	}
	var results []result
	if !unpackOk {
		for _, command := range commands {
			results = append(results, result{ref: command.ref, ok: false, reason: "unpack-error"})
		}
	} else {
		for _, command := range commands {
			current, ok := repo.Refs[command.ref]
			if !ok {
				current = ZeroOID
			}
			res := result{ref: command.ref, ok: true}
			switch {
			case command.oldOID != current:
				res.ok = false
				res.reason = "stale info"
			case command.newOID == ZeroOID:
				if command.ref == "refs/heads/"+repo.DefaultBranch {
					res.ok = false
					res.reason = "deletion of the current branch prohibited"
				}
			case forge.GetObject(command.newOID) == nil:
				res.ok = false
				res.reason = "missing object"
			case strings.HasPrefix(command.ref, "refs/heads/") && current != ZeroOID:
				newCommit := forge.PeelToCommit(command.newOID)
				curCommit := forge.PeelToCommit(current)
				if newCommit != "" && curCommit != "" && !forge.IsAncestor(curCommit, newCommit) {
					res.ok = false
					res.reason = "non-fast-forward"
				}
			}
			results = append(results, res)
			if res.ok {
				if command.newOID == ZeroOID {
					delete(repo.Refs, command.ref)
				} else {
					repo.Refs[command.ref] = command.newOID
				}
			}
		}
		forge.TouchRepo(repo)
	}
	var report bytes.Buffer
	if unpackOk {
		report.Write(pktLineStr("unpack ok\n"))
	} else {
		report.Write(pktLineStr("unpack " + unpackDetail + "\n"))
	}
	for _, res := range results {
		if res.ok {
			report.Write(pktLineStr("ok " + res.ref + "\n"))
		} else {
			report.Write(pktLineStr("ng " + res.ref + " " + res.reason + "\n"))
		}
	}
	report.Write(pktFlushBytes)
	reportPkt := report.Bytes()
	if strings.Contains(caps, "side-band-64k") {
		var buf bytes.Buffer
		const chunk = 65515
		for offset := 0; offset < len(reportPkt); offset += chunk {
			end := offset + chunk
			if end > len(reportPkt) {
				end = len(reportPkt)
			}
			part := make([]byte, 0, end-offset+1)
			part = append(part, 1)
			part = append(part, reportPkt[offset:end]...)
			buf.Write(pktLine(part))
		}
		buf.Write(pktFlushBytes)
		return buf.Bytes(), nil
	}
	return reportPkt, nil
}
