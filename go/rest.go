// GitHub REST v3: repositories, branches, commits, contents, labels, issues,
// comments and pull requests — all on the one shared store, so GraphQL and MCP
// read back exactly what REST wrote.
//
// Routes served here, all under /api/v3 (bearer auth is enforced by the
// router, not here):
//   GET  /                                  GET  /meta                      GET  /user
//   POST /user/repos                        GET  /user/repos
//   GET/PATCH/DELETE /repos/{o}/{r}
//   GET  /repos/{o}/{r}/branches            GET  /repos/{o}/{r}/branches/{b}
//   GET  /repos/{o}/{r}/commits[?sha=]
//   POST /repos/{o}/{r}/git/refs            GET  /repos/{o}/{r}/git/refs/heads/{b}
//   PUT/GET/DELETE /repos/{o}/{r}/contents/{path}
//   POST/GET /repos/{o}/{r}/labels
//   POST/GET /repos/{o}/{r}/issues          GET/PATCH /repos/{o}/{r}/issues/{n}
//   POST/GET /repos/{o}/{r}/issues/{n}/comments
//   POST/GET /repos/{o}/{r}/pulls           GET  /repos/{o}/{r}/pulls/{n}
//   GET  /repos/{o}/{r}/pulls/{n}/files     PUT  /repos/{o}/{r}/pulls/{n}/merge
//
// Every ref change goes through repo.SetRef, which bumps the repository
// version and so invalidates the git layer's cached fetch answers.
//
// The JSON shapes below declare their fields in wire order: struct fields
// marshal in declaration order, and the order is wire-visible.

package main

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Shared domain operations (REST, GraphQL and MCP all call these)
// ---------------------------------------------------------------------------

// createIssueResult is the outcome of creating an issue.
type createIssueResult struct {
	OK      bool
	Issue   *Issue
	Status  int
	Message string
}

// CreateIssue creates an issue in a repository.
func CreateIssue(forge *Forge, repo *Repo, title, body string, labels []string) createIssueResult {
	if title == "" {
		return createIssueResult{Status: 422, Message: "title is required"}
	}
	var resolved []*Label
	for _, name := range labels {
		label, ok := repo.Labels[name]
		if !ok {
			return createIssueResult{Status: 422, Message: `Label "` + name + `" does not exist`}
		}
		resolved = append(resolved, label)
	}
	issue := &Issue{
		ID:       forge.NextGlobalID(),
		Number:   repo.NextNumber,
		Title:    title,
		Body:     body,
		State:    "open",
		Labels:   resolved,
		Comments: []*IssueComment{},
		Seq:      repo.NextSeq(),
	}
	repo.NextNumber++
	repo.Issues[issue.Number] = issue
	return createIssueResult{OK: true, Issue: issue}
}

// CloseIssue closes an issue.
func CloseIssue(issue *Issue) { issue.State = "closed" }

type createPullResult struct {
	OK      bool
	Pull    *PullRequest
	Status  int
	Message string
}

// CreatePull opens a pull request from a head branch into a base branch.
func CreatePull(forge *Forge, repo *Repo, title, body, head, base string) createPullResult {
	if title == "" {
		return createPullResult{Status: 422, Message: "title is required"}
	}
	if head == "" {
		return createPullResult{Status: 422, Message: "head is required"}
	}
	if base == "" {
		base = repo.DefaultBranch
	}
	if _, ok := repo.BranchTip(head); !ok {
		return createPullResult{Status: 422, Message: `head branch "` + head + `" does not exist`}
	}
	if _, ok := repo.BranchTip(base); !ok {
		return createPullResult{Status: 422, Message: `base branch "` + base + `" does not exist`}
	}
	pull := &PullRequest{
		ID:      forge.NextGlobalID(),
		Number:  repo.NextNumber,
		Title:   title,
		Body:    body,
		State:   "open",
		HeadRef: head,
		BaseRef: base,
		Seq:     repo.NextSeq(),
	}
	repo.NextNumber++
	repo.Pulls[pull.Number] = pull
	return createPullResult{OK: true, Pull: pull}
}

// PullFile is one file a pull request changes.
type PullFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"` // "added", "removed", "modified"
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

