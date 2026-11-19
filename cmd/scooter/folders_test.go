package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

var boardFolders = []map[string]any{
	{"id": "starred", "name": "Starred", "kind": "starred", "position": 0},
	{"id": "s2", "name": "Review", "kind": "user", "parentId": "b", "position": 2},
	{"id": "b", "name": "Board", "kind": "user", "position": 1},
	{"id": "s1", "name": "Design", "kind": "user", "parentId": "b", "position": 1},
	{"id": "x", "name": "design", "kind": "user", "position": 3},
}

func TestFoldersListAsPathsInSidebarOrder(t *testing.T) {
	f, srv := newFake(t)
	f.folders = boardFolders
	got, err := listFolders(context.Background(), newClient(srv.URL, headerSource{}))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range got {
		paths = append(paths, e.Path)
	}
	if want := []string{"Starred", "Board", "Board/Design", "Board/Review", "design"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("got %v, want %v", paths, want)
	}
}

func TestResolveFolderByIDPathOrCase(t *testing.T) {
	folders := []folderEntry{
		{ID: "s1", Path: "Board/Design"}, {ID: "x", Path: "Design"}, {ID: "y", Path: "Other/A"}, {ID: "z", Path: "other/a"},
	}
	for ref, want := range map[string]string{"s1": "s1", "Board/Design": "s1", "/board/design/": "s1", "Design": "x", "other/a": "z"} {
		if got, err := resolveFolder(folders, ref); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", ref, got, err, want)
		}
	}
	if _, err := resolveFolder(folders, "OTHER/A"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := resolveFolder(folders, "Nope"); err == nil {
		t.Error("missing folder resolved")
	}
}

func TestNewFilesBeforeTheFirstMessage(t *testing.T) {
	f, srv := newFake(t)
	f.onMessage = func(*fakeScooter, string) {}
	c := newClient(srv.URL, headerSource{})
	if _, err := startConversation(context.Background(), c, map[string]any{}, "s1", "hi", false, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var writes []string
	for _, l := range f.log {
		if !strings.HasPrefix(l, "GET ") {
			writes = append(writes, l)
		}
	}
	want := []string{"POST /conversations", "PUT /conversations/c1/folder", "POST /conversations/c1/messages"}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("got %v, want %v", writes, want)
	}
}
