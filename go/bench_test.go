package main

import (
	"encoding/json"
	"testing"
)

// Micro-benchmarks for the hot JSON paths: how much does reflection-based
// marshaling cost per issue, and how much would a hand-rolled builder save?
func BenchmarkMarshalIssueJSON(b *testing.B) {
	forge := NewForge()
	repo := forge.CreateRepo("bench-repo", "", false)
	created := CreateIssue(forge, repo, "Benchmark issue title", "Benchmark body text for the issue.", nil)
	issue := IssueJSON(forge, repo, created.Issue)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = marshalJSON(issue)
	}
}

func BenchmarkUnmarshalCreateIssue(b *testing.B) {
	body := []byte(`{"title":"Benchmark issue title","body":"Benchmark body text."}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var parsed any
		_ = json.Unmarshal(body, &parsed)
	}
}

func BenchmarkHandleGetIssue(b *testing.B) {
	forge := NewForge()
	repo := forge.CreateRepo("bench-repo", "", false)
	created := CreateIssue(forge, repo, "Benchmark issue title", "Benchmark body text.", nil)
	_ = created
	req := &Req{
		Method:   "GET",
		Pathname: "/api/v3/repos/adaiversen/bench-repo/issues/1",
		Segments: []string{"api", "v3", "repos", "adaiversen", "bench-repo", "issues", "1"},
		Query:    map[string][]string{},
		Headers:  map[string][]string{},
		Host:     "127.0.0.1",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = HandleRest(forge, req)
	}
}