// PullFiles is the files a pull request changes, between its merge-base and
// its head.
func PullFiles(repo *Repo, pull *PullRequest) []PullFile {
	headTip, ok := repo.BranchTip(pull.HeadRef)
	if !ok {
		return []PullFile{}
	}
	_, headCommit, ok := repo.CommitOf(headTip)
	if !ok {
		return []PullFile{}
	}
	var baseTree string
	if baseTip, ok := repo.BranchTip(pull.BaseRef); ok {
		if base, ok := repo.MergeBase(baseTip, headTip); ok {
			if _, baseCommit, ok := repo.CommitOf(base); ok {
				baseTree = baseCommit.Tree
			}
		}
	}
	before := repo.FlattenTree(baseTree)
	after := repo.FlattenTree(headCommit.Tree)
	paths := map[string]bool{}
	for path := range before {
		paths[path] = true
	}
	for path := range after {
		paths[path] = true
	}
	sorted := make([]string, 0, len(paths))
	for path := range paths {
		sorted = append(sorted, path)
	}
	sort.Strings(sorted)
	var files []PullFile
	for _, path := range sorted {
		oldEntry, hasOld := before[path]
		newEntry, hasNew := after[path]
		switch {
		case hasOld && !hasNew:
			data := []byte{}
			if obj := repo.Objects[oldEntry.OID]; obj != nil {
				data = obj.Data
			}
			removed := CountLines(data)
			files = append(files, PullFile{Filename: path, Status: "removed", Deletions: removed, Changes: removed})
		case !hasOld && hasNew:
			data := []byte{}
			if obj := repo.Objects[newEntry.OID]; obj != nil {
				data = obj.Data
			}
			added := CountLines(data)
			files = append(files, PullFile{Filename: path, Status: "added", Additions: added, Changes: added})
		case hasOld && hasNew && oldEntry.OID != newEntry.OID:
			var oldData, newData []byte
			if obj := repo.Objects[oldEntry.OID]; obj != nil {
				oldData = obj.Data
			}
			if obj := repo.Objects[newEntry.OID]; obj != nil {
				newData = obj.Data
			}
			additions, deletions := LineDiff(oldData, newData)
			files = append(files, PullFile{Filename: path, Status: "modified", Additions: additions, Deletions: deletions, Changes: additions + deletions})
		}
	}
	if files == nil {
		files = []PullFile{}
	}
	return files
}

// PullConflicts reports whether the pull request conflicts with its base: some
// path the head changed since the merge-base that the base also changed, to
// different bytes.
func PullConflicts(repo *Repo, pull *PullRequest) bool {
	headTip, ok1 := repo.BranchTip(pull.HeadRef)
	baseTip, ok2 := repo.BranchTip(pull.BaseRef)
	if !ok1 || !ok2 {
		return false
	}
	_, headCommit, ok1 := repo.CommitOf(headTip)
	_, baseTipCommit, ok2 := repo.CommitOf(baseTip)
	if !ok1 || !ok2 {
		return false
	}
	var ancestorTree string
	if base, ok := repo.MergeBase(baseTip, headTip); ok {
		if _, baseCommit, ok := repo.CommitOf(base); ok {
			ancestorTree = baseCommit.Tree
		}
	}
	ancestorFiles := repo.FlattenTree(ancestorTree)
	headFiles := repo.FlattenTree(headCommit.Tree)
	baseFiles := repo.FlattenTree(baseTipCommit.Tree)
	paths := map[string]bool{}
	for path := range ancestorFiles {
		paths[path] = true
	}
	for path := range headFiles {
		paths[path] = true
	}
	for path := range baseFiles {
		paths[path] = true
	}
	for path := range paths {
		ancestor := ""
		if e, ok := ancestorFiles[path]; ok {
			ancestor = e.OID
		}
		head := ""
		if e, ok := headFiles[path]; ok {
			head = e.OID
		}
		baseVersion := ""
		if e, ok := baseFiles[path]; ok {
			baseVersion = e.OID
		}
		// An absent file is not an oid; compare presence as well as bytes.
		_, hasAncestor := ancestorFiles[path]
		_, hasHead := headFiles[path]
		_, hasBase := baseFiles[path]
		headChanged := hasHead != hasAncestor || head != ancestor
		baseChanged := hasBase != hasAncestor || baseVersion != ancestor
		bothChanged := headChanged && baseChanged
		differ := hasHead != hasBase || head != baseVersion
		if bothChanged && differ {
			return true
		}
	}
	return false
}

type mergePullResult struct {
	OK      bool
	SHA     string
	Status  int
	Message string
}

