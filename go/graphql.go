// GitHub GraphQL v4, for the documents gh and github-mcp-server send: a small
// lexer, a recursive-descent parser for the query/mutation subset the suite
// uses, and an executor over the shared store. Introspection answers the
// __type probes gh feature-detects with.
//
// Supported: named operations with variable definitions, aliases, arguments
// (variables, strings, ints, enums, lists, input objects, null), inline
// fragments, batched arrays. Not supported: fragment definitions and spreads,
// directives, subscriptions — the suite sends none of those.
//
// Errors surface as panics carrying a gqlError, caught by executeOperation and
// rendered as {data: null, errors: [{message}]} — the same shape the original
// reaches through exceptions.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// gqlError is a GraphQL failure: a lex/parse error, an unknown field, or a
// mutation against an unknown id.
type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

func failGql(format string, args ...any) {
	panic(&gqlError{msg: fmt.Sprintf(format, args...)})
}

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type lexKind int

const (
	tokName lexKind = iota
	tokString
	tokInt
	tokFloat
	tokPunct
	tokEOF
)

type lexToken struct {
	kind  lexKind
	value string
}

// tokenize splits a GraphQL document into tokens. It panics with a gqlError on
// the first bad character.
func tokenize(input string) []lexToken {
	var tokens []lexToken
	i := 0
	for i < len(input) {
		ch := input[i]
		// Whitespace, commas and a byte-order mark are skipped.
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if ch == 0xEF && i+2 < len(input) && input[i+1] == 0xBB && input[i+2] == 0xBF {
			i += 3
			continue
		}
		if ch == '#' {
			for i < len(input) && input[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '.' && i+2 < len(input) && input[i+1] == '.' && input[i+2] == '.' {
			tokens = append(tokens, lexToken{tokPunct, "..."})
			i += 3
			continue
		}
		if strings.IndexByte("{}():$![]=@|", ch) >= 0 {
			tokens = append(tokens, lexToken{tokPunct, string(ch)})
			i++
			continue
		}
		if ch == '"' {
			if i+2 < len(input) && input[i+1] == '"' && input[i+2] == '"' {
				end := strings.Index(input[i+3:], `"""`)
				if end < 0 {
					failGql("unterminated block string")
				}
				tokens = append(tokens, lexToken{tokString, input[i+3 : i+3+end]})
				i = i + 3 + end + 3
				continue
			}
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
					case 'b':
						out.WriteByte('\b')
					case 'f':
						out.WriteByte('\f')
					case 'u':
						// Up to four hex digits; the scan always consumes
						// four characters, like the original's slice.
						code := 0
						for k := 0; k < 4 && j+2+k < len(input); k++ {
							d := hexVal(input[j+2+k])
							if d < 0 {
								break
							}
							code = code*16 + d
						}
						out.WriteRune(rune(code))
						j += 4
					default:
						out.WriteByte(esc)
					}
					j += 2
					continue
				}
				out.WriteByte(input[j])
				j++
			}
			if j >= len(input) {
				failGql("unterminated string")
			}
			tokens = append(tokens, lexToken{tokString, out.String()})
			i = j + 1
			continue
		}
		if ch == '-' || (ch >= '0' && ch <= '9') {
			j := i
			if input[j] == '-' {
				j++
			}
			for j < len(input) && input[j] >= '0' && input[j] <= '9' {
				j++
			}
			floating := false
			if j+1 < len(input) && input[j] == '.' && input[j+1] >= '0' && input[j+1] <= '9' {
				floating = true
				j++
				for j < len(input) && input[j] >= '0' && input[j] <= '9' {
					j++
				}
			}
			if j < len(input) && (input[j] == 'e' || input[j] == 'E') {
				floating = true
				j++
				if j < len(input) && (input[j] == '+' || input[j] == '-') {
					j++
				}
				for j < len(input) && input[j] >= '0' && input[j] <= '9' {
					j++
				}
			}
			kind := tokInt
			if floating {
				kind = tokFloat
			}
			tokens = append(tokens, lexToken{kind, input[i:j]})
			i = j
			continue
		}
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch == '_' {
			j := i
			for j < len(input) {
				c := input[j]
				if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
					j++
				} else {
					break
				}
			}
			tokens = append(tokens, lexToken{tokName, input[i:j]})
			i = j
			continue
		}
		// A bad character: report it the way the original's JSON.stringify does.
		r, _ := utf8.DecodeRuneInString(input[i:])
		quoted, _ := json.Marshal(string(r))
		failGql("unexpected character %s", string(quoted))
	}
	tokens = append(tokens, lexToken{tokEOF, ""})
	return tokens
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// ---------------------------------------------------------------------------
// Parser (AST)
// ---------------------------------------------------------------------------

type valueKind int

const (
	valVariable valueKind = iota
	valString
	valInt
	valFloat
	valBoolean
	valNull
	valEnum
	valList
	valObject
)

