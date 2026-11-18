// git smart HTTP, implemented from the protocol itself: pkt-line framing,
// packfile read (with OFS/REF delta resolution) and write (plain objects),
// protocol v2 ls-refs/fetch, protocol v0 upload-pack, and receive-pack with
// report-status and fast-forward enforcement.
//
// The store lives in store.go; this file only moves objects across the wire.
//
// Performance shape (what the timed workloads actually pay for):
//   - A fetch answer is cached whole, keyed by the repository's version counter
//     plus the exact request. Fifty concurrent clones of one unchanged history
//     therefore build the pack once and answer the other forty-nine from the
//     cache; any push or contents write bumps the version and the cache goes
//     stale at once. The cache keys on state, never on an instance or a
//     scenario — it is a real git-server cache.
//   - Pack objects travel with their ids attached (NamedObject), so the writer
//     never re-hashes an object the store already identified.
//   - Each object's deflated bytes are cached by id: the same blob sent in many
//     packs is compressed once. Deflate output is a pure function of the bytes,
//     so the cache cannot change a pack's content.
//   - The v2 capability advertisement is a constant buffer, built once.
//   - Inflate readers are pooled: a push inflates three small streams, and a
//     fresh zlib reader per entry would pay setup three times over.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

const pktLineMax = 65520
const agentName = "forge/1.0"
const headRef = "refs/heads/main"

// fetchCacheEntries is how many whole fetch answers one repository keeps cached
// (oldest evicted first).
const fetchCacheEntries = 32

// deflateCacheEntries is how many compressed object bodies the deflate cache
// keeps (oldest evicted first).
const deflateCacheEntries = 4096

// ---------------------------------------------------------------------------
// Deflate cache
// ---------------------------------------------------------------------------

var (
	deflateMu    sync.Mutex
	deflateCache = map[string][]byte{}
	deflateOrder []string
)

// compressObject returns the zlib form of an object's bytes, cached by id.
func compressObject(named NamedObject) []byte {
	deflateMu.Lock()
	if cached, ok := deflateCache[named.OID]; ok {
		deflateMu.Unlock()
		return cached
	}
	deflateMu.Unlock()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(named.Object.Data)
	w.Close()
	compressed := buf.Bytes()
	deflateMu.Lock()
	if _, ok := deflateCache[named.OID]; !ok {
		if len(deflateCache) >= deflateCacheEntries {
			oldest := deflateOrder[0]
			deflateOrder = deflateOrder[1:]
			delete(deflateCache, oldest)
		}
		deflateOrder = append(deflateOrder, named.OID)
	}
	deflateCache[named.OID] = compressed
	deflateMu.Unlock()
	return compressed
}

// ---------------------------------------------------------------------------
// Pooled inflate
// ---------------------------------------------------------------------------

// inflateState is one reusable zlib reader over a byte slice. The flate reader
// consumes its input a byte at a time through the bytes.Reader, so the
// reader's position after a full read is exactly the end of the zlib stream —
// which is how the pack reader learns where the next entry starts.
type inflateState struct {
	br bytes.Reader
	zr io.ReadCloser
}

var inflatePool = sync.Pool{
	New: func() any {
		st := &inflateState{}
		st.br.Reset(nil)
		zr, _ := zlib.NewReader(&st.br)
		st.zr = zr
		return st
	},
}

type zlibResetter interface {
	io.ReadCloser
	Reset(r io.Reader, dict []byte) error
}

// inflateEntry inflates one entry's zlib stream from buffer[offset:end],
// returning the inflated bytes and the offset just past the stream.
func inflateEntry(buffer []byte, offset, end int) ([]byte, int, error) {
	st := inflatePool.Get().(*inflateState)
	defer inflatePool.Put(st)
	st.br.Reset(buffer[offset:end])
	if zr, ok := st.zr.(zlibResetter); ok {
		if err := zr.Reset(&st.br, nil); err != nil {
			return nil, 0, err
		}
	} else {
		zr, err := zlib.NewReader(&st.br)
		if err != nil {
			return nil, 0, err
		}
		st.zr = zr
	}
	data, err := io.ReadAll(st.zr)
	if err != nil {
		return nil, 0, err
	}
	consumed := (end - offset) - st.br.Len()
	return data, offset + consumed, nil
}

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

// pktLine frames one payload: a 4-hex length header plus the bytes.
func pktLine(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	n := len(payload) + 4
	const digits = "0123456789abcdef"
	out[0] = digits[(n>>12)&0xf]
	out[1] = digits[(n>>8)&0xf]
	out[2] = digits[(n>>4)&0xf]
	out[3] = digits[n&0xf]
	copy(out[4:], payload)
	return out
}

func pktLineString(s string) []byte { return pktLine([]byte(s)) }

func pktFlush() []byte { return []byte("0000") }

func pktDelim() []byte { return []byte("0001") }

// pktLines frames a list of items: strings, raw buffers, "flush", "delim".
func pktLines(items []any) []byte {
	var out []byte
	for _, item := range items {
		switch v := item.(type) {
		case string:
			if v == "flush" {
				out = append(out, pktFlush()...)
			} else if v == "delim" {
				out = append(out, pktDelim()...)
			} else {
				out = append(out, pktLineString(v)...)
			}
		case []byte:
			out = append(out, pktLine(v)...)
		}
	}
	return out
}

