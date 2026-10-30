package mcp

// list_todos on an issue-dependency hold (ADR-0055, stump.wtf/switchboard#583): a blocked row names
// the issues it waits on, so a worker sees the hold from the listing instead of discovering it by
// claiming; pending rows carry no blockers field. A worker may still try to claim a held row — the
// store refuses it — but the listing's job is to make that attempt pointless.

import (
	"context"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

func TestListTodosNamesWhatABlockedRowWaitsOn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{EndpointID: defaultTestEndpointID, ID: "td_h", Queue: "reviews",
		Title: "waits on two", State: store.StateBlocked})
	f.putTodo(store.Todo{EndpointID: defaultTestEndpointID, ID: "td_p", Queue: "reviews",
		Title: "ready", State: "pending"})
	f.putBlockers("td_h",
		store.BlockerRef{Owner: "octo", Repo: "repo", Number: 7, State: "open"},
		store.BlockerRef{Owner: "other", Repo: "thing", Number: 3, State: "open",
			HTMLURL: "https://git.test/other/thing/issues/3"})
	cs := session(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	var out struct {
		Todos []struct {
			ID       string `json:"id"`
			State    string `json:"state"`
			Blockers []struct {
				Ref     string `json:"ref"`
				Owner   string `json:"owner"`
				Repo    string `json:"repo"`
				Number  int    `json:"number"`
				State   string `json:"state"`
				HTMLURL string `json:"html_url"`
			} `json:"blockers"`
		} `json:"todos"`
	}
	callOK(t, ctx, cs, "list_todos", map[string]any{}, &out)
	byID := map[string]int{}
	for i, r := range out.Todos {
		byID[r.ID] = i
	}
	if len(out.Todos) != 2 {
		t.Fatalf("list_todos = %+v, want both rows", out.Todos)
	}
	held := out.Todos[byID["td_h"]]
	if held.State != "blocked" {
		t.Fatalf("td_h state = %q, want blocked", held.State)
	}
	if len(held.Blockers) != 2 {
		t.Fatalf("td_h blockers = %+v, want the two recorded refs", held.Blockers)
	}
	if held.Blockers[0].Ref != "octo/repo#7" || held.Blockers[0].State != "open" {
		t.Errorf("blocker[0] = %+v, want octo/repo#7 open", held.Blockers[0])
	}
	if held.Blockers[1].HTMLURL != "https://git.test/other/thing/issues/3" {
		t.Errorf("blocker[1] lost its url: %+v", held.Blockers[1])
	}
	if ready := out.Todos[byID["td_p"]]; ready.Blockers != nil {
		t.Errorf("a pending row carries blockers: %+v", ready.Blockers)
	}

	// A state=blocked filter finds exactly the held row.
	var filtered struct {
		Todos []struct {
			ID string `json:"id"`
		} `json:"todos"`
	}
	callOK(t, ctx, cs, "list_todos", map[string]any{"state": "blocked"}, &filtered)
	if len(filtered.Todos) != 1 || filtered.Todos[0].ID != "td_h" {
		t.Fatalf("state=blocked listing = %+v, want exactly td_h", filtered.Todos)
	}
}