// valueExpr is one literal value in a document.
type valueExpr struct {
	kind    valueKind
	str     string
	num     float64
	boolean bool
	list    []*valueExpr
	fields  []objectField
}

type objectField struct {
	name  string
	value *valueExpr
}

type argExpr struct {
	name  string
	value *valueExpr
}

// selection is one field or inline fragment in a selection set.
type selection struct {
	isFragment bool
	// Field:
	alias string
	name  string
	args  []argExpr
	// Fragment:
	typeCondition string // "" means no condition
	selections    []*selection
}

type varDef struct {
	name         string
	defaultValue *valueExpr // nil means no default
}

type operation struct {
	kind       string // "query" or "mutation"
	name       string // "" means anonymous
	variables  []varDef
	selections []*selection
}

type docParser struct {
	tokens []lexToken
	pos    int
}

func (p *docParser) peek() lexToken { return p.tokens[p.pos] }

func (p *docParser) advance() lexToken {
	token := p.tokens[p.pos]
	if p.pos < len(p.tokens)-1 {
		p.pos++
	}
	return token
}

func (p *docParser) expectPunct(value string) {
	token := p.advance()
	if token.kind != tokPunct || token.value != value {
		quoted, _ := json.Marshal(token.value)
		failGql("expected %q, got %s", value, string(quoted))
	}
}

func (p *docParser) expectName(want ...string) string {
	token := p.advance()
	if token.kind != tokName {
		quoted, _ := json.Marshal(token.value)
		failGql("expected a name, got %s", string(quoted))
	}
	if len(want) > 0 && token.value != want[0] {
		failGql("expected %q, got %q", want[0], token.value)
	}
	return token.value
}

// skipDirectives parses directives and discards them (the suite sends none
// that change meaning).
func (p *docParser) skipDirectives() {
	for p.peek().kind == tokPunct && p.peek().value == "@" {
		p.advance()
		p.expectName()
		if p.peek().kind == tokPunct && p.peek().value == "(" {
			p.readArguments()
		}
	}
}

func (p *docParser) readDocument() []*operation {
	var operations []*operation
	for p.peek().kind != tokEOF {
		operations = append(operations, p.readOperation())
	}
	return operations
}

func (p *docParser) readOperation() *operation {
	kind := "query"
	name := ""
	token := p.peek()
	if token.kind == tokName && (token.value == "query" || token.value == "mutation") {
		kind = p.advance().value
		if p.peek().kind == tokName {
			name = p.advance().value
		}
	} else if token.kind == tokName && token.value == "subscription" {
		failGql("subscriptions are not supported")
	} else if token.kind == tokName && token.value == "fragment" {
		failGql("fragment definitions are not supported")
	}
	variables := p.readVariableDefinitions()
	p.skipDirectives()
	selections := p.readSelectionSet()
	return &operation{kind: kind, name: name, variables: variables, selections: selections}
}

func (p *docParser) readVariableDefinitions() []varDef {
	var variables []varDef
	if !(p.peek().kind == tokPunct && p.peek().value == "(") {
		return variables
	}
	p.advance()
	for !(p.peek().kind == tokPunct && p.peek().value == ")") {
		p.expectPunct("$")
		name := p.expectName()
		p.expectPunct(":")
		p.readType()
		var defaultValue *valueExpr
		if p.peek().kind == tokPunct && p.peek().value == "=" {
			p.advance()
			defaultValue = p.readValue(true)
		}
		p.skipDirectives()
		variables = append(variables, varDef{name: name, defaultValue: defaultValue})
	}
	p.expectPunct(")")
	return variables
}

// readType consumes a type reference, which execution needs no record of.
func (p *docParser) readType() {
	if p.peek().kind == tokPunct && p.peek().value == "[" {
		p.advance()
		p.readType()
		p.expectPunct("]")
	} else {
		p.expectName()
	}
	if p.peek().kind == tokPunct && p.peek().value == "!" {
		p.advance()
	}
}

func (p *docParser) readSelectionSet() []*selection {
	p.expectPunct("{")
	var selections []*selection
	for !(p.peek().kind == tokPunct && p.peek().value == "}") {
		selections = append(selections, p.readSelection())
	}
	p.expectPunct("}")
	return selections
}

func (p *docParser) readSelection() *selection {
	if p.peek().kind == tokPunct && p.peek().value == "..." {
		p.advance()
		typeCondition := ""
		if p.peek().kind == tokName && p.peek().value == "on" {
			p.advance()
			typeCondition = p.expectName()
		} else if p.peek().kind == tokName {
			failGql("fragment spreads are not supported")
		}
		p.skipDirectives()
		return &selection{isFragment: true, typeCondition: typeCondition, selections: p.readSelectionSet()}
	}
	name := p.expectName()
	alias := name
	if p.peek().kind == tokPunct && p.peek().value == ":" {
		p.advance()
		alias = name
		name = p.expectName()
	}
	args := p.readArguments()
	p.skipDirectives()
	var selections []*selection
	if p.peek().kind == tokPunct && p.peek().value == "{" {
		selections = p.readSelectionSet()
	}
	return &selection{alias: alias, name: name, args: args, selections: selections}
}

