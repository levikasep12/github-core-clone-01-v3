// The forge's in-memory store.
//
// One process, no disk, no clock: git objects and refs per repository, plus the
// issues, pull requests, labels and comments that REST, GraphQL and MCP all read
// and write. Every timestamp the forge emits is fixed, so a replayed trial
// answers byte-identical bytes.
//
// Each repository carries a version counter that rises on every object or ref
// mutation. The git layer caches whole fetch answers keyed by that version, so
// fifty concurrent clones of one unchanged history are answered from the cache,
// and any push, contents write or merge invalidates it at once. The counter is
// process-local and never leaves the server.

package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
)

// ZeroOID is the all-zero object id: no object, no tip.
const ZeroOID = "0000000000000000000000000000000000000000"

// DefaultBranch is the branch new repositories start on.
const DefaultBranch = "main"

// ForgeEpochSec is the fixed timestamp every server-made commit carries
// (2023-11-14T22:13:20Z).
const ForgeEpochSec = 1700000000

// FixedISO is the fixed ISO timestamp every REST/GraphQL created_at, updated_at
// and merged_at carries.
const FixedISO = "2025-01-01T00:00:00.000Z"

// ForgeIdentity is the identity every server-made commit and tag carries.
const ForgeIdentity = "forge <forge@localhost>"

// GitObjectType names the four git object kinds.
type GitObjectType string

const (
	ObjectCommit GitObjectType = "commit"
	ObjectTree   GitObjectType = "tree"
	ObjectBlob   GitObjectType = "blob"
	ObjectTag    GitObjectType = "tag"
)

// GitObject is one stored object: its type and raw bytes.
type GitObject struct {
	Type GitObjectType
	Data []byte
}

// NamedObject is an object together with its id, so pack writers never re-hash
// what the store already knows.
type NamedObject struct {
	OID    string
	Object *GitObject
}

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// CommitView is a parsed commit object.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// IsObjectID reports whether value is a 40-hex object id.
func IsObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		if !isHexByte(value[i]) {
			return false
		}
	}
	return true
}

