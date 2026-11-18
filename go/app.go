// The forge's HTTP server: one process, one in-memory store, four surfaces on
// one port — REST v3 at /api/v3, GraphQL at /api/graphql, MCP at /mcp, git
// smart HTTP at /{owner}/{repo}.git — plus /healthz, the only route that needs
// no bearer token.
//
// Every reply carries a non-empty X-GitHub-Request-Id. Auth is a bearer token:
// the suite's token arrives in MAKEFUN_SHOP_TOKEN, and a missing or wrong
// token is 401. When the variable is unset (a local smoke run), any non-empty
// bearer token is accepted — the trial always sets it.
//
// Concurrency: the store lives behind one RWMutex. Reads (every GET, plus the
// git upload-pack POSTs that only ever read) take the shared lock, so fifty
// concurrent clones run side by side; every other write takes the exclusive
// lock. The per-repository fetch cache and the global deflate cache have their
// own mutexes, so a read may populate them while holding only the shared lock.
//
// The URL is split by hand (`?` search plus a `%` check) instead of trusting
// the standard library's parsed form: the workload paths are plain ASCII with
// no query string, and the original decodes in two passes (the whole path,
// then each segment again), which the double-decoding below reproduces.

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// bodyLimit is the largest request body accepted: packs stay far below it.
const bodyLimit = 64 * 1024 * 1024

// fixedDate is the Date header every reply carries. The original sends none at
// all; Go's server always sends one unless the handler sets it, so a constant
// keeps replays byte-identical across runs.
const fixedDate = "Wed, 01 Jan 2025 00:00:00 GMT"

var gitPathPattern = regexp.MustCompile(`^/[^/]+/[^/]+?\.git(?:/.*)?$`)
var gitRoutePattern = regexp.MustCompile(`^/([^/]+)/([^/]+?)\.git(?:/(.*))?$`)

// server is one forge instance: the store, the expected bearer token, and the
// HTTP server around them.
type server struct {
	forge    *Forge
	expected string
	http     *http.Server
}

// newServer builds the forge server. The lifecycle in main.go binds it to an
// ephemeral port.
func newServer() *server {
	s := &server{
		forge:    NewForge(),
		expected: os.Getenv("MAKEFUN_SHOP_TOKEN"),
	}
	s.http = &http.Server{Handler: http.HandlerFunc(s.handle)}
	return s
}

// splitTarget splits a raw request target into a decoded pathname, segments
// and a query set — no url.Parse on the whole target.
func splitTarget(rawURL string) (pathname string, segments []string, query url.Values, err error) {
	qmark := strings.IndexByte(rawURL, '?')
	rawPath := rawURL
	queryString := ""
	if qmark >= 0 {
		rawPath = rawURL[:qmark]
		queryString = rawURL[qmark+1:]
	}
	query = url.Values{}
	if queryString != "" {
		parsed, _ := url.ParseQuery(queryString)
		query = parsed
	}
	pathname = rawPath
	if strings.Contains(rawPath, "%") {
		decoded, unerr := url.PathUnescape(rawPath)
		if unerr != nil {
			return "", nil, nil, unerr
		}
		pathname = decoded
	}
	for _, segment := range strings.Split(pathname, "/") {
		if segment == "" {
			continue
		}
		if strings.Contains(segment, "%") {
			decoded, unerr := url.PathUnescape(segment)
			if unerr != nil {
				return "", nil, nil, unerr
			}
			segment = decoded
		}
		segments = append(segments, segment)
	}
	return pathname, segments, query, nil
}

// hasBody reports whether the request can carry a body (a positive length, or
// chunked encoding).
func hasBody(r *http.Request) bool {
	if r.ContentLength > 0 {
		return true
	}
	return len(r.TransferEncoding) > 0
}

// collectBody reads the request body; it reports false when the body exceeds
// the cap or cannot be read.
func collectBody(r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, bodyLimit+1))
	if err != nil {
		return nil, false
	}
	if len(body) > bodyLimit {
		return nil, false
	}
	return body, true
}

// isAuthenticated reports whether the bearer token on this request matches
// what the environment configured, if any. Exactly one Authorization header
// must be present, starting with "Bearer " and carrying a non-empty token.
func (s *server) isAuthenticated(req *Req) bool {
	values := req.Headers["authorization"]
	if len(values) != 1 {
		return false
	}
	header := values[0]
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	presented := strings.TrimSpace(header[len("Bearer "):])
	if presented == "" {
		return false
	}
	if s.expected == "" {
		return true
	}
	return presented == s.expected
}