func (p *docParser) readArguments() []argExpr {
	var args []argExpr
	if !(p.peek().kind == tokPunct && p.peek().value == "(") {
		return args
	}
	p.advance()
	for !(p.peek().kind == tokPunct && p.peek().value == ")") {
		name := p.expectName()
		p.expectPunct(":")
		args = append(args, argExpr{name: name, value: p.readValue(false)})
	}
	p.expectPunct(")")
	return args
}

func (p *docParser) readValue(constValue bool) *valueExpr {
	token := p.advance()
	if token.kind == tokPunct && token.value == "$" {
		if constValue {
			failGql("a variable is not allowed here")
		}
		return &valueExpr{kind: valVariable, str: p.expectName()}
	}
	if token.kind == tokString {
		return &valueExpr{kind: valString, str: token.value}
	}
	if token.kind == tokInt {
		num, _ := strconv.ParseFloat(token.value, 64)
		return &valueExpr{kind: valInt, num: num}
	}
	if token.kind == tokFloat {
		num, _ := strconv.ParseFloat(token.value, 64)
		return &valueExpr{kind: valFloat, num: num}
	}
	if token.kind == tokPunct && token.value == "[" {
		var values []*valueExpr
		for !(p.peek().kind == tokPunct && p.peek().value == "]") {
			values = append(values, p.readValue(constValue))
		}
		p.expectPunct("]")
		return &valueExpr{kind: valList, list: values}
	}
	if token.kind == tokPunct && token.value == "{" {
		var fields []objectField
		for !(p.peek().kind == tokPunct && p.peek().value == "}") {
			name := p.expectName()
			p.expectPunct(":")
			fields = append(fields, objectField{name: name, value: p.readValue(constValue)})
		}
		p.expectPunct("}")
		return &valueExpr{kind: valObject, fields: fields}
	}
	if token.kind == tokName {
		switch token.value {
		case "true":
			return &valueExpr{kind: valBoolean, boolean: true}
		case "false":
			return &valueExpr{kind: valBoolean, boolean: false}
		case "null":
			return &valueExpr{kind: valNull}
		default:
			return &valueExpr{kind: valEnum, str: token.value}
		}
	}
	quoted, _ := json.Marshal(token.value)
	failGql("unexpected value %s", string(quoted))
	return nil
}

// ---------------------------------------------------------------------------
// Values and the executor
// ---------------------------------------------------------------------------

// typedValue is a GraphQL object value: its type name plus its domain payload.
type typedValue struct {
	t string
	v any
}

func wrap(t string, v any) typedValue { return typedValue{t: t, v: v} }

type variables map[string]any

func evalValue(node *valueExpr, vars variables) any {
	switch node.kind {
	case valVariable:
		return vars[node.str]
	case valString, valEnum:
		return node.str
	case valInt, valFloat:
		return node.num
	case valBoolean:
		return node.boolean
	case valNull:
		return nil
	case valList:
		out := make([]any, 0, len(node.list))
		for _, entry := range node.list {
			out = append(out, evalValue(entry, vars))
		}
		return out
	case valObject:
		out := map[string]any{}
		for _, field := range node.fields {
			out[field.name] = evalValue(field.value, vars)
		}
		return out
	}
	return nil
}

func evalArgs(field *selection, vars variables) map[string]any {
	out := map[string]any{}
	for _, arg := range field.args {
		out[arg.name] = evalValue(arg.value, vars)
	}
	return out
}

// ---------------------------------------------------------------------------
// Introspection data
// ---------------------------------------------------------------------------

