package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// folderRow is a GET /folders entry.
type folderRow struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	ParentID string  `json:"parentId"`
	Position float64 `json:"position"`
}

// folderEntry is what scooter folders prints.
type folderEntry struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// listFolders returns the caller's folders as slash-joined paths, in sidebar order.
func listFolders(ctx context.Context, c *client) ([]folderEntry, error) {
	var body struct {
		Folders []folderRow `json:"folders"`
	}
	if err := c.call(ctx, "GET", "/folders", nil, &body); err != nil {
		return nil, err
	}
	children := map[string][]folderRow{}
	for _, f := range body.Folders {
		children[f.ParentID] = append(children[f.ParentID], f)
	}
	out := []folderEntry{}
	var walk func(parent, prefix string, depth int)
	walk = func(parent, prefix string, depth int) {
		kids := children[parent]
		sort.SliceStable(kids, func(i, j int) bool { return kids[i].Position < kids[j].Position })
		for _, f := range kids {
			path := prefix + f.Name
			out = append(out, folderEntry{ID: f.ID, Path: path, Kind: f.Kind})
			// The router caps nesting; the bound only guards a malformed cycle.
			if depth < 16 {
				walk(f.ID, path+"/", depth+1)
			}
		}
	}
	walk("", "", 0)
	return out, nil
}

// resolveFolder finds ref by id, then by path, then by path ignoring case.
func resolveFolder(folders []folderEntry, ref string) (string, error) {
	ref = strings.Trim(ref, "/")
	for _, f := range folders {
		if f.ID == ref || f.Path == ref {
			return f.ID, nil
		}
	}
	var hits []folderEntry
	for _, f := range folders {
		if strings.EqualFold(f.Path, ref) {
			hits = append(hits, f)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0].ID, nil
	case 0:
		return "", fmt.Errorf("no folder %q; scooter folders lists them", ref)
	}
	return "", fmt.Errorf("folder %q is ambiguous; pass its id from scooter folders", ref)
}

// findFolder resolves ref against the server's folders.
func findFolder(ctx context.Context, c *client, ref string) (string, error) {
	folders, err := listFolders(ctx, c)
	if err != nil {
		return "", err
	}
	return resolveFolder(folders, ref)
}

// fileConversation puts id in folderID; "" takes it out of any folder.
func fileConversation(ctx context.Context, c *client, id, folderID string) error {
	var folder any
	if folderID != "" {
		folder = folderID
	}
	return c.call(ctx, "PUT", convPath(id, "/folder"), map[string]any{"folderId": folder}, nil)
}
