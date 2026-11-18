// gitcore.go — git object primitives: hashing, trees, commits, packs and pkt-lines.
// A from-scratch Go port of the forge's byte-level git machinery: pure byte work
// over []byte; the object store itself lives in forge.go.

package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type ObjType string

const (
	ObjBlob   ObjType = "blob"
	ObjTree   ObjType = "tree"
	ObjCommit ObjType = "commit"
	ObjTag    ObjType = "tag"
)

const ZeroOID = "0000000000000000000000000000000000000000"

var typeCodes = map[ObjType]int{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}
var codeTypes = map[int]ObjType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag, 6: ObjTag, 7: ObjBlob}

func hashObject(t ObjType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", t, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type CommitInfo struct {
	Tree    string
	Parents []string
	Time    int64
	Message string
}

var committerTimeRe = regexp.MustCompile(` (\d+) [+-]\d{4}$`)

// Parse a commit object's headers; tolerant of extra headers and a missing blank line.
func parseCommit(data []byte) CommitInfo {
	split := bytes.Index(data, []byte("\n\n"))
	var head, message []byte
	if split < 0 {
		head = data
	} else {
		head = data[:split]
		message = data[split+2:]
	}
	info := CommitInfo{Message: string(message)}
	for _, line := range strings.Split(string(head), "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			info.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			info.Parents = append(info.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "committer "):
			if m := committerTimeRe.FindStringSubmatch(line); m != nil {
				info.Time, _ = strconv.ParseInt(m[1], 10, 64)
			}
		}
	}
	return info
}

type TagInfo struct {
	Object string
	Type   string
	Tag    string
}

func parseTag(data []byte) TagInfo {
	var info TagInfo
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			info.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			info.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			info.Tag = strings.TrimSpace(line[4:])
		case line == "":
			return info
		}
	}
	return info
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			break
		}
		space += offset
		mode := string(data[offset:space])
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			break
		}
		entries = append(entries, TreeEntry{Mode: mode, Name: string(data[space+1 : nul]), OID: hex.EncodeToString(data[nul+1 : nul+21])})
		offset = nul + 21
	}
	return entries
}

// Serialise tree entries with git's ordering: names bytewise, directories compared as name + '/'.
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		ka, kb := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			ka += "/"
		}
		if sorted[j].Mode == "40000" {
			kb += "/"
		}
		return ka < kb
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------------------------
// packs
// ---------------------------------------------------------------------------------------------

type PackObject struct {
	OID  string
	Type ObjType
	Data []byte
}

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, 6)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// Write a plain (undeltified) version-2 pack over `objects`, in the order given.
func writePack(objects []PackObject) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(objects)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	buf.Write(header)
	for _, object := range objects {
		size := len(object.Data)
		first := byte(typeCodes[object.Type]<<4) | byte(size&0x0f)
		size >>= 4
		if size > 0 {
			buf.WriteByte(first | 0x80)
			for size > 0 {
				b := byte(size & 0x7f)
				size >>= 7
				if size > 0 {
					b |= 0x80
				}
				buf.WriteByte(b)
			}
		} else {
			buf.WriteByte(first)
		}
		buf.Write(deflate(object.Data))
	}
	body := buf.Bytes()
	trailer := sha1.Sum(body)
	return append(body, trailer[:]...)
}

// Inflate one zlib stream starting at `offset`; answers the data and how many input bytes it consumed.
func inflateAt(buffer []byte, offset int) ([]byte, int, error) {
	src := bytes.NewReader(buffer[offset:])
	br := bufio.NewReader(src)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	if err := zr.Close(); err != nil {
		return nil, 0, err
	}
	consumed := (len(buffer) - offset) - br.Buffered() - src.Len()
	return data, consumed, nil
}