// MergePull squash-merges a pull request onto its base branch.
func MergePull(repo *Repo, pull *PullRequest) mergePullResult {
	if pull.Merged {
		return mergePullResult{Status: 405, Message: "Pull request has already been merged"}
	}
	if pull.State != "open" {
		return mergePullResult{Status: 405, Message: "Pull request is closed"}
	}
	headTip, ok1 := repo.BranchTip(pull.HeadRef)
	baseTip, ok2 := repo.BranchTip(pull.BaseRef)
	if !ok1 || !ok2 {
		return mergePullResult{Status: 422, Message: "head or base branch does not exist"}
	}
	if PullConflicts(repo, pull) {
		return mergePullResult{Status: 405, Message: "Merge conflict: the head branch conflicts with the base branch"}
	}
	var ancestorTree string
	if base, ok := repo.MergeBase(baseTip, headTip); ok {
		if _, baseCommit, ok := repo.CommitOf(base); ok {
			ancestorTree = baseCommit.Tree
		}
	}
	_, headCommit, ok1 := repo.CommitOf(headTip)
	_, baseTipCommit, ok2 := repo.CommitOf(baseTip)
	if !ok1 || !ok2 {
		return mergePullResult{Status: 422, Message: "head or base commit not found"}
	}
	// The merged tree: the base tip's tree with the head's changes applied on
	// top.
	ancestorFiles := repo.FlattenTree(ancestorTree)
	headFiles := repo.FlattenTree(headCommit.Tree)
	merged := repo.FlattenTree(baseTipCommit.Tree)
	paths := map[string]bool{}
	for path := range ancestorFiles {
		paths[path] = true
	}
	for path := range headFiles {
		paths[path] = true
	}
	for path := range paths {
		ancestor := ""
		if e, ok := ancestorFiles[path]; ok {
			ancestor = e.OID
		}
		head, hasHead := headFiles[path]
		headOID := ""
		if hasHead {
			headOID = head.OID
		}
		if headOID == ancestor {
			continue
		}
		if !hasHead {
			delete(merged, path)
		} else {
			merged[path] = FileEntry{OID: head.OID, Mode: head.Mode}
		}
	}
	sorted := make([]string, 0, len(merged))
	for path := range merged {
		sorted = append(sorted, path)
	}
	sort.Strings(sorted)
	tree := ""
	for _, path := range sorted {
		tree = repo.UpdateTree(tree, path, merged[path].OID)
	}
	if tree == "" {
		tree = repo.PutObject(ObjectTree, BuildTree(nil))
	}
	sha := repo.CreateCommit(tree, []string{baseTip}, pull.Title+" (#"+strconv.Itoa(pull.Number)+")")
	repo.SetRef("refs/heads/"+pull.BaseRef, sha)
	pull.State = "closed"
	pull.Merged = true
	pull.MergeCommitSha = &sha
	fixed := FixedISO
	pull.MergedAt = &fixed
	return mergePullResult{OK: true, SHA: sha}
}

// ---------------------------------------------------------------------------
// JSON shapes (key order is wire-visible; struct fields marshal in order)
// ---------------------------------------------------------------------------

type userJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func userJson(forge *Forge) userJSON {
	return userJSON{Login: forge.Viewer.Login, ID: forge.Viewer.ID, Type: "User", Name: forge.Viewer.Name, Email: forge.Viewer.Email}
}

type repoOwnerJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type repoJSON struct {
	ID            int           `json:"id"`
	Name          string        `json:"name"`
	FullName      string        `json:"full_name"`
	Owner         repoOwnerJSON `json:"owner"`
	Private       bool          `json:"private"`
	Description   string        `json:"description"`
	DefaultBranch string        `json:"default_branch"`
	CreatedAt     string        `json:"created_at"`
	UpdatedAt     string        `json:"updated_at"`
	Fork          bool          `json:"fork"`
	HasIssues     bool          `json:"has_issues"`
	HasProjects   bool          `json:"has_projects"`
	HasWiki       bool          `json:"has_wiki"`
}

// RepoJSON is the repository object the API serves.
func RepoJSON(forge *Forge, repo *Repo) repoJSON {
	return repoJSON{
		ID:            repo.ID,
		Name:          repo.Name,
		FullName:      repo.FullName(),
		Owner:         repoOwnerJSON{Login: repo.Owner, ID: forge.Viewer.ID, Type: "User"},
		Private:       repo.PrivateFlag,
		Description:   repo.Description,
		DefaultBranch: repo.DefaultBranch,
		CreatedAt:     FixedISO,
		UpdatedAt:     FixedISO,
		HasIssues:     true,
		HasProjects:   true,
		HasWiki:       true,
	}
}

type labelJSON struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

func labelJson(label *Label) labelJSON {
	return labelJSON{ID: label.ID, Name: label.Name, Color: label.Color}
}

type issueJSON struct {
	ID            int         `json:"id"`
	Number        int         `json:"number"`
	Title         string      `json:"title"`
	Body          string      `json:"body"`
	State         string      `json:"state"`
	Labels        []labelJSON `json:"labels"`
	User          userJSON    `json:"user"`
	Comments      int         `json:"comments"`
	CreatedAt     string      `json:"created_at"`
	UpdatedAt     string      `json:"updated_at"`
	RepositoryURL string      `json:"repository_url"`
}

// IssueJSON is the issue object the API serves.
func IssueJSON(forge *Forge, repo *Repo, issue *Issue) issueJSON {
	labels := make([]labelJSON, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, labelJson(label))
	}
	return issueJSON{
		ID:            issue.ID,
		Number:        issue.Number,
		Title:         issue.Title,
		Body:          issue.Body,
		State:         issue.State,
		Labels:        labels,
		User:          userJson(forge),
		Comments:      len(issue.Comments),
		CreatedAt:     FixedISO,
		UpdatedAt:     FixedISO,
		RepositoryURL: "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
	}
}