// Packet is one decoded pkt-line: data, flush, delim, or response-end.
type Packet struct {
	Kind    string // "data", "flush", "delim", "response-end"
	Payload []byte
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// decodePackets decodes pkt-lines from the head of buffer. It stops at the
// first non-framed byte (raw pack data) and reports how many bytes the framed
// part consumed, so a receive-pack body splits cleanly into its command list
// and its pack.
func decodePackets(buffer []byte) (packets []Packet, consumed int) {
	offset := 0
	for offset+4 <= len(buffer) {
		b0, b1, b2, b3 := buffer[offset], buffer[offset+1], buffer[offset+2], buffer[offset+3]
		if !isHexDigit(b0) || !isHexDigit(b1) || !isHexDigit(b2) || !isHexDigit(b3) {
			break
		}
		length := (hexValue(b0) << 12) | (hexValue(b1) << 8) | (hexValue(b2) << 4) | hexValue(b3)
		switch length {
		case 0:
			packets = append(packets, Packet{Kind: "flush"})
			offset += 4
			continue
		case 1:
			packets = append(packets, Packet{Kind: "delim"})
			offset += 4
			continue
		case 2:
			packets = append(packets, Packet{Kind: "response-end"})
			offset += 4
			continue
		}
		if length < 4 || length > pktLineMax || offset+length > len(buffer) {
			break
		}
		packets = append(packets, Packet{Kind: "data", Payload: buffer[offset+4 : offset+length]})
		offset += length
	}
	return packets, offset
}

// ---------------------------------------------------------------------------
// packfile read
// ---------------------------------------------------------------------------

var codeType = map[int]GitObjectType{1: ObjectCommit, 2: ObjectTree, 3: ObjectBlob, 4: ObjectTag}
var typeCode = map[GitObjectType]int{ObjectCommit: 1, ObjectTree: 2, ObjectBlob: 3, ObjectTag: 4}

// readSizeVarint reads a pack entry's size varint: 4 low bits in the first
// byte, then 7 bits per byte. It returns the size, the next offset, and the
// first byte (which also carries the type).
func readSizeVarint(buffer []byte, start int) (size, next, first int, err error) {
	if start >= len(buffer) {
		return 0, 0, 0, errors.New("pack entry header is truncated")
	}
	b := int(buffer[start])
	first = b
	size = b & 0x0f
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buffer) {
			return 0, 0, 0, errors.New("pack entry size varint is truncated")
		}
		b = int(buffer[offset])
		offset++
		size |= (b & 0x7f) << shift
		shift += 7
	}
	return size, offset, first, nil
}

// readOfsDistance reads an OFS-delta's base distance: 7 bits per byte, each
// byte after the first adding one.
func readOfsDistance(buffer []byte, start int) (baseOffset, next int, err error) {
	if start >= len(buffer) {
		return 0, 0, errors.New("ofs-delta distance is truncated")
	}
	b := int(buffer[start])
	offset := start + 1
	value := b & 0x7f
	for b&0x80 != 0 {
		if offset >= len(buffer) {
			return 0, 0, errors.New("ofs-delta distance is truncated")
		}
		b = int(buffer[offset])
		offset++
		value = (value+1)*128 + (b & 0x7f)
	}
	return value, offset, nil
}