// HashObject is git's object id: SHA-1 over "<type> <size>\0" plus the bytes.
func HashObject(typ GitObjectType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(string(typ) + " " + itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ParseTree splits a tree object into its entries, in stored order.
func ParseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := indexByte(data, ' ', offset)
		if space < 0 {
			break
		}
		nul := indexByte(data, 0, space+1)
		if nul < 0 || nul+21 > len(data) {
			break
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries
}

func indexByte(data []byte, c byte, from int) int {
	for i := from; i < len(data); i++ {
		if data[i] == c {
			return i
		}
	}
	return -1
}

// treeLess is git's tree ordering: a directory sorts as though its name ended
// in '/'.
func treeLess(a, b TreeEntry) bool {
	left := a.Name
	if a.Mode == "40000" {
		left += "/"
	}
	right := b.Name
	if b.Mode == "40000" {
		right += "/"
	}
	return left < right
}

// BuildTree renders a tree object's bytes, entries sorted the way git sorts them.
func BuildTree(entries []TreeEntry) []byte {
	ordered := make([]TreeEntry, len(entries))
	copy(ordered, entries)
	sort.Slice(ordered, func(i, j int) bool { return treeLess(ordered[i], ordered[j]) })
	var out []byte
	for _, e := range ordered {
		out = append(out, []byte(e.Mode+" "+e.Name+"\x00")...)
		raw, _ := hex.DecodeString(e.OID)
		out = append(out, raw...)
	}
	return out
}

// ParseCommit parses a commit object: headers above the blank line, message
// below it.
func ParseCommit(data []byte) CommitView {
	blank := indexSub(data, []byte("\n\n"))
	headerEnd := len(data)
	message := ""
	if blank >= 0 {
		headerEnd = blank
		message = string(data[blank+2:])
	}
	var view CommitView
	view.Message = message
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		key, value := line, ""
		if sp := strings.IndexByte(line, ' '); sp >= 0 {
			key, value = line[:sp], line[sp+1:]
		}
		switch key {
		case "tree":
			view.Tree = value
		case "parent":
			view.Parents = append(view.Parents, value)
		case "author":
			view.Author = value
		case "committer":
			view.Committer = value
		}
	}
	return view
}

func indexSub(data, sub []byte) int {
	if len(sub) == 0 || len(data) < len(sub) {
		return -1
	}
	for i := 0; i+len(sub) <= len(data); i++ {
		match := true
		for j := range sub {
			if data[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// BuildCommitBytes renders a commit object's bytes with the forge's fixed
// identity and timestamp.
func BuildCommitBytes(tree string, parents []string, message string) []byte {
	who := ForgeIdentity + " " + itoa(ForgeEpochSec) + " +0000"
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + who + "\n")
	b.WriteString("committer " + who + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// ParseTagTarget parses an annotated tag's target object id and type.
func ParseTagTarget(data []byte) (object, typ string) {
	blank := indexSub(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if strings.HasPrefix(line, "object ") {
			object = line[7:]
		} else if strings.HasPrefix(line, "type ") {
			typ = line[5:]
		}
	}
	return object, typ
}

// ---------------------------------------------------------------------------
// Issues, pulls, labels, comments
// ---------------------------------------------------------------------------

// Label is an issue label.
type Label struct {
	ID    int
	Name  string
	Color string
}

// IssueComment is one comment on an issue.
type IssueComment struct {
	ID        int
	Body      string
	UserLogin string
}

// Issue is one issue. Title, Body, State, Labels and Comments mutate; the rest
// is fixed at creation. Seq is the creation sequence, for created-order sorts
// (timestamps are fixed).
type Issue struct {
	ID       int
	Number   int
	Title    string
	Body     string
	State    string // "open" or "closed"
	Labels   []*Label
	Comments []*IssueComment
	Seq      int
}

// PullRequest is one pull request.
type PullRequest struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" or "closed"
	Merged         bool
	MergeCommitSha *string
	MergedAt       *string
	HeadRef        string
	BaseRef        string
	Seq            int
}

// ---------------------------------------------------------------------------
// Global (Relay) ids: opaque base64url of a typed payload, stable across reads.
// ---------------------------------------------------------------------------

func encodeB64url(text string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(text))
}

func decodeB64url(text string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		// The original decoder is lenient about padding and alphabet; try the
		// padded standard form as a second chance.
		raw, err = base64.URLEncoding.DecodeString(text)
		if err != nil {
			return "", false
		}
	}
	return string(raw), true
}

// RepoGlobalID is the Relay id for a repository.
func RepoGlobalID(repo *Repo) string {
	return encodeB64url("Repository:" + itoa(repo.ID))
}

// IssueGlobalID is the Relay id for an issue.
func IssueGlobalID(repo *Repo, issue *Issue) string {
	return encodeB64url("Issue:" + itoa(repo.ID) + ":" + itoa(issue.Number))
}

// PullGlobalID is the Relay id for a pull request.
func PullGlobalID(repo *Repo, pull *PullRequest) string {
	return encodeB64url("PullRequest:" + itoa(repo.ID) + ":" + itoa(pull.Number))
}

// CommentGlobalID is the Relay id for an issue comment.
func CommentGlobalID(repo *Repo, comment *IssueComment) string {
	return encodeB64url("IssueComment:" + itoa(repo.ID) + ":" + itoa(comment.ID))
}

// DecodedGlobalID is a parsed Relay id.
type DecodedGlobalID struct {
	Kind      string // "Repository", "Issue", "PullRequest", "IssueComment"
	RepoID    int
	Number    int // issue or pull number
	CommentID int
}

// DecodeGlobalID parses a Relay id, or reports it unparseable.
func DecodeGlobalID(id string) (DecodedGlobalID, bool) {
	var out DecodedGlobalID
	text, ok := decodeB64url(id)
	if !ok {
		return out, false
	}
	parts := strings.Split(text, ":")
	if len(parts) < 2 {
		return out, false
	}
	first, ok := parseNonNegativeInt(parts[1])
	if !ok {
		return out, false
	}
	switch parts[0] {
	case "Repository":
		if len(parts) != 2 {
			return out, false
		}
		return DecodedGlobalID{Kind: "Repository", RepoID: first}, true
	case "Issue", "PullRequest":
		if len(parts) != 3 {
			return out, false
		}
		num, ok := parseNonNegativeInt(parts[2])
		if !ok {
			return out, false
		}
		return DecodedGlobalID{Kind: parts[0], RepoID: first, Number: num}, true
	case "IssueComment":
		if len(parts) != 3 {
			return out, false
		}
		num, ok := parseNonNegativeInt(parts[2])
		if !ok {
			return out, false
		}
		return DecodedGlobalID{Kind: "IssueComment", RepoID: first, CommentID: num}, true
	}
	return out, false
}

func parseNonNegativeInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// CountLines counts the lines of a text blob; a trailing newline adds no line.
func CountLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return len(lines)
}

// LineDiff is a simple multiset line diff: [added, removed] between two texts.
func LineDiff(oldData, newData []byte) (additions, deletions int) {
	counts := func(data []byte) map[string]int {
		m := map[string]int{}
		if len(data) == 0 {
			return m
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		for _, line := range lines {
			m[line]++
		}
		return m
	}
	before := counts(oldData)
	after := counts(newData)
	for line, count := range after {
		if d := count - before[line]; d > 0 {
			additions += d
		}
	}
	for line, count := range before {
		if d := count - after[line]; d > 0 {
			deletions += d
		}
	}
	return additions, deletions
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

// Repo is one repository: git objects and refs, issues, pulls, labels. All
// fields are guarded by the Forge's lock; the fetch cache has its own mutex
// because reads populate it.
type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	PrivateFlag   bool
	DefaultBranch string
	Objects       map[string]*GitObject
	Refs          map[string]string
	Labels        map[string]*Label
	Issues        map[int]*Issue
	Pulls         map[int]*PullRequest
	// NextNumber is the shared issue/PR numbering, GitHub-style.
	NextNumber   int
	orderCounter int
	// Version rises on every object or ref change; the git layer's caches key
	// on it.
	Version int

	cacheMu    sync.Mutex
	cache      map[string][]byte
	cacheOrder []string
}

// NewRepo builds an empty repository.
func NewRepo(id int, owner, name, description string, privateFlag bool) *Repo {
	return &Repo{
		ID:            id,
		Owner:         owner,
		Name:          name,
		Description:   description,
		PrivateFlag:   privateFlag,
		DefaultBranch: DefaultBranch,
		Objects:       map[string]*GitObject{},
		Refs:          map[string]string{},
		Labels:        map[string]*Label{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*PullRequest{},
		NextNumber:    1,
		cache:         map[string][]byte{},
	}
}

// FullName is "owner/name".
func (r *Repo) FullName() string { return r.Owner + "/" + r.Name }

// NextSeq is the next creation-sequence number (issues and pulls share one
// counter).
func (r *Repo) NextSeq() int {
	r.orderCounter++
	return r.orderCounter
}

// Bump records a git-state change; cached fetch answers for this repo go stale.
func (r *Repo) Bump() { r.Version++ }

// GetObject fetches one object by id.
func (r *Repo) GetObject(oid string) *GitObject { return r.Objects[oid] }

// PutObject stores one object, bumping the version only when it is new.
func (r *Repo) PutObject(typ GitObjectType, data []byte) string {
	oid := HashObject(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &GitObject{Type: typ, Data: data}
		r.Bump()
	}
	return oid
}

// AdoptObjects stores objects that already carry their ids (a received pack);
// one bump for the whole batch.
func (r *Repo) AdoptObjects(objects []NamedObject) {
	changed := false
	for _, named := range objects {
		if _, ok := r.Objects[named.OID]; !ok {
			r.Objects[named.OID] = named.Object
			changed = true
		}
	}
	if changed {
		r.Bump()
	}
}

// RefTip is the tip of a full ref name, or "".
func (r *Repo) RefTip(ref string) (string, bool) {
	oid, ok := r.Refs[ref]
	return oid, ok
}

// BranchTip is the tip of a branch name, or "".
func (r *Repo) BranchTip(branch string) (string, bool) {
	oid, ok := r.Refs["refs/heads/"+branch]
	return oid, ok
}

// DefaultTip is the tip of the default branch, or "".
func (r *Repo) DefaultTip() (string, bool) {
	oid, ok := r.Refs["refs/heads/"+r.DefaultBranch]
	return oid, ok
}

// SetRef moves a ref, bumping the version only when the ref actually changes.
func (r *Repo) SetRef(ref, oid string) {
	if r.Refs[ref] != oid {
		r.Refs[ref] = oid
		r.Bump()
	}
}

// DeleteRef deletes a ref, bumping the version only when the ref existed.
func (r *Repo) DeleteRef(ref string) bool {
	if _, ok := r.Refs[ref]; !ok {
		return false
	}
	delete(r.Refs, ref)
	r.Bump()
	return true
}

// ResolveRef resolves a ref-ish: a branch name, a full ref, or a raw oid. An
// empty ref-ish means the default branch.
func (r *Repo) ResolveRef(refish string) (string, bool) {
	if refish == "" {
		return r.DefaultTip()
	}
	if IsObjectID(refish) {
		if _, ok := r.Objects[refish]; ok {
			return refish, true
		}
	}
	if strings.HasPrefix(refish, "refs/") {
		oid, ok := r.Refs[refish]
		return oid, ok
	}
	if oid, ok := r.Refs["refs/heads/"+refish]; ok {
		return oid, true
	}
	oid, ok := r.Refs["refs/tags/"+refish]
	return oid, ok
}

// CreateCommit creates a commit object in the store and returns its id.
func (r *Repo) CreateCommit(tree string, parents []string, message string) string {
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	return r.PutObject(ObjectCommit, BuildCommitBytes(tree, parents, message))
}

// CommitOf is the commit view of an oid, unwrapping an annotated tag to its
// target commit.
func (r *Repo) CommitOf(oid string) (string, CommitView, bool) {
	current := oid
	for hops := 0; hops < 8; hops++ {
		object, ok := r.Objects[current]
		if !ok {
			return "", CommitView{}, false
		}
		if object.Type == ObjectCommit {
			return current, ParseCommit(object.Data), true
		}
		if object.Type == ObjectTag {
			target, _ := ParseTagTarget(object.Data)
			current = target
			continue
		}
		return "", CommitView{}, false
	}
	return "", CommitView{}, false
}

// Reachable is every object reachable from roots (gitlinks skipped).
func (r *Repo) Reachable(roots []string) map[string]bool {
	seen := map[string]bool{}
	var stack []string
	for _, root := range roots {
		if IsObjectID(root) {
			stack = append(stack, root)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		object, ok := r.Objects[oid]
		if !ok {
			continue
		}
		seen[oid] = true
		switch object.Type {
		case ObjectCommit:
			commit := ParseCommit(object.Data)
			stack = append(stack, commit.Tree)
			stack = append(stack, commit.Parents...)
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
	return seen
}

// IsAncestor reports whether ancestor is reachable from tip following commit
// parents (tags unwrapped).
func (r *Repo) IsAncestor(ancestor, tip string) bool {
	if ancestor == tip {
		return true
	}
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		_, commit, ok := r.CommitOf(oid)
		if !ok {
			continue
		}
		stack = append(stack, commit.Parents...)
	}
	return false
}

// MergeBase is the first common ancestor in BFS order from a.
func (r *Repo) MergeBase(a, b string) (string, bool) {
	ancestorsOfB := map[string]bool{}
	stackB := []string{b}
	for len(stackB) > 0 {
		oid := stackB[len(stackB)-1]
		stackB = stackB[:len(stackB)-1]
		if ancestorsOfB[oid] {
			continue
		}
		ancestorsOfB[oid] = true
		_, commit, ok := r.CommitOf(oid)
		if !ok {
			continue
		}
		stackB = append(stackB, commit.Parents...)
	}
	seen := map[string]bool{}
	queue := []string{a}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		if ancestorsOfB[oid] {
			return oid, true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		_, commit, ok := r.CommitOf(oid)
		if !ok {
			continue
		}
		queue = append(queue, commit.Parents...)
	}
	return "", false
}

// CommitHistory is the commits reachable from tip, newest first (BFS).
func (r *Repo) CommitHistory(tip string) []CommitRecord {
	var out []CommitRecord
	if tip == "" {
		return out
	}
	seen := map[string]bool{}
	queue := []string{tip}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		realOID, commit, ok := r.CommitOf(oid)
		if !ok {
			continue
		}
		out = append(out, CommitRecord{OID: realOID, View: commit})
		queue = append(queue, commit.Parents...)
	}
	return out
}

// CommitRecord pairs a commit id with its parsed view.
type CommitRecord struct {
	OID  string
	View CommitView
}

// FileEntry is one flattened tree entry: a file's object id and mode.
type FileEntry struct {
	OID  string
	Mode string
}

// FlattenTree flattens a tree into path -> file entry, files only.
func (r *Repo) FlattenTree(treeOID string) map[string]FileEntry {
	out := map[string]FileEntry{}
	r.flattenInto(treeOID, "", out)
	return out
}

func (r *Repo) flattenInto(treeOID, prefix string, out map[string]FileEntry) {
	if treeOID == "" {
		return
	}
	object, ok := r.Objects[treeOID]
	if !ok || object.Type != ObjectTree {
		return
	}
	for _, entry := range ParseTree(object.Data) {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			r.flattenInto(entry.OID, path, out)
		} else {
			out[path] = FileEntry{OID: entry.OID, Mode: entry.Mode}
		}
	}
}

// TreeAt is the tree object id at path under treeOID ("" is the root).
func (r *Repo) TreeAt(treeOID, path string) (string, bool) {
	if treeOID == "" {
		return "", false
	}
	if path == "" {
		return treeOID, true
	}
	current := treeOID
	for _, segment := range strings.Split(path, "/") {
		object, ok := r.Objects[current]
		if !ok || object.Type != ObjectTree {
			return "", false
		}
		found := ""
		for _, entry := range ParseTree(object.Data) {
			if entry.Name == segment && entry.Mode == "40000" {
				found = entry.OID
				break
			}
		}
		if found == "" {
			return "", false
		}
		current = found
	}
	return current, true
}

// EntryAt is the blob entry at path under treeOID (also reports directories).
func (r *Repo) EntryAt(treeOID, path string) (TreeEntry, bool) {
	if treeOID == "" {
		return TreeEntry{}, false
	}
	segments := splitNonEmpty(path, "/")
	current := treeOID
	for i, segment := range segments {
		object, ok := r.Objects[current]
		if !ok || object.Type != ObjectTree {
			return TreeEntry{}, false
		}
		var entry TreeEntry
		found := false
		for _, candidate := range ParseTree(object.Data) {
			if candidate.Name == segment {
				entry, found = candidate, true
				break
			}
		}
		if !found {
			return TreeEntry{}, false
		}
		if i == len(segments)-1 {
			return entry, true
		}
		if entry.Mode != "40000" {
			return TreeEntry{}, false
		}
		current = entry.OID
	}
	return TreeEntry{}, false
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// UpdateTree returns a new tree with path set to blobOID (or removed when
// blobOID is ""), creating intermediate trees and pruning directories left
// empty. It returns the new root tree oid.
func (r *Repo) UpdateTree(treeOID, path, blobOID string) string {
	segments := splitNonEmpty(path, "/")
	if len(segments) == 0 {
		// No path to change: the tree stands as it is.
		if treeOID != "" {
			return treeOID
		}
		return r.PutObject(ObjectTree, BuildTree(nil))
	}
	var rebuild func(current string, depth int) (string, bool)
	rebuild = func(current string, depth int) (string, bool) {
		entries := map[string]TreeEntry{}
		if current != "" {
			if object, ok := r.Objects[current]; ok && object.Type == ObjectTree {
				for _, entry := range ParseTree(object.Data) {
					entries[entry.Name] = entry
				}
			}
		}
		name := segments[depth]
		if depth == len(segments)-1 {
			if blobOID == "" {
				delete(entries, name)
			} else {
				entries[name] = TreeEntry{Mode: "100644", Name: name, OID: blobOID}
			}
		} else {
			child, ok := entries[name]
			childOID := ""
			if ok && child.Mode == "40000" {
				childOID = child.OID
			}
			next, ok := rebuild(childOID, depth+1)
			if !ok {
				delete(entries, name)
			} else {
				entries[name] = TreeEntry{Mode: "40000", Name: name, OID: next}
			}
		}
		if len(entries) == 0 {
			return "", false
		}
		list := make([]TreeEntry, 0, len(entries))
		for _, entry := range entries {
			list = append(list, entry)
		}
		return r.PutObject(ObjectTree, BuildTree(list)), true
	}
	if root, ok := rebuild(treeOID, 0); ok {
		return root
	}
	return r.PutObject(ObjectTree, BuildTree(nil))
}

// cacheLookup reads a cached whole answer.
func (r *Repo) cacheLookup(key string) ([]byte, bool) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	v, ok := r.cache[key]
	return v, ok
}

// cacheStore writes a cached whole answer, evicting the oldest first.
func (r *Repo) cacheStore(key string, value []byte) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if _, ok := r.cache[key]; !ok {
		if len(r.cache) >= fetchCacheEntries {
			oldest := r.cacheOrder[0]
			r.cacheOrder = r.cacheOrder[1:]
			delete(r.cache, oldest)
		}
		r.cacheOrder = append(r.cacheOrder, key)
	}
	r.cache[key] = value
}

// ---------------------------------------------------------------------------
// The forge
// ---------------------------------------------------------------------------

// Forge is the whole store: the viewer, their repositories, and the counters.
// Mu guards everything; readers take RLock, writers take Lock.
type Forge struct {
	Mu         sync.RWMutex
	Viewer     ScenarioUser
	repos      map[string]*Repo
	repoOrder  []*Repo
	nextRepoID int
	globalSeq  int
	commentSeq int
}

// NewForge builds an empty forge.
func NewForge() *Forge {
	return &Forge{
		Viewer:     viewerUser(),
		repos:      map[string]*Repo{},
		nextRepoID: 1,
		globalSeq:  1,
		commentSeq: 1,
	}
}

// CreateRepo creates a repository owned by the viewer.
func (f *Forge) CreateRepo(name, description string, privateFlag bool) *Repo {
	repo := NewRepo(f.nextRepoID, f.Viewer.Login, name, description, privateFlag)
	f.nextRepoID++
	f.repos[repo.Owner+"/"+repo.Name] = repo
	f.repoOrder = append(f.repoOrder, repo)
	return repo
}

// GetRepo fetches a repository by owner and name; another owner's name is a
// miss, never an error.
func (f *Forge) GetRepo(owner, name string) *Repo {
	if owner != f.Viewer.Login {
		return nil
	}
	return f.repos[owner+"/"+name]
}

// DeleteRepo removes a repository.
func (f *Forge) DeleteRepo(owner, name string) bool {
	if owner != f.Viewer.Login {
		return false
	}
	key := owner + "/" + name
	if _, ok := f.repos[key]; !ok {
		return false
	}
	delete(f.repos, key)
	for i, repo := range f.repoOrder {
		if repo.Owner == owner && repo.Name == name {
			f.repoOrder = append(f.repoOrder[:i], f.repoOrder[i+1:]...)
			break
		}
	}
	return true
}

// ViewerRepos is the viewer's repositories, in creation order.
func (f *Forge) ViewerRepos() []*Repo {
	out := make([]*Repo, 0, len(f.repoOrder))
	for _, repo := range f.repoOrder {
		if repo.Owner == f.Viewer.Login {
			out = append(out, repo)
		}
	}
	return out
}

// NextGlobalID is the next global id (issues, pulls, labels share one counter).
func (f *Forge) NextGlobalID() int {
	id := f.globalSeq
	f.globalSeq++
	return id
}

// NextComment is the next comment id.
func (f *Forge) NextComment() int {
	id := f.commentSeq
	f.commentSeq++
	return id
}