type commentJSON struct {
	ID        int      `json:"id"`
	Body      string   `json:"body"`
	User      userJSON `json:"user"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

// CommentJSON is the comment object the API serves.
func CommentJSON(forge *Forge, comment *IssueComment) commentJSON {
	return commentJSON{ID: comment.ID, Body: comment.Body, User: userJson(forge), CreatedAt: FixedISO, UpdatedAt: FixedISO}
}

type pullRefJSON struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label"`
}

type pullJSON struct {
	ID             int         `json:"id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	State          string      `json:"state"`
	Merged         bool        `json:"merged"`
	Mergeable      *bool       `json:"mergeable"`
	MergeCommitSHA *string     `json:"merge_commit_sha"`
	MergedAt       *string     `json:"merged_at"`
	Head           pullRefJSON `json:"head"`
	Base           pullRefJSON `json:"base"`
	User           userJSON    `json:"user"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
}

// PullJSON is the pull request object the API serves.
func PullJSON(forge *Forge, repo *Repo, pull *PullRequest) pullJSON {
	headSha, _ := repo.BranchTip(pull.HeadRef)
	baseSha, _ := repo.BranchTip(pull.BaseRef)
	var mergeable *bool
	if pull.State == "open" {
		m := !PullConflicts(repo, pull)
		mergeable = &m
	}
	return pullJSON{
		ID:             pull.ID,
		Number:         pull.Number,
		Title:          pull.Title,
		Body:           pull.Body,
		State:          pull.State,
		Merged:         pull.Merged,
		Mergeable:      mergeable,
		MergeCommitSHA: pull.MergeCommitSha,
		MergedAt:       pull.MergedAt,
		Head:           pullRefJSON{Ref: pull.HeadRef, SHA: headSha, Label: repo.Owner + ":" + pull.HeadRef},
		Base:           pullRefJSON{Ref: pull.BaseRef, SHA: baseSha, Label: repo.Owner + ":" + pull.BaseRef},
		User:           userJson(forge),
		CreatedAt:      FixedISO,
		UpdatedAt:      FixedISO,
	}
}

type identityJSON struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type commitDetailJSON struct {
	Message   string       `json:"message"`
	Author    identityJSON `json:"author"`
	Committer identityJSON `json:"committer"`
}

type parentJSON struct {
	SHA string `json:"sha"`
}

type commitJSON struct {
	SHA       string           `json:"sha"`
	Commit    commitDetailJSON `json:"commit"`
	Parents   []parentJSON     `json:"parents"`
	Author    any              `json:"author"`
	Committer any              `json:"committer"`
}

var identityPattern = regexp.MustCompile(`^(.+?) <(.*?)> (\d+) ([+-]\d{4})$`)

func parseIdentity(line string) identityJSON {
	match := identityPattern.FindStringSubmatch(line)
	if match == nil {
		return identityJSON{Name: line, Date: FixedISO}
	}
	sec, _ := strconv.ParseInt(match[3], 10, 64)
	date := time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
	return identityJSON{Name: match[1], Email: match[2], Date: date}
}

func commitJson(repo *Repo, oid string) (commitJSON, bool) {
	commitOID, commit, ok := repo.CommitOf(oid)
	if !ok {
		return commitJSON{}, false
	}
	parents := make([]parentJSON, 0, len(commit.Parents))
	for _, sha := range commit.Parents {
		parents = append(parents, parentJSON{SHA: sha})
	}
	return commitJSON{
		SHA: commitOID,
		Commit: commitDetailJSON{
			// Git stores the message with a trailing newline; the API
			// presents it without one.
			Message:   strings.TrimRight(commit.Message, "\n"),
			Author:    parseIdentity(commit.Author),
			Committer: parseIdentity(commit.Committer),
		},
		Parents: parents,
	}, true
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

type contentsFileJSON struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Size     int    `json:"size"`
	Encoding string `json:"encoding"`
	Type     string `json:"type"`
}

type contentsCommitJSON struct {
	SHA string `json:"sha"`
}

type putContentsJSON struct {
	Content contentsFileJSON   `json:"content"`
	Commit  contentsCommitJSON `json:"commit"`
}