var typeFields = map[string][]string{
	"Issue":                            {"id", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "comments", "labels", "closed", "url"},
	"PullRequest":                      {"id", "number", "title", "state", "merged", "mergeable", "headRefName", "baseRefName", "files", "body", "createdAt", "updatedAt", "author", "mergedAt", "mergeCommit", "isDraft"},
	"Repository":                       {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate", "createdAt", "updatedAt"},
	"User":                             {"id", "login", "name", "email", "url"},
	"Query":                            {"viewer", "repository", "node"},
	"Mutation":                         {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"IssueConnection":                  {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection":            {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changeType"},
	"PageInfo":                         {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"Ref":                              {"name", "target"},
}

var typeEnumValues = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":         {"ASC", "DESC"},
}

// ---------------------------------------------------------------------------
// Resolvers
// ---------------------------------------------------------------------------

type execCtx struct {
	forge *Forge
}

// findRepoByID finds a repository by its numeric id (Relay ids only carry the
// id).
func findRepoByID(forge *Forge, id int) *Repo {
	for _, repo := range forge.ViewerRepos() {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

// makeCursor is the page cursor for an issue's creation sequence.
func makeCursor(seq int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("issue-cursor:" + itoa(seq)))
}

var cursorPattern = regexp.MustCompile(`^issue-cursor:(\d+)$`)

// cursorSeq is the creation sequence a page cursor names, or -1 when it names
// none.
func cursorSeq(cursor any) int {
	s, ok := cursor.(string)
	if !ok || s == "" {
		return -1
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return -1
		}
	}
	match := cursorPattern.FindSubmatch(raw)
	if match == nil {
		return -1
	}
	n, _ := strconv.Atoi(string(match[1]))
	return n
}

func issueStateName(issue *Issue) string {
	if issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func pullStateName(pull *PullRequest) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

// Parent payloads for nested resolvers.

type issueParent struct {
	repo  *Repo
	issue *Issue
}

type pullParent struct {
	repo *Repo
	pull *PullRequest
}

type commentParent struct {
	repo    *Repo
	issue   *Issue
	comment *IssueComment
}

type issueConnParent struct {
	repo        *Repo
	issues      []*Issue
	hasNextPage bool
}

type pullConnParent struct {
	repo  *Repo
	pulls []*PullRequest
}

type fileConnParent struct {
	files []string // paths only
}

type refParent struct{ name string }

type namedParent struct{ name string }

// strArg renders an argument the way the original's String(x ?? ”) does.
func strArg(args map[string]any, name string) string {
	value, ok := args[name]
	if !ok || value == nil {
		return ""
	}
	return jsString(value)
}

func jsString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return jsNumber(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// jsNumber formats a number the way JavaScript's String(number) does for the
// magnitudes the suite sends.
func jsNumber(n float64) string {
	if n == float64(int64(n)) && n >= -1e15 && n <= 1e15 {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'g', -1, 64)
}

// numArg reads a numeric argument the way the original's Number() does for the
// values the suite sends.
func numArg(args map[string]any, name string) (float64, bool) {
	value, ok := args[name]
	if !ok || value == nil {
		return 0, false
	}
	switch v := value.(type) {
	case float64:
		return v, true
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		return n, true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// inputObject is the `input` argument of a mutation, or an empty record.
func inputObject(args map[string]any) map[string]any {
	if m, ok := args["input"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

type fieldResolver func(parent any, args map[string]any, ctx *execCtx) any

var fieldResolvers = map[string]map[string]fieldResolver{
	"Query": {
		"viewer": func(_ any, _ map[string]any, ctx *execCtx) any {
			return wrap("User", ctx.forge.Viewer)
		},
		"repository": func(_ any, args map[string]any, ctx *execCtx) any {
			repo := ctx.forge.GetRepo(strArg(args, "owner"), strArg(args, "name"))
			if repo == nil {
				return nil
			}
			return wrap("Repository", repo)
		},
		"node": func(_ any, args map[string]any, ctx *execCtx) any {
			decoded, ok := DecodeGlobalID(strArg(args, "id"))
			if !ok {
				return nil
			}
			repo := findRepoByID(ctx.forge, decoded.RepoID)
			if repo == nil {
				return nil
			}
			switch decoded.Kind {
			case "Repository":
				return wrap("Repository", repo)
			case "Issue":
				issue := repo.Issues[decoded.Number]
				if issue == nil {
					return nil
				}
				return wrap("Issue", issueParent{repo, issue})
			case "PullRequest":
				pull := repo.Pulls[decoded.Number]
				if pull == nil {
					return nil
				}
				return wrap("PullRequest", pullParent{repo, pull})
			default:
				return nil
			}
		},
		"__type": func(_ any, args map[string]any, _ *execCtx) any {
			name := strArg(args, "name")
			if _, ok := typeFields[name]; ok {
				return wrap("__Type", name)
			}
			if _, ok := typeEnumValues[name]; ok {
				return wrap("__Type", name)
			}
			return nil
		},
	},
	"Mutation": {
		"createIssue": func(_ any, args map[string]any, ctx *execCtx) any {
			input := inputObject(args)
			decoded, ok := DecodeGlobalID(strArg(input, "repositoryId"))
			if !ok || decoded.Kind != "Repository" {
				failGql("createIssue: unknown repositoryId")
			}
			repo := findRepoByID(ctx.forge, decoded.RepoID)
			if repo == nil {
				failGql("createIssue: unknown repositoryId")
			}
			created := CreateIssue(ctx.forge, repo, strArg(input, "title"), strArg(input, "body"), nil)
			if !created.OK {
				failGql("createIssue: %s", created.Message)
			}
			return wrap("CreateIssuePayload", issueParent{repo, created.Issue})
		},
		"closeIssue": func(_ any, args map[string]any, ctx *execCtx) any {
			input := inputObject(args)
			decoded, ok := DecodeGlobalID(strArg(input, "issueId"))
			if !ok || decoded.Kind != "Issue" {
				failGql("closeIssue: unknown issueId")
			}
			repo := findRepoByID(ctx.forge, decoded.RepoID)
			var issue *Issue
			if repo != nil {
				issue = repo.Issues[decoded.Number]
			}
			if repo == nil || issue == nil {
				failGql("closeIssue: unknown issueId")
			}
			CloseIssue(issue)
			return wrap("CloseIssuePayload", issueParent{repo, issue})
		},
		"createPullRequest": func(_ any, args map[string]any, ctx *execCtx) any {
			input := inputObject(args)
			decoded, ok := DecodeGlobalID(strArg(input, "repositoryId"))
			if !ok || decoded.Kind != "Repository" {
				failGql("createPullRequest: unknown repositoryId")
			}
			repo := findRepoByID(ctx.forge, decoded.RepoID)
			if repo == nil {
				failGql("createPullRequest: unknown repositoryId")
			}
			base := strArg(input, "baseRefName")
			if _, present := input["baseRefName"]; !present || input["baseRefName"] == nil {
				base = repo.DefaultBranch
			}
			created := CreatePull(ctx.forge, repo, strArg(input, "title"), strArg(input, "body"), strArg(input, "headRefName"), base)
			if !created.OK {
				failGql("createPullRequest: %s", created.Message)
			}
			return wrap("CreatePullRequestPayload", pullParent{repo, created.Pull})
		},
		"mergePullRequest": func(_ any, args map[string]any, ctx *execCtx) any {
			input := inputObject(args)
			decoded, ok := DecodeGlobalID(strArg(input, "pullRequestId"))
			if !ok || decoded.Kind != "PullRequest" {
				failGql("mergePullRequest: unknown pullRequestId")
			}
			repo := findRepoByID(ctx.forge, decoded.RepoID)
			var pull *PullRequest
			if repo != nil {
				pull = repo.Pulls[decoded.Number]
			}
			if repo == nil || pull == nil {
				failGql("mergePullRequest: unknown pullRequestId")
			}
			merged := MergePull(repo, pull)
			if !merged.OK {
				failGql("mergePullRequest: %s", merged.Message)
			}
			return wrap("MergePullRequestPayload", pullParent{repo, pull})
		},
		"addComment": func(_ any, args map[string]any, ctx *execCtx) any {
			input := inputObject(args)
			decoded, ok := DecodeGlobalID(strArg(input, "subjectId"))
			if !ok || decoded.Kind != "Issue" {
				failGql("addComment: unknown subjectId")
			}
			repo := findRepoByID(ctx.forge, decoded.RepoID)
			var issue *Issue
			if repo != nil {
				issue = repo.Issues[decoded.Number]
			}
			if repo == nil || issue == nil {
				failGql("addComment: unknown subjectId")
			}
			comment := &IssueComment{ID: ctx.forge.NextComment(), Body: strArg(input, "body"), UserLogin: ctx.forge.Viewer.Login}
			issue.Comments = append(issue.Comments, comment)
			return wrap("AddCommentPayload", commentParent{repo, issue, comment})
		},
	},
	"User": {
		"login": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(ScenarioUser).Login },
		"id":    func(parent any, _ map[string]any, _ *execCtx) any { return itoa(parent.(ScenarioUser).ID) },
		"name":  func(parent any, _ map[string]any, _ *execCtx) any { return parent.(ScenarioUser).Name },
		"email": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(ScenarioUser).Email },
	},
	"Repository": {
		"id":            func(parent any, _ map[string]any, _ *execCtx) any { return RepoGlobalID(parent.(*Repo)) },
		"databaseId":    func(parent any, _ map[string]any, _ *execCtx) any { return parent.(*Repo).ID },
		"name":          func(parent any, _ map[string]any, _ *execCtx) any { return parent.(*Repo).Name },
		"nameWithOwner": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(*Repo).FullName() },
		"owner":         func(_ any, _ map[string]any, ctx *execCtx) any { return wrap("User", ctx.forge.Viewer) },
		"defaultBranchRef": func(parent any, _ map[string]any, _ *execCtx) any {
			repo := parent.(*Repo)
			if _, ok := repo.DefaultTip(); !ok {
				return nil
			}
			return wrap("Ref", refParent{repo.DefaultBranch})
		},
		"viewerPermission": func(_ any, _ map[string]any, _ *execCtx) any { return "ADMIN" },
		"hasIssuesEnabled": func(_ any, _ map[string]any, _ *execCtx) any { return true },
		"description":      func(parent any, _ map[string]any, _ *execCtx) any { return parent.(*Repo).Description },
		"isPrivate":        func(parent any, _ map[string]any, _ *execCtx) any { return parent.(*Repo).PrivateFlag },
		"issue": func(parent any, args map[string]any, _ *execCtx) any {
			repo := parent.(*Repo)
			num, ok := numArg(args, "number")
			if !ok {
				return nil
			}
			issue := repo.Issues[int(num)]
			if issue == nil {
				return nil
			}
			return wrap("Issue", issueParent{repo, issue})
		},
		"issues": func(parent any, args map[string]any, _ *execCtx) any {
			repo := parent.(*Repo)
			issues := make([]*Issue, 0, len(repo.Issues))
			for _, issue := range repo.Issues {
				issues = append(issues, issue)
			}
			if states, ok := args["states"].([]any); ok && len(states) > 0 {
				wanted := map[string]bool{}
				for _, s := range states {
					wanted[jsString(s)] = true
				}
				filtered := issues[:0]
				for _, issue := range issues {
					if wanted[issueStateName(issue)] {
						filtered = append(filtered, issue)
					}
				}
				issues = filtered
			}
			orderBy, _ := args["orderBy"].(map[string]any)
			field, _ := orderBy["field"].(string)
			direction, _ := orderBy["direction"].(string)
			switch {
			case field == "CREATED_AT" && direction == "DESC":
				sort.Slice(issues, func(i, j int) bool {
					if issues[i].Seq != issues[j].Seq {
						return issues[i].Seq > issues[j].Seq
					}
					return issues[i].Number > issues[j].Number
				})
			case field == "CREATED_AT" && direction == "ASC":
				sort.Slice(issues, func(i, j int) bool {
					if issues[i].Seq != issues[j].Seq {
						return issues[i].Seq < issues[j].Seq
					}
					return issues[i].Number < issues[j].Number
				})
			default:
				sort.Slice(issues, func(i, j int) bool { return issues[i].Seq < issues[j].Seq })
			}
			if after := cursorSeq(args["after"]); after >= 0 {
				for i, issue := range issues {
					if issue.Seq == after {
						issues = issues[i+1:]
						break
					}
				}
			}
			first := len(issues)
			if f, ok := args["first"].(float64); ok {
				first = int(f)
				if first < 0 {
					first = 0
				}
			}
			if first > len(issues) {
				first = len(issues)
			}
			page := issues[:first]
			return wrap("IssueConnection", issueConnParent{repo, page, len(issues) > len(page)})
		},
		"pullRequests": func(parent any, args map[string]any, _ *execCtx) any {
			repo := parent.(*Repo)
			var pulls []*PullRequest
			for _, pull := range repo.Pulls {
				if pull.State == "open" {
					pulls = append(pulls, pull)
				}
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].Seq > pulls[j].Seq })
			first := len(pulls)
			if f, ok := args["first"].(float64); ok {
				first = int(f)
				if first < 0 {
					first = 0
				}
			}
			if first > len(pulls) {
				first = len(pulls)
			}
			return wrap("PullRequestConnection", pullConnParent{repo, pulls[:first]})
		},
		"pullRequest": func(parent any, args map[string]any, _ *execCtx) any {
			repo := parent.(*Repo)
			num, ok := numArg(args, "number")
			if !ok {
				return nil
			}
			pull := repo.Pulls[int(num)]
			if pull == nil {
				return nil
			}
			return wrap("PullRequest", pullParent{repo, pull})
		},
	},
	"Ref": {
		"name": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(refParent).name },
	},
	"Issue": {
		"id": func(parent any, _ map[string]any, _ *execCtx) any {
			p := parent.(issueParent)
			return IssueGlobalID(p.repo, p.issue)
		},
		"number": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(issueParent).issue.Number },
		"title":  func(parent any, _ map[string]any, _ *execCtx) any { return parent.(issueParent).issue.Title },
		"state":  func(parent any, _ map[string]any, _ *execCtx) any { return issueStateName(parent.(issueParent).issue) },
		"body":   func(parent any, _ map[string]any, _ *execCtx) any { return parent.(issueParent).issue.Body },
	},
	"IssueConnection": {
		"nodes": func(parent any, _ map[string]any, _ *execCtx) any {
			p := parent.(issueConnParent)
			out := make([]any, 0, len(p.issues))
			for _, issue := range p.issues {
				out = append(out, wrap("Issue", issueParent{p.repo, issue}))
			}
			return out
		},
		"pageInfo": func(parent any, _ map[string]any, _ *execCtx) any { return wrap("PageInfo", parent) },
	},
	"PageInfo": {
		"hasNextPage": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(issueConnParent).hasNextPage },
		"endCursor": func(parent any, _ map[string]any, _ *execCtx) any {
			issues := parent.(issueConnParent).issues
			if len(issues) == 0 {
				return nil
			}
			return makeCursor(issues[len(issues)-1].Seq)
		},
		"hasPreviousPage": func(_ any, _ map[string]any, _ *execCtx) any { return false },
		"startCursor": func(parent any, _ map[string]any, _ *execCtx) any {
			issues := parent.(issueConnParent).issues
			if len(issues) == 0 {
				return nil
			}
			return makeCursor(issues[0].Seq)
		},
	},
	"PullRequest": {
		"id": func(parent any, _ map[string]any, _ *execCtx) any {
			p := parent.(pullParent)
			return PullGlobalID(p.repo, p.pull)
		},
		"number":      func(parent any, _ map[string]any, _ *execCtx) any { return parent.(pullParent).pull.Number },
		"title":       func(parent any, _ map[string]any, _ *execCtx) any { return parent.(pullParent).pull.Title },
		"state":       func(parent any, _ map[string]any, _ *execCtx) any { return pullStateName(parent.(pullParent).pull) },
		"merged":      func(parent any, _ map[string]any, _ *execCtx) any { return parent.(pullParent).pull.Merged },
		"headRefName": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(pullParent).pull.HeadRef },
		"baseRefName": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(pullParent).pull.BaseRef },
		"mergeable": func(parent any, _ map[string]any, _ *execCtx) any {
			p := parent.(pullParent)
			if p.pull.State != "open" {
				return "UNKNOWN"
			}
			if PullConflicts(p.repo, p.pull) {
				return "CONFLICTING"
			}
			return "MERGEABLE"
		},
		"files": func(parent any, args map[string]any, _ *execCtx) any {
			p := parent.(pullParent)
			all := PullFiles(p.repo, p.pull)
			paths := make([]string, 0, len(all))
			for _, f := range all {
				paths = append(paths, f.Filename)
			}
			first := len(paths)
			if f, ok := args["first"].(float64); ok {
				first = int(f)
				if first < 0 {
					first = 0
				}
			}
			if first > len(paths) {
				first = len(paths)
			}
			return wrap("PullRequestChangedFileConnection", fileConnParent{paths[:first]})
		},
	},
	"PullRequestConnection": {
		"nodes": func(parent any, _ map[string]any, _ *execCtx) any {
			p := parent.(pullConnParent)
			out := make([]any, 0, len(p.pulls))
			for _, pull := range p.pulls {
				out = append(out, wrap("PullRequest", pullParent{p.repo, pull}))
			}
			return out
		},
		"pageInfo": func(_ any, _ map[string]any, _ *execCtx) any {
			return wrap("PageInfo", issueConnParent{issues: nil, hasNextPage: false})
		},
	},
	"PullRequestChangedFileConnection": {
		"nodes": func(parent any, _ map[string]any, _ *execCtx) any {
			p := parent.(fileConnParent)
			out := make([]any, 0, len(p.files))
			for _, path := range p.files {
				out = append(out, wrap("PullRequestChangedFile", namedParent{path}))
			}
			return out
		},
		"pageInfo": func(_ any, _ map[string]any, _ *execCtx) any {
			return wrap("PageInfo", issueConnParent{issues: nil, hasNextPage: false})
		},
	},
	"PullRequestChangedFile": {
		"path": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(namedParent).name },
	},
	"CreateIssuePayload": {
		"issue": func(parent any, _ map[string]any, _ *execCtx) any { return wrap("Issue", parent) },
	},
	"CloseIssuePayload": {
		"issue": func(parent any, _ map[string]any, _ *execCtx) any { return wrap("Issue", parent) },
	},
	"CreatePullRequestPayload": {
		"pullRequest": func(parent any, _ map[string]any, _ *execCtx) any { return wrap("PullRequest", parent) },
	},
	"MergePullRequestPayload": {
		"pullRequest": func(parent any, _ map[string]any, _ *execCtx) any { return wrap("PullRequest", parent) },
	},
	"AddCommentPayload": {
		"commentEdge": func(parent any, _ map[string]any, _ *execCtx) any { return wrap("CommentEdge", parent) },
	},
	"CommentEdge": {
		"node": func(parent any, _ map[string]any, _ *execCtx) any { return wrap("Comment", parent) },
	},
	"Comment": {
		"id": func(parent any, _ map[string]any, _ *execCtx) any {
			p := parent.(commentParent)
			return CommentGlobalID(p.repo, p.comment)
		},
		"body": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(commentParent).comment.Body },
	},
	"__Type": {
		"name": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(string) },
		"kind": func(parent any, _ map[string]any, _ *execCtx) any {
			if _, ok := typeEnumValues[parent.(string)]; ok {
				return "ENUM"
			}
			return "OBJECT"
		},
		"fields": func(parent any, _ map[string]any, _ *execCtx) any {
			fields, ok := typeFields[parent.(string)]
			if !ok {
				return nil
			}
			out := make([]any, 0, len(fields))
			for _, name := range fields {
				out = append(out, wrap("__Field", namedParent{name}))
			}
			return out
		},
		"enumValues": func(parent any, _ map[string]any, _ *execCtx) any {
			values, ok := typeEnumValues[parent.(string)]
			if !ok {
				return nil
			}
			out := make([]any, 0, len(values))
			for _, name := range values {
				out = append(out, wrap("__EnumValue", namedParent{name}))
			}
			return out
		},
	},
	"__Field": {
		"name": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(namedParent).name },
	},
	"__EnumValue": {
		"name": func(parent any, _ map[string]any, _ *execCtx) any { return parent.(namedParent).name },
	},
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

