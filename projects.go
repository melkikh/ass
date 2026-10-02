package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type projectEntry struct{ ID, Path, Name string }

func (c *codexClient) projects() ([]projectEntry, error) {
	var entries []projectEntry
	var cursor *string
	seen := map[string]bool{}
	for {
		var page struct {
			Data []struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Roots []struct {
					Path string `json:"path"`
				} `json:"roots"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := c.call("project/list", object{"limit": 100, "cursor": cursor}, &page); err != nil {
			return nil, err
		}
		for _, p := range page.Data {
			for _, root := range p.Roots {
				if !safeField(p.ID) || !safeField(p.Name) || !safeField(root.Path) {
					continue
				}
				if path := projectDirectory(root.Path); path != "" {
					entries = append(entries, projectEntry{p.ID, path, p.Name})
				}
			}
		}
		cursor = page.NextCursor
		if cursor == nil || *cursor == "" {
			return entries, nil
		}
		if seen[*cursor] || len(seen) >= 10000 {
			return nil, fmt.Errorf("invalid Codex project pagination")
		}
		seen[*cursor] = true
	}
}

func safeField(s string) bool { return s != "" && !strings.ContainsAny(s, "\x00\t\r\n") }

func projectDirectory(path string) string {
	if !safeField(path) {
		return ""
	}
	p, err := absolutePath(path)
	if err != nil || !safeField(p) {
		return ""
	}
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		return ""
	}
	return p
}

func writeProjects(w io.Writer, entries []projectEntry, original, current string) error {
	var rows []projectEntry
	for _, p := range []projectEntry{{"-", original, "Original project"}, {"-", current, "Current directory"}} {
		p.Path = projectDirectory(p.Path)
		if p.Path == "" {
			continue
		}
		for _, entry := range entries {
			if entry.Path == p.Path {
				p.ID = entry.ID
				break
			}
		}
		rows = append(rows, p)
	}
	seen := map[[2]string]bool{}
	for _, p := range append(rows, entries...) {
		key := [2]string{p.ID, p.Path}
		if seen[key] {
			continue
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", p.ID, p.Path, p.Name); err != nil {
			return err
		}
		seen[key] = true
	}
	return nil
}

func launchCommand(ctx context.Context, command string, args []string, w io.Writer) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	source := fs.String("source", "", "")
	file := fs.String("file", "", "")
	root := fs.String("root", "", "")
	cwd := fs.String("cwd", "", "")
	project := fs.String("project", "-", "")
	original := fs.String("original", "", "")
	current := fs.String("current", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments to %s", command)
	}
	if command == "metadata" {
		return sessionMetadata(ctx, *source, *file, w)
	}
	if command == "projects" {
		c, err := startCodex(ctx, "")
		if err != nil {
			return err
		}
		defer c.close()
		entries, err := c.projects()
		if err != nil {
			return err
		}
		return writeProjects(w, entries, *original, *current)
	}
	if *source == "" || *file == "" || *cwd == "" {
		return fmt.Errorf("import needs --source, --file and --cwd")
	}
	if *root == "" {
		cfg, err := settings()
		if err != nil {
			return err
		}
		*root = filepath.Join(cfg.Cache, *source, "import")
	}
	p, err := prepareImport(ctx, *source, *file, *root, *cwd)
	if err != nil {
		return fmt.Errorf("prepare import: %w", err)
	}
	c, err := startCodex(ctx, p.Home)
	if err != nil {
		return err
	}
	defer c.close()
	id, err := c.importSession(p, *source)
	if err != nil {
		return err
	}
	if !uuidRE.MatchString(id) {
		return fmt.Errorf("Codex import returned an invalid thread id")
	}
	if *project != "-" {
		if err = c.call("thread/metadata/update", object{"threadId": id, "projectId": *project}, nil); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(w, id)
	return err
}