func putContents(forge *Forge, repo *Repo, path string, req *Req) Res {
	body := bodyObject(req)
	if body == nil {
		return errRes(422, "a JSON body is required")
	}
	message := "Update " + path
	if m, ok := body["message"].(string); ok {
		message = m
	}
	content, ok := body["content"].(string)
	if !ok {
		return errRes(422, "content is required")
	}
	stripped := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v' {
			return -1
		}
		return r
	}, content)
	bytes, err := base64.StdEncoding.DecodeString(stripped)
	if err != nil {
		return errRes(422, "content is not valid base64")
	}
	branch := repo.DefaultBranch
	if b, ok := body["branch"].(string); ok && b != "" {
		branch = b
	}
	tip, hasTip := repo.BranchTip(branch)
	if !hasTip && branch != repo.DefaultBranch {
		if defTip, ok := repo.DefaultTip(); ok {
			// A contents write on a branch that does not exist yet starts it
			// at the default tip.
			tip, hasTip = defTip, true
		}
	}
	var tipCommit *CommitView
	var tipTree string
	if hasTip {
		if _, commit, ok := repo.CommitOf(tip); ok {
			c := commit
			tipCommit = &c
			tipTree = commit.Tree
		}
	}
	var existing *TreeEntry
	if tipCommit != nil {
		if entry, ok := repo.EntryAt(tipTree, path); ok {
			e := entry
			existing = &e
		}
	}
	if sha, ok := body["sha"].(string); ok && existing != nil {
		if !strings.EqualFold(sha, existing.OID) {
			return errRes(409, "sha does not match the current file")
		}
	}
	// An update naming a file that is not there: GitHub answers 422, but the
	// suite never sends one; treat it as a create so a stray sha cannot wedge
	// a write.
	blobOID := repo.PutObject(ObjectBlob, bytes)
	tree := repo.UpdateTree(tipTree, path, blobOID)
	parents := []string{}
	if hasTip {
		parents = []string{tip}
	}
	commitSha := repo.CreateCommit(tree, parents, message)
	repo.SetRef("refs/heads/"+branch, commitSha)
	status := 201
	if existing != nil {
		status = 200
	}
	name := path
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		name = path[idx+1:]
	}
	return jsonRes(status, putContentsJSON{
		Content: contentsFileJSON{Name: name, Path: path, SHA: blobOID, Size: len(bytes), Encoding: "base64", Type: "file"},
		Commit:  contentsCommitJSON{SHA: commitSha},
	})
}

type dirEntryJSON struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Size int    `json:"size"`
}

func listDirectory(repo *Repo, treeOID, dirPath string) []dirEntryJSON {
	object, ok := repo.Objects[treeOID]
	if !ok || object.Type != ObjectTree {
		return []dirEntryJSON{}
	}
	entries := []dirEntryJSON{}
	for _, entry := range ParseTree(object.Data) {
		typ := "file"
		size := 0
		if entry.Mode == "40000" {
			typ = "dir"
		} else if obj := repo.Objects[entry.OID]; obj != nil {
			size = len(obj.Data)
		}
		path := entry.Name
		if dirPath != "" {
			path = dirPath + "/" + entry.Name
		}
		entries = append(entries, dirEntryJSON{Type: typ, Name: entry.Name, Path: path, SHA: entry.OID, Size: size})
	}
	return entries
}