func runSelections(typeName string, value any, selections []*selection, vars variables, ctx *execCtx) *Obj {
	out := NewObj()
	for _, selection := range selections {
		if selection.isFragment {
			if selection.typeCondition != "" && selection.typeCondition != typeName {
				continue
			}
			fragment := runSelections(typeName, value, selection.selections, vars, ctx)
			for _, key := range fragment.keys {
				out.Set(key, fragment.vals[key])
			}
			continue
		}
		resolvers := fieldResolvers[typeName]
		resolver := resolvers[selection.name]
		if resolver == nil {
			failGql("Cannot query field %q on type %q.", selection.name, typeName)
		}
		args := evalArgs(selection, vars)
		resolved := resolver(value, args, ctx)
		out.Set(selection.alias, finishValue(resolved, selection.selections, vars, ctx))
	}
	return out
}

func finishValue(value any, selections []*selection, vars variables, ctx *execCtx) any {
	if value == nil {
		return nil
	}
	if list, ok := value.([]any); ok {
		out := make([]any, 0, len(list))
		for _, entry := range list {
			out = append(out, finishValue(entry, selections, vars, ctx))
		}
		return out
	}
	if typed, ok := value.(typedValue); ok {
		if len(selections) == 0 {
			return nil
		}
		return runSelections(typed.t, typed.v, selections, vars, ctx)
	}
	return value
}