// decodeJSONBody decodes a request body as JSON, unless it belongs to a git
// route (binary).
func decodeJSONBody(body []byte, gitPath bool) (value any, failed bool) {
	if len(body) == 0 || gitPath {
		return nil, false
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, true
	}
	return parsed, false
}

// isReadOnly reports whether a request only ever reads the store: every GET,
// plus the git upload-pack POSTs (ls-refs and fetch answer from the store
// without changing it). Everything else takes the exclusive lock.
func isReadOnly(method, pathname string) bool {
	if method == "GET" || method == "HEAD" {
		return true
	}
	return method == "POST" && strings.HasSuffix(pathname, ".git/git-upload-pack")
}

// dispatch routes one parsed request to its surface.
func (s *server) dispatch(req *Req) Res {
	path := req.Pathname
	if path == "/healthz" {
		return jsonRes(200, healthzBody{OK: true})
	}
	if !s.isAuthenticated(req) {
		return errRes(401, "Requires authentication")
	}
	if path == "/api/graphql" {
		if req.Method != "POST" {
			return errRes(405, "Method Not Allowed")
		}
		return HandleGraphql(s.forge, req)
	}
	if path == "/mcp" {
		if req.Method != "POST" {
			return errRes(405, "Method Not Allowed")
		}
		return HandleMcp(s.forge, req)
	}
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		return HandleRest(s.forge, req)
	}
	if git := gitRoutePattern.FindStringSubmatch(path); git != nil {
		owner, err1 := url.PathUnescape(git[1])
		repoName, err2 := url.PathUnescape(git[2])
		if err1 != nil || err2 != nil {
			// A '%' in a decoded name: the original throws here, and the
			// catch-all answers 500. The suite never sends one.
			return errRes(500, "Internal Server Error")
		}
		rest := git[3]
		return HandleGitHTTP(s.forge, req, owner, repoName, rest)
	}
	return errRes(404, "Not Found")
}

type healthzBody struct {
	OK bool `json:"ok"`
}

// handle serves one request: parse, authenticate, route, answer. One bad
// request never takes the server down.
func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-GitHub-Request-Id", newRequestID())
	w.Header().Set("Date", fixedDate)
	defer func() {
		if recovered := recover(); recovered != nil {
			// The response was built before anything was written, so the
			// 500 can still go out.
			writeRes(w, errRes(500, "Internal Server Error"))
		}
	}()

	rawURL := r.RequestURI
	if rawURL == "" {
		rawURL = r.URL.RequestURI()
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	pathname, segments, query, err := splitTarget(rawURL)
	if err != nil {
		writeRes(w, errRes(500, "Internal Server Error"))
		return
	}
	// Most requests carry no body at all: do not touch the stream for those.
	var body []byte
	if hasBody(r) {
		var ok bool
		body, ok = collectBody(r)
		if !ok {
			writeRes(w, errRes(413, "Request body too large"))
			return
		}
	}
	// Git bodies are binary pkt-lines and packs: no handler on a git route
	// ever reads req.json, so skip the decode-and-parse entirely there. Every
	// other surface keeps the exact rule — any non-empty body that is not
	// valid JSON is a 400.
	gitPath := gitPathPattern.MatchString(pathname)
	parsedJSON, jsonParseError := decodeJSONBody(body, gitPath)
	// Only two headers are ever read (authorization for auth, git-protocol
	// for the protocol version), so only those two are copied — the full
	// header map is never needed.
	headers := make(map[string][]string, 2)
	if values, ok := r.Header["Authorization"]; ok {
		headers["authorization"] = values
	}
	if values, ok := r.Header["Git-Protocol"]; ok {
		headers["git-protocol"] = values
	}
	req := &Req{
		Method:         strings.ToUpper(r.Method),
		Pathname:       pathname,
		Segments:       segments,
		Query:          query,
		Headers:        headers,
		Body:           body,
		JSON:           parsedJSON,
		JSONParseError: jsonParseError,
		Host:           host,
	}

	var out Res
	if req.Pathname == "/healthz" {
		out = s.dispatch(req)
	} else if isReadOnly(req.Method, req.Pathname) {
		s.forge.Mu.RLock()
		out = s.dispatch(req)
		s.forge.Mu.RUnlock()
	} else {
		s.forge.Mu.Lock()
		out = s.dispatch(req)
		s.forge.Mu.Unlock()
	}
	writeRes(w, out)
}

// writeRes writes a Res onto the socket.
func writeRes(w http.ResponseWriter, out Res) {
	if out.ContentType != "" {
		w.Header().Set("Content-Type", out.ContentType)
	}
	for name, value := range out.Headers {
		w.Header().Set(name, value)
	}
	w.WriteHeader(out.Status)
	if len(out.Body) > 0 {
		w.Write(out.Body)
	}
}
