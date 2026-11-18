// Shared HTTP plumbing for the forge's surfaces: the request and response
// shapes the routers pass around, and the JSON helpers that keep every reply
// application/json with a message on errors.
//
// JSON encoding matches the original's byte for byte where the wire can see it:
// struct fields marshal in declaration order (the shapes in rest.go declare
// their fields in wire order), HTML escaping is off (the original does not
// escape <, > or &), and GraphQL results keep selection order through Obj.

package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
)

// Req is one parsed request, as the routers see it.
type Req struct {
	Method string
	// Pathname is the decoded path, e.g. /api/v3/repos/adaiversen/amber-thistle.
	Pathname string
	// Segments is the pathname split on '/', without empty segments.
	Segments []string
	Query    url.Values
	Headers  map[string][]string
	// Body is the raw request body.
	Body []byte
	// JSON is the parsed JSON body, when the body parsed as JSON.
	JSON any
	// JSONParseError is true when a body was present but did not parse as JSON.
	JSONParseError bool
	// Host is the request's Host header, for absolute pagination URLs.
	Host string
}

// Res is one router answer.
type Res struct {
	Status      int
	Body        []byte
	ContentType string
	Headers     map[string]string
}

var requestSeq atomic.Int64

// newRequestID is a request id: deterministic, non-empty, unique per request in
// this process.
func newRequestID() string {
	return "forge-" + strconv.FormatInt(requestSeq.Add(1), 36)
}

// marshalJSON renders a value the way the original's JSON.stringify does: no
// HTML escaping, shortest-round-trip numbers.
func marshalJSON(value any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoder.Encode appends a newline; strip it.
	if err := enc.Encode(value); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// jsonRes is a JSON reply.
func jsonRes(status int, value any) Res {
	return Res{Status: status, Body: marshalJSON(value), ContentType: "application/json"}
}

// jsonResHeaders is a JSON reply with extra headers.
func jsonResHeaders(status int, value any, headers map[string]string) Res {
	return Res{Status: status, Body: marshalJSON(value), ContentType: "application/json", Headers: headers}
}

// errRes is a JSON error reply: {message}.
func errRes(status int, message string) Res {
	return jsonRes(status, errBody{Message: message})
}

type errBody struct {
	Message string `json:"message"`
}

// noContentRes is a 204 with no body.
func noContentRes() Res { return Res{Status: 204} }

// bodyObject is the parsed JSON body as a record, or nil when it is not one.
func bodyObject(req *Req) map[string]any {
	if m, ok := req.JSON.(map[string]any); ok {
		return m
	}
	return nil
}

// headerHas reports whether a header value contains a needle.
func headerHas(req *Req, name, needle string) bool {
	for _, value := range req.Headers[strings.ToLower(name)] {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

// queryInt is a query integer with a default, clamped to a range. It parses a
// leading integer the way the original's parseInt does: "3abc" is 3, "abc" is
// no number at all.
func queryInt(query url.Values, name string, fallback, min, max int) int {
	raw := query.Get(name)
	if raw == "" {
		return fallback
	}
	value, ok := parseLeadingInt(raw)
	if !ok {
		return fallback
	}
	if value < min {
		value = min
	}
	if value > max {
		value = max
	}
	return value
}

func parseLeadingInt(s string) (int, bool) {
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	if i >= len(s) || s[i] < '0' || s[i] > '9' {
		return 0, false
	}
	n := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	if neg {
		n = -n
	}
	return n, true
}

// paginate slices a list into one page and builds the Link header value (never
// empty: first and last are always present).
func paginate[T any](items []T, req *Req, path string) (page []T, link string) {
	perPage := queryInt(req.Query, "per_page", 30, 1, 100)
	pageNum := queryInt(req.Query, "page", 1, 1, 1000000)
	start := (pageNum - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	page = items[start:end]
	totalPages := (len(items) + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	urlFor := func(p int) string {
		params := url.Values{}
		for k, vs := range req.Query {
			for _, v := range vs {
				params.Add(k, v)
			}
		}
		params.Set("page", strconv.Itoa(p))
		params.Set("per_page", strconv.Itoa(perPage))
		return "http://" + req.Host + path + "?" + params.Encode()
	}
	var links []string
	if pageNum < totalPages {
		links = append(links, "<"+urlFor(pageNum+1)+">; rel=\"next\"")
	}
	if pageNum > 1 {
		links = append(links, "<"+urlFor(pageNum-1)+">; rel=\"prev\"")
	}
	links = append(links, "<"+urlFor(1)+">; rel=\"first\"")
	links = append(links, "<"+urlFor(totalPages)+">; rel=\"last\"")
	return page, strings.Join(links, ", ")
}

// ---------------------------------------------------------------------------
// Obj: an insertion-ordered JSON object, for GraphQL results.
// ---------------------------------------------------------------------------

// Obj is a JSON object whose keys marshal in insertion order (the order the
// query selected them), like the original's plain objects.
type Obj struct {
	keys []string
	vals map[string]any
}

// NewObj builds an empty ordered object.
func NewObj() *Obj {
	return &Obj{vals: map[string]any{}}
}

// Set assigns a key, appending it to the order on first use.
func (o *Obj) Set(key string, value any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
}

// MarshalJSON renders the object with its keys in insertion order.
func (o *Obj) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb := marshalJSON(o.vals[key])
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