type gqlErrorJSON struct {
	Message string `json:"message"`
}

type gqlResult struct {
	Data   any            `json:"data"`
	Errors []gqlErrorJSON `json:"errors,omitempty"`
}

func gqlFailure(message string) gqlResult {
	return gqlResult{Data: nil, Errors: []gqlErrorJSON{{Message: message}}}
}

// executeOperation runs one operation document against the forge.
func executeOperation(forge *Forge, input map[string]any) (result gqlResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(*gqlError); ok {
				result = gqlFailure(err.msg)
				return
			}
			if err, ok := recovered.(error); ok {
				result = gqlFailure(err.Error())
				return
			}
			result = gqlFailure(fmt.Sprint(recovered))
		}
	}()
	ctx := &execCtx{forge: forge}
	query, ok := input["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return gqlFailure("query is required")
	}
	vars := variables{}
	if m, ok := input["variables"].(map[string]any); ok {
		vars = m
	}
	parser := &docParser{tokens: tokenize(query)}
	operations := parser.readDocument()
	if len(operations) == 0 {
		return gqlFailure("the document holds no operation")
	}
	var operation *operation
	if name, ok := input["operationName"].(string); ok && name != "" {
		for _, candidate := range operations {
			if candidate.name == name {
				operation = candidate
				break
			}
		}
		if operation == nil {
			quoted, _ := json.Marshal(name)
			return gqlFailure(fmt.Sprintf("unknown operation %s", string(quoted)))
		}
	} else if len(operations) == 1 {
		operation = operations[0]
	} else {
		return gqlFailure("the document holds several operations and no operationName was given")
	}
	merged := variables{}
	for k, v := range vars {
		merged[k] = v
	}
	for _, variable := range operation.variables {
		if _, present := merged[variable.name]; !present && variable.defaultValue != nil {
			merged[variable.name] = evalValue(variable.defaultValue, variables{})
		}
	}
	rootType := "Query"
	if operation.kind == "mutation" {
		rootType = "Mutation"
	}
	data := runSelections(rootType, nil, operation.selections, merged, ctx)
	return gqlResult{Data: data}
}

// HandleGraphql handles one POST /api/graphql request: a single operation
// document, or a batched array.
func HandleGraphql(forge *Forge, req *Req) Res {
	if req.JSONParseError {
		return errRes(400, "Malformed JSON body")
	}
	body := req.JSON
	if list, ok := body.([]any); ok {
		results := make([]gqlResult, 0, len(list))
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				results = append(results, gqlFailure("each batch item must be an operation document"))
				continue
			}
			results = append(results, executeOperation(forge, m))
		}
		return jsonRes(200, results)
	}
	m, ok := body.(map[string]any)
	if !ok {
		return errRes(400, "query is required")
	}
	return jsonRes(200, executeOperation(forge, m))
}