// Apply a git delta (`base` -> result) following the copy/insert opcode stream.
func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readVarint := func() int {
		value := 0
		shift := uint(0)
		for {
			b := delta[offset]
			offset++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	sourceSize := readVarint()
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta base size %d != %d", sourceSize, len(base))
	}
	targetSize := readVarint()
	var buf bytes.Buffer
	produced := 0
	for offset < len(delta) {
		opcode := delta[offset]
		offset++
		if opcode&0x80 != 0 {
			from := 0
			length := 0
			if opcode&0x01 != 0 {
				from = int(delta[offset])
				offset++
			}
			if opcode&0x02 != 0 {
				from |= int(delta[offset]) << 8
				offset++
			}
			if opcode&0x04 != 0 {
				from |= int(delta[offset]) << 16
				offset++
			}
			if opcode&0x08 != 0 {
				from |= int(delta[offset]) << 24
				offset++
			}
			if opcode&0x10 != 0 {
				length = int(delta[offset])
				offset++
			}
			if opcode&0x20 != 0 {
				length |= int(delta[offset]) << 8
				offset++
			}
			if opcode&0x40 != 0 {
				length |= int(delta[offset]) << 16
				offset++
			}
			if length == 0 {
				length = 0x10000
			}
			buf.Write(base[from : from+length])
			produced += length
		} else if opcode > 0 {
			buf.Write(delta[offset : offset+int(opcode)])
			produced += int(opcode)
			offset += int(opcode)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	out := buf.Bytes()
	if len(out) != targetSize || produced != targetSize {
		return nil, errors.New("delta size mismatch")
	}
	return out, nil
}

type rawEntry struct {
	typ             ObjType
	data            []byte
	deltaBaseOID    string
	hasBaseOID      bool
	deltaBaseOffset int
	hasBaseOffset   bool
	delta           []byte
	offset          int
}

type readPackResult struct {
	objects []PackObject
	count   int
}

// Read a pack into fully resolved objects. `lookup` resolves thin-pack bases the pack itself lacks.
func readPack(pack []byte, lookup func(oid string) *PackObject) (*readPackResult, error) {
	if len(pack) < 12+20 {
		return nil, errors.New("pack too short")
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("bad pack signature")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	digest := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(digest[:], pack[len(pack)-20:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	end := len(pack) - 20
	offset := 12
	var raw []*rawEntry
	byOffset := map[int]*PackObject{}
	var resolved []*PackObject

	resolvePlain := func(e *rawEntry) *PackObject {
		return &PackObject{OID: hashObject(e.typ, e.data), Type: e.typ, Data: e.data}
	}
	for index := 0; index < count; index++ {
		entryOffset := offset
		b := pack[offset]
		offset++
		code := int(b>>4) & 0x07
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b = pack[offset]
			offset++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		entry := &rawEntry{offset: entryOffset}
		if code == 6 {
			bb := pack[offset]
			offset++
			back := int(bb & 0x7f)
			for bb&0x80 != 0 {
				bb = pack[offset]
				offset++
				back = ((back + 1) << 7) | int(bb&0x7f)
			}
			entry.deltaBaseOffset = entryOffset - back
			entry.hasBaseOffset = true
		} else if code == 7 {
			entry.deltaBaseOID = hex.EncodeToString(pack[offset : offset+20])
			entry.hasBaseOID = true
			offset += 20
		}
		data, read, err := inflateAt(pack, offset)
		if err != nil {
			return nil, err
		}
		offset += read
		if offset > end {
			return nil, errors.New("pack entry overruns the trailer")
		}
		plain, ok := codeTypes[code]
		if !ok {
			return nil, fmt.Errorf("unknown pack type code %d", code)
		}
		if code == 6 || code == 7 {
			entry.typ = ObjBlob
			entry.delta = data
		} else {
			if len(data) != size {
				return nil, errors.New("pack entry size mismatch")
			}
			entry.typ = plain
			entry.data = data
		}
		raw = append(raw, entry)
	}
	if offset != end {
		return nil, errors.New("pack has trailing garbage")
	}
	pending := map[*rawEntry]bool{}
	for _, e := range raw {
		pending[e] = true
	}
	for _, e := range raw {
		if e.data != nil && e.delta == nil {
			obj := resolvePlain(e)
			byOffset[e.offset] = obj
			resolved = append(resolved, obj)
			delete(pending, e)
		}
	}
	guard := 0
	for len(pending) > 0 {
		guard++
		if guard > count+2 {
			return nil, errors.New("unresolvable delta chain")
		}
		progressed := false
		for e := range pending {
			var base *PackObject
			if e.hasBaseOffset {
				base = byOffset[e.deltaBaseOffset]
			} else if e.hasBaseOID {
				for _, o := range resolved {
					if o.OID == e.deltaBaseOID {
						base = o
						break
					}
				}
				if base == nil {
					base = lookup(e.deltaBaseOID)
				}
			}
			if base == nil {
				continue
			}
			data, err := applyDelta(base.Data, e.delta)
			if err != nil {
				return nil, err
			}
			obj := &PackObject{OID: hashObject(base.Type, data), Type: base.Type, Data: data}
			byOffset[e.offset] = obj
			resolved = append(resolved, obj)
			delete(pending, e)
			progressed = true
		}
		if !progressed {
			return nil, errors.New("unresolvable delta chain")
		}
	}
	objects := make([]PackObject, 0, len(resolved))
	for _, o := range resolved {
		objects = append(objects, *o)
	}
	return &readPackResult{objects: objects, count: count}, nil
}

// ---------------------------------------------------------------------------------------------
// pkt-lines
// ---------------------------------------------------------------------------------------------

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktEnd
)

type pkt struct {
	kind    pktKind
	payload []byte
}

// Frame one payload as a pkt-line (payload capped at 65516 bytes by the format).
func pktLine(payload []byte) []byte {
	head := fmt.Sprintf("%04x", len(payload)+4)
	return append([]byte(head), payload...)
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlushBytes = []byte("0000")
var pktDelimBytes = []byte("0001")

var pktLenRe = regexp.MustCompile(`^[0-9a-fA-F]{4}$`)

func parsePktLen(buffer []byte, offset int) (int, error) {
	hexStr := string(buffer[offset : offset+4])
	if !pktLenRe.MatchString(hexStr) {
		return 0, fmt.Errorf("bad pkt-line length %q", hexStr)
	}
	n, _ := strconv.ParseInt(hexStr, 16, 32)
	return int(n), nil
}

// Decode a whole buffer of pkt-lines; errors on malformed framing.
func decodePkts(buffer []byte) ([]pkt, error) {
	var packets []pkt
	offset := 0
	for offset < len(buffer) {
		if offset+4 > len(buffer) {
			return nil, errors.New("truncated pkt-line header")
		}
		length, err := parsePktLen(buffer, offset)
		if err != nil {
			return nil, err
		}
		switch {
		case length == 0:
			packets = append(packets, pkt{kind: pktFlush})
			offset += 4
		case length == 1:
			packets = append(packets, pkt{kind: pktDelim})
			offset += 4
		case length == 2:
			packets = append(packets, pkt{kind: pktEnd})
			offset += 4
		default:
			if length < 4 || offset+length > len(buffer) {
				return nil, errors.New("pkt-line overruns the buffer")
			}
			packets = append(packets, pkt{kind: pktData, payload: buffer[offset+4 : offset+length]})
			offset += length
		}
	}
	return packets, nil
}

// Split a buffer at the first flush packet: the pkt-lines before it, and the raw remainder after.
func splitAtFlush(buffer []byte) ([]pkt, []byte, error) {
	var packets []pkt
	offset := 0
	for offset+4 <= len(buffer) {
		length, err := parsePktLen(buffer, offset)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case length == 0:
			return packets, buffer[offset+4:], nil
		case length == 1:
			packets = append(packets, pkt{kind: pktDelim})
			offset += 4
		case length == 2:
			packets = append(packets, pkt{kind: pktEnd})
			offset += 4
		default:
			if length < 4 || offset+length > len(buffer) {
				return nil, nil, errors.New("pkt-line overruns the buffer")
			}
			packets = append(packets, pkt{kind: pktData, payload: buffer[offset+4 : offset+length]})
			offset += length
		}
	}
	return nil, nil, errors.New("no flush packet found")
}
