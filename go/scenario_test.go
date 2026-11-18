package main

import (
	"fmt"
	"testing"
)

func TestScenarioUsers(t *testing.T) {
	users := allUsers()
	if len(users) != 12 {
		t.Fatalf("expected 12 users, got %d", len(users))
	}
	if users[0].Login != "adaiversen" {
		t.Fatalf("viewer login = %q, want adaiversen", users[0].Login)
	}
	if users[0].ID != 1000 || users[0].Name != "ada iversen" || users[0].Email != "adaiversen@example.invalid" {
		t.Fatalf("viewer user = %+v", users[0])
	}
	for _, u := range users {
		fmt.Printf("user %d %s %s %s\n", u.ID, u.Login, u.Name, u.Email)
	}
}