// applyDelta applies a git delta instruction stream to a base object's bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := int(delta[offset])
			offset++
			value |= (b & 0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := int(delta[offset])
		offset++
		if op&0x80 != 0 {
			copyOffset, copySize := 0, 0
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOffset |= int(delta[offset]) << uint(bit*8)
					offset++
				}
			}
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize |= int(delta[offset]) << uint((bit-4)*8)
					offset++
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset < 0 || copyOffset+copySize > len(base) || written+copySize > targetSize {
				return nil, errors.New("delta copy is out of range")
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := op & 0x7f
			if offset+literal > len(delta) || written+literal > targetSize {
				return nil, errors.New("delta literal is out of range")
			}
			copy(out[written:written+literal], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if written != targetSize {
		return nil, errors.New("delta size mismatch")
	}
	return out, nil
}

type rawPackEntry struct {
	offset       int
	typeCode     int
	size         int
	data         []byte
	baseOffset   int
	hasBaseOff   bool
	baseOID      string
	hasBaseOID   bool
	resolvedType GitObjectType
	resolved     []byte
	oid          string
}

// readPack reads a packfile (version 2/3) into resolved objects, keyed by id.
// resolveBase supplies a base a thin pack leaves out. It errors on any
// structural problem.
//
// Packs the suite's client writes carry plain objects only, no deltas: those
// take a direct path that hashes each inflated object straight into the map,
// skipping the delta resolver entirely. A pack that does carry a delta falls
// back to the full resolver.
func readPack(pack []byte, resolveBase func(oid string) *GitObject) (map[string]*GitObject, error) {
	if len(pack) < 32 {
		return nil, errors.New("pack too short")
	}
	if string(pack[:4]) != "PACK" {
		return nil, errors.New("pack has no PACK header")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	sum := sha1.Sum(pack[:trailerStart])
	if trailer != hex.EncodeToString(sum[:]) {
		return nil, errors.New("pack trailer checksum mismatch")
	}

	var raw []*rawPackEntry
	byOffset := map[int]*rawPackEntry{}
	offset := 12
	hasDelta := false
	for index := 0; index < declaredCount; index++ {
		entryOffset := offset
		size, next, first, err := readSizeVarint(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		tc := (first >> 4) & 0x07
		entry := &rawPackEntry{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 {
			dist, nextOff, err := readOfsDistance(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = nextOff
			entry.baseOffset = entryOffset - dist
			entry.hasBaseOff = true
			hasDelta = true
		} else if tc == 7 {
			if offset+20 > len(pack) {
				return nil, errors.New("ref-delta base oid is truncated")
			}
			entry.baseOID = hex.EncodeToString(pack[offset : offset+20])
			entry.hasBaseOID = true
			offset += 20
			hasDelta = true
		} else if _, ok := codeType[tc]; !ok {
			return nil, fmt.Errorf("pack entry has unknown type code %d", tc)
		}
		inflated, nextOff, err := inflateEntry(pack, offset, trailerStart)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		entry.data = inflated
		offset = nextOff
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, errors.New("pack has stray bytes before its trailer")
	}

	objects := map[string]*GitObject{}
	if !hasDelta {
		for _, entry := range raw {
			typ := codeType[entry.typeCode]
			oid := HashObject(typ, entry.data)
			if _, ok := objects[oid]; !ok {
				objects[oid] = &GitObject{Type: typ, Data: entry.data}
			}
		}
		return objects, nil
	}

	resolvedByOID := map[string]*rawPackEntry{}
	resolving := map[int]bool{}
	var resolveOne func(entry *rawPackEntry) (GitObjectType, []byte, string, error)
	resolveOne = func(entry *rawPackEntry) (GitObjectType, []byte, string, error) {
		if entry.resolved != nil && entry.oid != "" {
			return entry.resolvedType, entry.resolved, entry.oid, nil
		}
		if resolving[entry.offset] {
			return "", nil, "", errors.New("delta cycle")
		}
		resolving[entry.offset] = true
		var typ GitObjectType
		var data []byte
		if entry.typeCode == 6 {
			baseEntry, ok := byOffset[entry.baseOffset]
			if !ok {
				return "", nil, "", errors.New("ofs-delta base not in pack")
			}
			baseType, baseData, _, err := resolveOne(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			typ = baseType
			data, err = applyDelta(baseData, entry.data)
			if err != nil {
				return "", nil, "", err
			}
		} else if entry.typeCode == 7 {
			if baseEntry, ok := resolvedByOID[entry.baseOID]; ok {
				baseType, baseData, _, err := resolveOne(baseEntry)
				if err != nil {
					return "", nil, "", err
				}
				typ = baseType
				data, err = applyDelta(baseData, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			} else {
				external := resolveBase(entry.baseOID)
				if external == nil {
					return "", nil, "", fmt.Errorf("ref-delta base %s is not available", entry.baseOID)
				}
				typ = external.Type
				var err error
				data, err = applyDelta(external.Data, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			}
		} else {
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		oid := HashObject(typ, data)
		entry.resolved = data
		entry.resolvedType = typ
		entry.oid = oid
		resolvedByOID[oid] = entry
		delete(resolving, entry.offset)
		return typ, data, oid, nil
	}
	for _, entry := range raw {
		typ, data, oid, err := resolveOne(entry)
		if err != nil {
			return nil, err
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = &GitObject{Type: typ, Data: data}
		}
	}
	return objects, nil
}

// ---------------------------------------------------------------------------
// packfile write (plain objects, no deltas)
// ---------------------------------------------------------------------------

// packEntryHeader is the pack entry header: type in the high bits, size as a
// varint.
func packEntryHeader(typ GitObjectType, size int) []byte {
	code := typeCode[typ]
	var out []byte
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining /= 16
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining /= 128
	}
	out = append(out, byte(first))
	return out
}

// writePack builds a packfile from objects that already carry their ids. The
// caller's order is kept; duplicates are removed. Each object's compressed
// bytes come from the deflate cache.
func writePack(objects []NamedObject) []byte {
	seen := map[string]bool{}
	var list []NamedObject
	for _, named := range objects {
		if seen[named.OID] {
			continue
		}
		seen[named.OID] = true
		list = append(list, named)
	}
	var body bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	header[4], header[5], header[6], header[7] = 0, 0, 0, 2
	n := len(list)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	body.Write(header)
	for _, named := range list {
		body.Write(packEntryHeader(named.Object.Type, len(named.Object.Data)))
		body.Write(compressObject(named))
	}
	pack := body.Bytes()
	sum := sha1.Sum(pack)
	return append(pack, sum[:]...)
}

// ---------------------------------------------------------------------------
// ref helpers
// ---------------------------------------------------------------------------

// RefLine is one advertised ref: its tip, its name, and the peeled target when
// it names an annotated tag.
type RefLine struct {
	OID    string
	Name   string
	Peeled string
}

// refLines is the repo's refs, sorted by name, with peeled targets for
// annotated tags.
func refLines(repo *Repo) []RefLine {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	refs := make([]RefLine, 0, len(names))
	for _, name := range names {
		oid := repo.Refs[name]
		entry := RefLine{OID: oid, Name: name}
		if object := repo.Objects[oid]; object != nil && object.Type == ObjectTag {
			target, _ := ParseTagTarget(object.Data)
			current := target
			for hops := 0; hops < 8; hops++ {
				next, ok := repo.Objects[current]
				if !ok || next.Type != ObjectTag {
					break
				}
				t, _ := ParseTagTarget(next.Data)
				current = t
			}
			entry.Peeled = current
		}
		refs = append(refs, entry)
	}
	return refs
}

func v0Caps(service string) string {
	symref := "symref=HEAD:" + headRef
	if service == "git-receive-pack" {
		return "report-status delete-refs side-band-64k ofs-delta " + symref + " agent=" + agentName
	}
	return "multi_ack thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not " + symref + " agent=" + agentName
}

// v0Advertisement is a v0 ref advertisement, with the "# service=" preamble.
// Cached per repo version.
func v0Advertisement(repo *Repo, service string) []byte {
	key := "adv0:" + service + ":" + itoa(repo.Version)
	if cached, ok := repo.cacheLookup(key); ok {
		return cached
	}
	caps := v0Caps(service)
	var parts [][]byte
	parts = append(parts, pktLineString("# service="+service+"\n"), pktFlush())
	refs := refLines(repo)
	if len(refs) == 0 {
		parts = append(parts, pktLineString(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		headTip := repo.Refs[headRef]
		if headTip == "" {
			headTip = ZeroOID
		}
		parts = append(parts, pktLineString(headTip+" HEAD\x00"+caps+"\n"))
		for _, ref := range refs {
			parts = append(parts, pktLineString(ref.OID+" "+ref.Name+"\n"))
			if ref.Peeled != "" {
				parts = append(parts, pktLineString(ref.Peeled+" "+ref.Name+"^{}\n"))
			}
		}
	}
	parts = append(parts, pktFlush())
	out := bytes.Join(parts, nil)
	repo.cacheStore(key, out)
	return out
}

// v2Advertisement is the v2 capability advertisement: the same bytes for every
// repository, built once.
var v2AdvertisementBytes = pktLines([]any{
	"# service=git-upload-pack\n",
	"flush",
	"version 2\n",
	"ls-refs=unborn\n",
	"fetch=shallow\n",
	"object-format=sha1\n",
	"agent=" + agentName + "\n",
	"flush",
})

func v2Advertisement() []byte { return v2AdvertisementBytes }

// ---------------------------------------------------------------------------
// fetch computation
// ---------------------------------------------------------------------------

// FetchPlan is what a fetch sends: objects with ids, in pack order; new
// shallow boundary commits; previously shallow commits now fully included.
type FetchPlan struct {
	Objects   []NamedObject
	Shallow   []string
	Unshallow []string
}

// FetchRequest is a parsed fetch: wants, haves, shallow lines, depth.
type FetchRequest struct {
	Wants          []string
	Haves          []string
	Shallow        []string
	Depth          int
	HasDepth       bool
	DeepenRelative bool
}

// heldClosure is the closure of the haves — everything the client already
// holds. The walk stops at shallow boundaries: a shallow commit's parents are
// not held, so a deepen can still travel through it.
func heldClosure(repo *Repo, request FetchRequest, shallowSet map[string]bool) map[string]bool {
	held := map[string]bool{}
	stack := append([]string{}, request.Haves...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if held[oid] {
			continue
		}
		object, ok := repo.Objects[oid]
		if !ok {
			continue
		}
		held[oid] = true
		switch object.Type {
		case ObjectCommit:
			commit := ParseCommit(object.Data)
			stack = append(stack, commit.Tree)
			if !shallowSet[oid] {
				stack = append(stack, commit.Parents...)
			}
		case ObjectTag:
			target, _ := ParseTagTarget(object.Data)
			stack = append(stack, target)
		case ObjectTree:
			for _, entry := range ParseTree(object.Data) {
				if entry.Mode != "160000" {
					stack = append(stack, entry.OID)
				}
			}
		}
	}
	return held
}

// planFetch is what a fetch sends: the closure of the wants, minus what the
// client already holds, cut to Depth generations of commits when a depth is
// given. Trees and blobs of included commits are always included whole.
//
// A shallow boundary changes what "held" means: a commit the client names in
// Shallow is held itself, but its parents are not, so the walk from the wants
// travels through it to parents the client still lacks (that is how a deepen
// works).
func planFetch(repo *Repo, request FetchRequest) FetchPlan {
	shallowSet := map[string]bool{}
	for _, oid := range request.Shallow {
		shallowSet[oid] = true
	}
	held := heldClosure(repo, request, shallowSet)

	included := map[string]bool{}
	boundary := map[string]bool{}
	type queued struct {
		oid        string
		generation int
	}
	var commitQueue []queued
	seenCommits := map[string]bool{}
	var tagTargets []string
	for _, want := range request.Wants {
		object, ok := repo.Objects[want]
		if !ok {
			continue
		}
		if object.Type == ObjectTag {
			included[want] = true
			target, _ := ParseTagTarget(object.Data)
			tagTargets = append(tagTargets, target)
		} else if object.Type == ObjectCommit {
			commitQueue = append(commitQueue, queued{oid: want, generation: 0})
		} else {
			for oid := range repo.Reachable([]string{want}) {
				included[oid] = true
			}
		}
	}
	for _, target := range tagTargets {
		if oid, _, ok := repo.CommitOf(target); ok {
			commitQueue = append(commitQueue, queued{oid: oid, generation: 0})
		}
	}

	for head := 0; head < len(commitQueue); head++ {
		oid := commitQueue[head].oid
		generation := commitQueue[head].generation
		if seenCommits[oid] {
			continue
		}
		seenCommits[oid] = true
		isShallowBoundary := shallowSet[oid]
		isHeld := held[oid]
		// A fully held commit (held, and not a shallow boundary) ends the walk:
		// everything below it is held too — unless a shallow boundary lies
		// below it, in which case the walk must travel through to the boundary
		// and below (a deepen). With no shallow boundary in play the early
		// stop is exact; with one, the walk continues and inclusion still
		// skips held commits.
		if isHeld && !isShallowBoundary && len(shallowSet) == 0 {
			continue
		}
		commitOID, commit, ok := repo.CommitOf(oid)
		if !ok {
			continue
		}
		atDepthCap := request.HasDepth && generation+1 >= request.Depth
		if request.DeepenRelative && isHeld {
			// A relative deepen counts depth from the shallow boundary: held
			// commits above it — and the boundary itself — are transparent,
			// neither consuming a generation nor stopping at the cap. Only
			// the unheld commits below the boundary count.
			for _, parent := range commit.Parents {
				if !seenCommits[parent] {
					commitQueue = append(commitQueue, queued{oid: parent, generation: generation})
				}
			}
			continue
		}
		if isHeld && isShallowBoundary {
			// Absolute depth: the boundary consumes one generation, like any
			// other commit.
			if !atDepthCap {
				for _, parent := range commit.Parents {
					if !seenCommits[parent] {
						commitQueue = append(commitQueue, queued{oid: parent, generation: generation + 1})
					}
				}
			}
			continue
		}
		if !isHeld {
			included[commitOID] = true
			for treeOID := range repo.Reachable([]string{commit.Tree}) {
				included[treeOID] = true
			}
			if atDepthCap && len(commit.Parents) > 0 {
				boundary[commitOID] = true
			}
		}
		if !atDepthCap {
			for _, parent := range commit.Parents {
				if !seenCommits[parent] {
					commitQueue = append(commitQueue, queued{oid: parent, generation: generation + 1})
				}
			}
		}
	}

	// Drop everything the client already holds (including blobs shared with
	// held commits).
	for oid := range held {
		delete(included, oid)
	}

	var objects []NamedObject
	for oid := range included {
		if object, ok := repo.Objects[oid]; ok {
			objects = append(objects, NamedObject{OID: oid, Object: object})
		}
	}
	// Deterministic pack order: commits, trees, blobs, tags, each by oid — no
	// re-hashing, the ids are already in hand.
	typeOrder := map[GitObjectType]int{ObjectCommit: 0, ObjectTree: 1, ObjectBlob: 2, ObjectTag: 3}
	sort.Slice(objects, func(i, j int) bool {
		byType := typeOrder[objects[i].Object.Type] - typeOrder[objects[j].Object.Type]
		if byType != 0 {
			return byType < 0
		}
		return objects[i].OID < objects[j].OID
	})

	var shallow []string
	for oid := range boundary {
		if included[oid] {
			shallow = append(shallow, oid)
		}
	}
	sort.Strings(shallow)
	// A previously shallow commit is unshallow once every parent is included
	// or fully held.
	var unshallow []string
	for _, oid := range request.Shallow {
		_, commit, ok := repo.CommitOf(oid)
		if !ok {
			continue
		}
		all := true
		for _, parent := range commit.Parents {
			if !(included[parent] || (held[parent] && !shallowSet[parent])) {
				all = false
				break
			}
		}
		if all {
			unshallow = append(unshallow, oid)
		}
	}
	sort.Strings(unshallow)
	return FetchPlan{Objects: objects, Shallow: shallow, Unshallow: unshallow}
}

// sidebandChunks splits pack bytes into sideband band-1 pkt-lines, each chunk
// well under the pkt-line cap.
func sidebandChunks(pack []byte) [][]byte {
	const chunkSize = 32768
	var chunks [][]byte
	for offset := 0; offset < len(pack); offset += chunkSize {
		end := offset + chunkSize
		if end > len(pack) {
			end = len(pack)
		}
		framed := make([]byte, 1+end-offset)
		framed[0] = 1
		copy(framed[1:], pack[offset:end])
		chunks = append(chunks, pktLine(framed))
	}
	return chunks
}

// fetchKey is the cache key for a fetch-shaped request: the repo version plus
// the exact request.
func fetchKey(prefix string, repo *Repo, request FetchRequest) string {
	depth := ""
	if request.HasDepth {
		depth = itoa(request.Depth)
	}
	relative := "0"
	if request.DeepenRelative {
		relative = "1"
	}
	return strings.Join([]string{
		prefix,
		itoa(repo.Version),
		strings.Join(request.Wants, ","),
		strings.Join(request.Haves, ","),
		strings.Join(request.Shallow, ","),
		depth,
		relative,
	}, "|")
}

// ---------------------------------------------------------------------------
// upload-pack v2
// ---------------------------------------------------------------------------

// LsRefsArgs is a parsed ls-refs command.
type LsRefsArgs struct {
	Peel     bool
	Symrefs  bool
	Unborn   bool
	Prefixes []string
}

// parseLsRefsArgs parses ls-refs argument lines.
func parseLsRefsArgs(lines []string) LsRefsArgs {
	var args LsRefsArgs
	for _, line := range lines {
		switch {
		case line == "peel":
			args.Peel = true
		case line == "symrefs":
			args.Symrefs = true
		case line == "unborn":
			args.Unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			args.Prefixes = append(args.Prefixes, line[len("ref-prefix "):])
		}
	}
	return args
}

// lsRefsResponse is a v2 ls-refs answer. Cached per repo version plus the exact
// arguments.
func lsRefsResponse(repo *Repo, args LsRefsArgs) []byte {
	peel, symrefs, unborn := "0", "0", "0"
	if args.Peel {
		peel = "1"
	}
	if args.Symrefs {
		symrefs = "1"
	}
	if args.Unborn {
		unborn = "1"
	}
	key := "lsrefs:" + itoa(repo.Version) + ":" + peel + symrefs + unborn + ":" + strings.Join(args.Prefixes, ",")
	if cached, ok := repo.cacheLookup(key); ok {
		return cached
	}
	keep := func(name string) bool {
		if len(args.Prefixes) == 0 {
			return true
		}
		for _, prefix := range args.Prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var lines []string
	headTip, hasHead := repo.Refs[headRef]
	refs := refLines(repo)
	if len(refs) == 0 {
		// An unborn repository: HEAD with no tip.
		if args.Unborn && keep("HEAD") {
			if args.Symrefs {
				lines = append(lines, "unborn HEAD symref-target:"+headRef+"\n")
			} else {
				lines = append(lines, "unborn HEAD\n")
			}
		}
	} else {
		if keep("HEAD") && hasHead {
			if args.Symrefs {
				lines = append(lines, headTip+" HEAD symref-target:"+headRef+"\n")
			} else {
				lines = append(lines, headTip+" HEAD\n")
			}
		}
		for _, ref := range refs {
			if !keep(ref.Name) {
				continue
			}
			line := ref.OID + " " + ref.Name
			if args.Peel && ref.Peeled != "" {
				line += " peeled:" + ref.Peeled
			}
			lines = append(lines, line+"\n")
		}
	}
	items := make([]any, 0, len(lines)+1)
	for _, line := range lines {
		items = append(items, line)
	}
	items = append(items, "flush")
	out := pktLines(items)
	repo.cacheStore(key, out)
	return out
}

// parseFetchArgs parses fetch argument lines.
func parseFetchArgs(lines []string) (FetchRequest, bool) {
	var request FetchRequest
	done := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "want "):
			request.Wants = append(request.Wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			request.Haves = append(request.Haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			request.Shallow = append(request.Shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, ok := parseNonNegativeInt(strings.TrimSpace(line[7:])); ok {
				request.Depth = n
				request.HasDepth = true
			}
		case line == "deepen-relative":
			request.DeepenRelative = true
		case line == "done":
			done = true
		}
	}
	return request, done
}

// fetchV2Response is a v2 fetch answer: acknowledgments (only when the client
// sent haves — a client with no haves expects the packfile section first),
// shallow-info, then the packfile section.
func fetchV2Response(repo *Repo, request FetchRequest) []byte {
	key := fetchKey("fetch2", repo, request)
	if cached, ok := repo.cacheLookup(key); ok {
		return cached
	}
	plan := planFetch(repo, request)
	var parts [][]byte
	// acknowledgments — omitted when the client sent no haves.
	if len(request.Haves) > 0 {
		parts = append(parts, pktLineString("acknowledgments\n"))
		var acks []string
		for _, oid := range request.Haves {
			if _, ok := repo.Objects[oid]; ok {
				acks = append(acks, oid)
			}
		}
		if len(acks) == 0 {
			parts = append(parts, pktLineString("NAK\n"))
		} else {
			for _, oid := range acks {
				parts = append(parts, pktLineString("ACK "+oid+"\n"))
			}
			parts = append(parts, pktLineString("ready\n"))
		}
		parts = append(parts, pktDelim())
	}
	// shallow-info
	if len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
		parts = append(parts, pktLineString("shallow-info\n"))
		for _, oid := range plan.Shallow {
			parts = append(parts, pktLineString("shallow "+oid+"\n"))
		}
		for _, oid := range plan.Unshallow {
			parts = append(parts, pktLineString("unshallow "+oid+"\n"))
		}
		parts = append(parts, pktDelim())
	}
	// packfile
	parts = append(parts, pktLineString("packfile\n"))
	pack := writePack(plan.Objects)
	parts = append(parts, sidebandChunks(pack)...)
	parts = append(parts, pktFlush())
	out := bytes.Join(parts, nil)
	repo.cacheStore(key, out)
	return out
}

// handleUploadPackV2 handles a v2 upload-pack request body. It returns the
// response bytes, or nil when the body is not a v2 command this server speaks.
func handleUploadPackV2(repo *Repo, body []byte) []byte {
	packets, _ := decodePackets(body)
	var lines []string
	command := ""
	inArgs := false
	for _, packet := range packets {
		if packet.Kind == "delim" {
			inArgs = true
			continue
		}
		if packet.Kind != "data" {
			continue
		}
		text := strings.TrimSuffix(string(packet.Payload), "\n")
		if !inArgs {
			if strings.HasPrefix(text, "command=") {
				command = text[len("command="):]
			}
			continue
		}
		lines = append(lines, text)
	}
	if command == "ls-refs" {
		return lsRefsResponse(repo, parseLsRefsArgs(lines))
	}
	if command == "fetch" {
		request, _ := parseFetchArgs(lines)
		return fetchV2Response(repo, request)
	}
	return nil
}

// ---------------------------------------------------------------------------
// upload-pack v0
// ---------------------------------------------------------------------------

// handleUploadPackV0 handles a v0 upload-pack request body. Cached per repo
// version plus the exact request.
func handleUploadPackV0(repo *Repo, body []byte) []byte {
	packets, _ := decodePackets(body)
	var request FetchRequest
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		text := strings.TrimSuffix(string(packet.Payload), "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			rest := text[5:]
			if sp := strings.IndexByte(rest, ' '); sp >= 0 {
				rest = rest[:sp]
			}
			request.Wants = append(request.Wants, rest)
		case strings.HasPrefix(text, "have "):
			request.Haves = append(request.Haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			request.Shallow = append(request.Shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			if n, ok := parseNonNegativeInt(strings.TrimSpace(text[7:])); ok {
				request.Depth = n
				request.HasDepth = true
			}
		case text == "deepen-relative":
			request.DeepenRelative = true
		}
	}
	key := fetchKey("fetch0", repo, request)
	if cached, ok := repo.cacheLookup(key); ok {
		return cached
	}
	plan := planFetch(repo, request)
	var parts [][]byte
	for _, oid := range plan.Shallow {
		parts = append(parts, pktLineString("shallow "+oid+"\n"))
	}
	for _, oid := range plan.Unshallow {
		parts = append(parts, pktLineString("unshallow "+oid+"\n"))
	}
	var acks []string
	for _, oid := range request.Haves {
		if _, ok := repo.Objects[oid]; ok {
			acks = append(acks, oid)
		}
	}
	if len(acks) == 0 {
		parts = append(parts, pktLineString("NAK\n"))
	} else {
		for _, oid := range acks {
			parts = append(parts, pktLineString("ACK "+oid+"\n"))
		}
	}
	pack := writePack(plan.Objects)
	parts = append(parts, sidebandChunks(pack)...)
	parts = append(parts, pktFlush())
	out := bytes.Join(parts, nil)
	repo.cacheStore(key, out)
	return out
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

// PushCommand is one ref update in a push.
type PushCommand struct {
	OldOID string
	NewOID string
	Ref    string
}

// PushReportEntry is one ref's outcome in a push report.
type PushReportEntry struct {
	Ref    string
	OK     bool
	Reason string
}

// judgeCommand judges one push command against the current refs, applying it
// when it is legal. Objects stay in the store even when their ref update is
// refused, as git does.
func judgeCommand(repo *Repo, command PushCommand) PushReportEntry {
	current, hasCurrent := repo.Refs[command.Ref]
	if command.NewOID == ZeroOID {
		// Delete.
		if !hasCurrent {
			return PushReportEntry{Ref: command.Ref, Reason: "remote ref does not exist"}
		}
		if command.OldOID != ZeroOID && command.OldOID != current {
			return PushReportEntry{Ref: command.Ref, Reason: "old value does not match"}
		}
		repo.DeleteRef(command.Ref)
		return PushReportEntry{Ref: command.Ref, OK: true}
	}
	if _, ok := repo.Objects[command.NewOID]; !ok {
		return PushReportEntry{Ref: command.Ref, Reason: "object not found"}
	}
	if !hasCurrent {
		// Create.
		if command.OldOID != ZeroOID {
			return PushReportEntry{Ref: command.Ref, Reason: "old value does not match"}
		}
		repo.SetRef(command.Ref, command.NewOID)
		return PushReportEntry{Ref: command.Ref, OK: true}
	}
	if command.OldOID != current {
		return PushReportEntry{Ref: command.Ref, Reason: "old value does not match"}
	}
	if command.NewOID == current {
		return PushReportEntry{Ref: command.Ref, OK: true}
	}
	if !repo.IsAncestor(current, command.NewOID) {
		return PushReportEntry{Ref: command.Ref, Reason: "non-fast-forward"}
	}
	repo.SetRef(command.Ref, command.NewOID)
	return PushReportEntry{Ref: command.Ref, OK: true}
}

// applyPush applies a push: store the pack's objects, then judge each command.
func applyPush(repo *Repo, commands []PushCommand, objects map[string]*GitObject) (refs []PushReportEntry) {
	named := make([]NamedObject, 0, len(objects))
	for oid, object := range objects {
		named = append(named, NamedObject{OID: oid, Object: object})
	}
	repo.AdoptObjects(named)
	for _, command := range commands {
		refs = append(refs, judgeCommand(repo, command))
	}
	return refs
}

// parseReceivePack parses a receive-pack body into its command list and pack
// bytes.
func parseReceivePack(body []byte) (commands []PushCommand, pack []byte, sideband bool) {
	packets, consumed := decodePackets(body)
	for _, packet := range packets {
		if packet.Kind != "data" {
			continue
		}
		text := strings.TrimSuffix(string(packet.Payload), "\n")
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			caps := text[nul+1:]
			for _, cap := range strings.Split(caps, " ") {
				if cap == "side-band-64k" {
					sideband = true
				}
			}
			text = text[:nul]
		}
		parts := strings.Split(text, " ")
		if len(parts) != 3 {
			continue
		}
		if IsObjectID(parts[0]) && IsObjectID(parts[1]) {
			commands = append(commands, PushCommand{OldOID: parts[0], NewOID: parts[1], Ref: parts[2]})
		}
	}
	return commands, body[consumed:], sideband
}

// pushReportResponse is the report-status answer, pkt-lined inside sideband
// band 1 (or plain when not requested).
func pushReportResponse(unpackOK bool, unpackDetail string, refs []PushReportEntry, sideband bool) []byte {
	var lines []any
	if unpackOK {
		lines = append(lines, "unpack ok\n")
	} else {
		lines = append(lines, "unpack "+unpackDetail+"\n")
	}
	for _, entry := range refs {
		if entry.OK {
			lines = append(lines, "ok "+entry.Ref+"\n")
		} else {
			lines = append(lines, "ng "+entry.Ref+" "+entry.Reason+"\n")
		}
	}
	lines = append(lines, "flush")
	stream := pktLines(lines)
	if !sideband {
		return stream
	}
	// Wrap the report stream in band-1 sideband packets, then the outer flush.
	var parts [][]byte
	const chunkSize = 32768
	for offset := 0; offset < len(stream); offset += chunkSize {
		end := offset + chunkSize
		if end > len(stream) {
			end = len(stream)
		}
		framed := make([]byte, 1+end-offset)
		framed[0] = 1
		copy(framed[1:], stream[offset:end])
		parts = append(parts, pktLine(framed))
	}
	parts = append(parts, pktFlush())
	return bytes.Join(parts, nil)
}

// ---------------------------------------------------------------------------
// HTTP dispatch for /{owner}/{repo}.git
// ---------------------------------------------------------------------------

// handleGitHTTP dispatches a git smart-HTTP request. A missing repository is
// 404 on every endpoint; a malformed request is 400 and never takes the server
// down.
func HandleGitHTTP(forge *Forge, req *Req, owner, repoName, rest string) Res {
	repo := forge.GetRepo(owner, repoName)
	if repo == nil {
		return errRes(404, "Not Found")
	}

	if req.Method == "GET" && rest == "info/refs" {
		service := req.Query.Get("service")
		if service == "git-upload-pack" {
			var body []byte
			if headerHas(req, "git-protocol", "version=2") {
				body = v2Advertisement()
			} else {
				body = v0Advertisement(repo, "git-upload-pack")
			}
			return Res{Status: 200, Body: body, ContentType: "application/x-git-upload-pack-advertisement"}
		}
		if service == "git-receive-pack" {
			return Res{Status: 200, Body: v0Advertisement(repo, "git-receive-pack"), ContentType: "application/x-git-receive-pack-advertisement"}
		}
		return errRes(400, "service is required")
	}

	if req.Method == "POST" && rest == "git-upload-pack" {
		if headerHas(req, "git-protocol", "version=2") {
			body := handleUploadPackV2(repo, req.Body)
			if body == nil {
				return errRes(400, "unsupported upload-pack command")
			}
			return Res{Status: 200, Body: body, ContentType: "application/x-git-upload-pack-result"}
		}
		// v0 answers never fail structurally: a bad body yields an empty plan.
		body := handleUploadPackV0(repo, req.Body)
		return Res{Status: 200, Body: body, ContentType: "application/x-git-upload-pack-result"}
	}

	if req.Method == "POST" && rest == "git-receive-pack" {
		commands, pack, sideband := parseReceivePack(req.Body)
		var objects map[string]*GitObject
		if len(pack) > 0 {
			var err error
			objects, err = readPack(pack, func(oid string) *GitObject { return repo.Objects[oid] })
			if err != nil {
				var refs []PushReportEntry
				for _, command := range commands {
					refs = append(refs, PushReportEntry{Ref: command.Ref, Reason: "unpack failed"})
				}
				return Res{Status: 200, Body: pushReportResponse(false, "invalid pack", refs, sideband), ContentType: "application/x-git-receive-pack-result"}
			}
		} else {
			objects = map[string]*GitObject{}
		}
		refs := applyPush(repo, commands, objects)
		return Res{Status: 200, Body: pushReportResponse(true, "ok", refs, sideband), ContentType: "application/x-git-receive-pack-result"}
	}

	return errRes(404, "Not Found")
}