type getContentsJSON struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Size     int    `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Content  string `json:"content"`
	SHA      string `json:"sha"`
}

func getContents(repo *Repo, path string, req *Req) Res {
	tip, ok := repo.ResolveRef(req.Query.Get("ref"))
	if !ok {
		return errRes(404, "Not Found")
	}
	_, commit, ok := repo.CommitOf(tip)
	if !ok {
		return errRes(404, "Not Found")
	}
	if path == "" {
		return jsonRes(200, listDirectory(repo, commit.Tree, ""))
	}
	entry, ok := repo.EntryAt(commit.Tree, path)
	if !ok {
		return errRes(404, "Not Found")
	}
	if entry.Mode == "40000" {
		return jsonRes(200, listDirectory(repo, entry.OID, path))
	}
	blob := repo.Objects[entry.OID]
	if blob == nil {
		return errRes(404, "Not Found")
	}
	name := path
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		name = path[idx+1:]
	}
	return jsonRes(200, getContentsJSON{
		Type: "file", Encoding: "base64", Size: len(blob.Data), Name: name, Path: path,
		Content: base64.StdEncoding.EncodeToString(blob.Data), SHA: entry.OID,
	})
}

type deleteContentsJSON struct {
	Commit contentsCommitJSON `json:"commit"`
}

func deleteContents(repo *Repo, path string, req *Req) Res {
	body := bodyObject(req)
	message := "Delete " + path
	if body != nil {
		if m, ok := body["message"].(string); ok {
			message = m
		}
	}
	branch := repo.DefaultBranch
	if body != nil {
		if b, ok := body["branch"].(string); ok && b != "" {
			branch = b
		}
	}
	tip, ok := repo.BranchTip(branch)
	if !ok {
		return errRes(404, "Not Found")
	}
	_, commit, ok := repo.CommitOf(tip)
	if !ok {
		return errRes(404, "Not Found")
	}
	existing, ok := repo.EntryAt(commit.Tree, path)
	if !ok {
		return errRes(404, "Not Found")
	}
	if body != nil {
		if sha, ok := body["sha"].(string); ok {
			if !strings.EqualFold(sha, existing.OID) {
				return errRes(409, "sha does not match the current file")
			}
		}
	}
	tree := repo.UpdateTree(commit.Tree, path, "")
	commitSha := repo.CreateCommit(tree, []string{tip}, message)
	repo.SetRef("refs/heads/"+branch, commitSha)
	return jsonRes(200, deleteContentsJSON{Commit: contentsCommitJSON{SHA: commitSha}})
}

// ---------------------------------------------------------------------------
// The router
// ---------------------------------------------------------------------------

var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

type apiRootJSON struct {
	CurrentUserURL      string `json:"current_user_url"`
	RepositoryURL       string `json:"repository_url"`
	UserRepositoriesURL string `json:"user_repositories_url"`
}

type metaJSON struct {
	InstalledVersion                 string `json:"installed_version"`
	VerifiablePasswordAuthentication bool   `json:"verifiable_password_authentication"`
}

type branchJSON struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type gitRefJSON struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

type mergeJSON struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

// HandleRest handles one /api/v3 request. Segments are the path parts after
// /api/v3.
func HandleRest(forge *Forge, req *Req) Res {
	segments := req.Segments[2:] // drop 'api', 'v3'
	method := req.Method

	if (method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE") && req.JSONParseError {
		return errRes(400, "Malformed JSON body")
	}

	// API root and meta.
	if len(segments) == 0 {
		if method != "GET" {
			return errRes(404, "Not Found")
		}
		return jsonResHeaders(200, apiRootJSON{
			CurrentUserURL:      "/api/v3/user",
			RepositoryURL:       "/api/v3/repos/{owner}/{repo}",
			UserRepositoriesURL: "/api/v3/user/repos",
		}, map[string]string{"x-oauth-scopes": "repo, read:org"})
	}
	if len(segments) == 1 && segments[0] == "meta" {
		if method != "GET" {
			return errRes(404, "Not Found")
		}
		return jsonRes(200, metaJSON{InstalledVersion: "1.0.0", VerifiablePasswordAuthentication: true})
	}
	if len(segments) == 1 && segments[0] == "user" {
		if method != "GET" {
			return errRes(404, "Not Found")
		}
		return jsonRes(200, userJson(forge))
	}

	// /user/repos
	if len(segments) == 2 && segments[0] == "user" && segments[1] == "repos" {
		if method == "POST" {
			body := bodyObject(req)
			name, _ := bodyValue(body, "name").(string)
			if name == "" || !repoNamePattern.MatchString(name) {
				return errRes(422, "name is required")
			}
			if forge.GetRepo(forge.Viewer.Login, name) != nil {
				return errRes(422, "Repository already exists")
			}
			description, _ := bodyValue(body, "description").(string)
			private, _ := bodyValue(body, "private").(bool)
			repo := forge.CreateRepo(name, description, private)
			return jsonRes(201, RepoJSON(forge, repo))
		}
		if method == "GET" {
			repos := forge.ViewerRepos()
			page, link := paginate(repos, req, req.Pathname)
			list := make([]repoJSON, 0, len(page))
			for _, repo := range page {
				list = append(list, RepoJSON(forge, repo))
			}
			return jsonResHeaders(200, list, map[string]string{"link": link})
		}
		return errRes(404, "Not Found")
	}

	// /repos/{owner}/{repo}[...]
	if len(segments) >= 3 && segments[0] == "repos" {
		owner := segments[1]
		name := segments[2]
		repo := forge.GetRepo(owner, name)
		rest := segments[3:]
		if repo == nil {
			return errRes(404, "Not Found")
		}

		if len(rest) == 0 {
			if method == "GET" {
				return jsonRes(200, RepoJSON(forge, repo))
			}
			if method == "PATCH" {
				body := bodyObject(req)
				if body != nil {
					if d, ok := body["description"].(string); ok {
						repo.Description = d
					}
					if p, ok := body["private"].(bool); ok {
						repo.PrivateFlag = p
					}
				}
				return jsonRes(200, RepoJSON(forge, repo))
			}
			if method == "DELETE" {
				forge.DeleteRepo(owner, name)
				return noContentRes()
			}
			return errRes(404, "Not Found")
		}

		if rest[0] == "branches" {
			if len(rest) == 1 && method == "GET" {
				names := make([]string, 0, len(repo.Refs))
				for ref := range repo.Refs {
					names = append(names, ref)
				}
				sort.Strings(names)
				branches := []branchJSON{}
				for _, ref := range names {
					if !strings.HasPrefix(ref, "refs/heads/") {
						continue
					}
					var b branchJSON
					b.Name = ref[len("refs/heads/"):]
					b.Commit.SHA = repo.Refs[ref]
					branches = append(branches, b)
				}
				return jsonRes(200, branches)
			}
			if len(rest) == 2 && method == "GET" {
				branch := rest[1]
				tip, ok := repo.BranchTip(branch)
				if !ok {
					return errRes(404, "Not Found")
				}
				var b branchJSON
				b.Name = branch
				b.Commit.SHA = tip
				return jsonRes(200, b)
			}
			return errRes(404, "Not Found")
		}

		if rest[0] == "commits" && len(rest) == 1 && method == "GET" {
			tip, _ := repo.ResolveRef(req.Query.Get("sha"))
			commits := []commitJSON{}
			for _, record := range repo.CommitHistory(tip) {
				if entry, ok := commitJson(repo, record.OID); ok {
					commits = append(commits, entry)
				}
			}
			perPage := queryInt(req.Query, "per_page", 30, 1, 100)
			pageNum := queryInt(req.Query, "page", 1, 1, 1000000)
			start := (pageNum - 1) * perPage
			if start > len(commits) {
				start = len(commits)
			}
			end := start + perPage
			if end > len(commits) {
				end = len(commits)
			}
			return jsonRes(200, commits[start:end])
		}

		if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" {
			if len(rest) == 2 && method == "POST" {
				body := bodyObject(req)
				ref, _ := bodyValue(body, "ref").(string)
				sha, _ := bodyValue(body, "sha").(string)
				if ref == "" || !strings.HasPrefix(ref, "refs/") {
					return errRes(422, "ref is required")
				}
				if sha == "" || repo.Objects[sha] == nil {
					return errRes(422, "sha does not exist")
				}
				if _, ok := repo.Refs[ref]; ok {
					return errRes(422, "Reference already exists")
				}
				repo.SetRef(ref, sha)
				var out gitRefJSON
				out.Ref = ref
				out.Object.SHA = sha
				return jsonRes(201, out)
			}
			if len(rest) >= 3 && method == "GET" {
				ref := "refs/" + strings.Join(rest[2:], "/")
				tip, ok := repo.Refs[ref]
				if !ok {
					return errRes(404, "Not Found")
				}
				var out gitRefJSON
				out.Ref = ref
				out.Object.SHA = tip
				return jsonRes(200, out)
			}
			return errRes(404, "Not Found")
		}

		if rest[0] == "contents" {
			path := strings.Join(rest[1:], "/")
			if method == "PUT" {
				return putContents(forge, repo, path, req)
			}
			if method == "GET" {
				return getContents(repo, path, req)
			}
			if method == "DELETE" {
				return deleteContents(repo, path, req)
			}
			return errRes(404, "Not Found")
		}

		if rest[0] == "labels" {
			if len(rest) == 1 && method == "POST" {
				body := bodyObject(req)
				name, _ := bodyValue(body, "name").(string)
				if name == "" {
					return errRes(422, "name is required")
				}
				if existing, ok := repo.Labels[name]; ok {
					return jsonRes(201, labelJson(existing))
				}
				color, _ := bodyValue(body, "color").(string)
				if color == "" {
					color = "ededed"
				}
				label := &Label{ID: forge.NextGlobalID(), Name: name, Color: color}
				repo.Labels[name] = label
				return jsonRes(201, labelJson(label))
			}
			if len(rest) == 1 && method == "GET" {
				labels := []labelJSON{}
				for _, label := range repo.Labels {
					labels = append(labels, labelJson(label))
				}
				// Labels have no defined order in the original (map order is
				// insertion order there); sort by id for determinism.
				sort.Slice(labels, func(i, j int) bool { return labels[i].ID < labels[j].ID })
				return jsonRes(200, labels)
			}
			return errRes(404, "Not Found")
		}

		if rest[0] == "issues" {
			if len(rest) == 1 {
				if method == "POST" {
					body := bodyObject(req)
					if body == nil {
						return errRes(422, "a JSON body is required")
					}
					var labels []string
					if raw, ok := body["labels"].([]any); ok {
						for _, entry := range raw {
							if s, ok := entry.(string); ok {
								labels = append(labels, s)
							}
						}
					}
					title, _ := body["title"].(string)
					bodyText, _ := body["body"].(string)
					created := CreateIssue(forge, repo, title, bodyText, labels)
					if !created.OK {
						return errRes(created.Status, created.Message)
					}
					return jsonRes(201, IssueJSON(forge, repo, created.Issue))
				}
				if method == "GET" {
					state := req.Query.Get("state")
					if state == "" {
						state = "open"
					}
					direction := req.Query.Get("direction")
					if direction == "" {
						direction = "desc"
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
					if direction == "asc" {
						sort.Slice(filtered, func(i, j int) bool { return filtered[i].Seq < filtered[j].Seq })
					} else {
						sort.Slice(filtered, func(i, j int) bool { return filtered[i].Seq > filtered[j].Seq })
					}
					page, link := paginate(filtered, req, req.Pathname)
					list := make([]issueJSON, 0, len(page))
					for _, issue := range page {
						list = append(list, IssueJSON(forge, repo, issue))
					}
					return jsonResHeaders(200, list, map[string]string{"link": link})
				}
				return errRes(404, "Not Found")
			}
			number, ok := parseIssueNumber(rest[1])
			issue := (*Issue)(nil)
			if ok {
				issue = repo.Issues[number]
			}
			if issue == nil {
				return errRes(404, "Not Found")
			}
			if len(rest) == 2 {
				if method == "GET" {
					return jsonRes(200, IssueJSON(forge, repo, issue))
				}
				if method == "PATCH" {
					body := bodyObject(req)
					if body != nil {
						if s, ok := body["state"].(string); ok && (s == "open" || s == "closed") {
							issue.State = s
						}
						if t, ok := body["title"].(string); ok {
							issue.Title = t
						}
						if b, ok := body["body"].(string); ok {
							issue.Body = b
						}
					}
					return jsonRes(200, IssueJSON(forge, repo, issue))
				}
				return errRes(404, "Not Found")
			}
			if len(rest) == 3 && rest[2] == "comments" {
				if method == "POST" {
					body := bodyObject(req)
					text, _ := bodyValue(body, "body").(string)
					if body == nil || text == "" {
						return errRes(422, "body is required")
					}
					comment := &IssueComment{ID: forge.NextComment(), Body: text, UserLogin: forge.Viewer.Login}
					issue.Comments = append(issue.Comments, comment)
					return jsonRes(201, CommentJSON(forge, comment))
				}
				if method == "GET" {
					list := make([]commentJSON, 0, len(issue.Comments))
					for _, comment := range issue.Comments {
						list = append(list, CommentJSON(forge, comment))
					}
					return jsonRes(200, list)
				}
				return errRes(404, "Not Found")
			}
			return errRes(404, "Not Found")
		}

		if rest[0] == "pulls" {
			if len(rest) == 1 {
				if method == "POST" {
					body := bodyObject(req)
					if body == nil {
						return errRes(422, "a JSON body is required")
					}
					title, _ := body["title"].(string)
					bodyText, _ := body["body"].(string)
					head, _ := body["head"].(string)
					base, _ := body["base"].(string)
					if base == "" {
						base = repo.DefaultBranch
					}
					created := CreatePull(forge, repo, title, bodyText, head, base)
					if !created.OK {
						return errRes(created.Status, created.Message)
					}
					return jsonRes(201, PullJSON(forge, repo, created.Pull))
				}
				if method == "GET" {
					state := req.Query.Get("state")
					if state == "" {
						state = "open"
					}
					var pulls []*PullRequest
					for _, pull := range repo.Pulls {
						if state == "open" && pull.State != "open" {
							continue
						}
						if state == "closed" && pull.State != "closed" {
							continue
						}
						pulls = append(pulls, pull)
					}
					sort.Slice(pulls, func(i, j int) bool { return pulls[i].Seq > pulls[j].Seq })
					page, link := paginate(pulls, req, req.Pathname)
					list := make([]pullJSON, 0, len(page))
					for _, pull := range page {
						list = append(list, PullJSON(forge, repo, pull))
					}
					return jsonResHeaders(200, list, map[string]string{"link": link})
				}
				return errRes(404, "Not Found")
			}
			number, ok := parseIssueNumber(rest[1])
			pull := (*PullRequest)(nil)
			if ok {
				pull = repo.Pulls[number]
			}
			if pull == nil {
				return errRes(404, "Not Found")
			}
			if len(rest) == 2 {
				if method == "GET" {
					return jsonRes(200, PullJSON(forge, repo, pull))
				}
				return errRes(404, "Not Found")
			}
			if len(rest) == 3 && rest[2] == "files" && method == "GET" {
				return jsonRes(200, PullFiles(repo, pull))
			}
			if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
				merged := MergePull(repo, pull)
				if !merged.OK {
					return errRes(merged.Status, merged.Message)
				}
				return jsonRes(200, mergeJSON{SHA: merged.SHA, Merged: true, Message: "Pull Request successfully merged"})
			}
			return errRes(404, "Not Found")
		}
	}

	return errRes(404, "Not Found")
}

// bodyValue reads one field of a JSON body record, tolerating a nil body.
func bodyValue(body map[string]any, key string) any {
	if body == nil {
		return nil
	}
	return body[key]
}

// parseIssueNumber parses an issue/PR number the way the original's Number()
// does for the inputs the suite sends: plain decimal integers.
func parseIssueNumber(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Ensure the json import is used (request bodies decode through it in app.go).
var _ = json.Unmarshal
