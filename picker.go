package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

var errCanceled = errors.New("selection canceled")

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func socketClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}
func client(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing client command")
	}
	command := args[0]
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	q := fs.String("query", "", "")
	all := fs.Bool("all", false, "")
	source := fs.String("source", "", "")
	id := fs.String("id", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	values := url.Values{"q": {*q}, "source": {*source}, "id": {*id}}
	if *all {
		values.Set("all", "1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://ass/"+command+"?"+values.Encode(), nil)
	if err != nil {
		return err
	}
	c := socketClient("api.sock")
	defer c.CloseIdleConnections()
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("index query failed (%s)", resp.Status)
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

func (s *store) pick(parent context.Context, all bool, w io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.cfg.Cache, "run-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// Relative socket paths avoid sockaddr_un's 104-byte macOS limit, even
	// when XDG_CACHE_HOME is a deeply nested directory. Only this child chdirs.
	previous, err := os.Getwd()
	if err != nil {
		return err
	}
	if err = os.Chdir(dir); err != nil {
		return err
	}
	defer os.Chdir(previous)
	l, err := net.Listen("unix", "api.sock")
	if err != nil {
		return err
	}
	defer l.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if err := s.search(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("all") == "1", w); err != nil && r.Context().Err() == nil {
			fmt.Fprintln(os.Stderr, "ass: search:", err)
		}
	})
	mux.HandleFunc("/preview", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		if err := s.preview(r.Context(), v.Get("source"), v.Get("id"), v.Get("q"), v.Get("all") == "1", w); err != nil && r.Context().Err() == nil {
			fmt.Fprintln(w, "Preview unavailable:", err)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go server.Serve(l)
	defer server.Close()
	queryCmd := shellQuote(exe) + fmt.Sprintf(" client search --all=%t --query {q}", all)
	previewCmd := shellQuote(exe) + fmt.Sprintf(" client preview --all=%t --source {3} --id {2} --query {q}", all)
	// Do not combine --track/--id-nth with dynamic reload here: fzf 0.74.4
	// loses burst-typed characters with that combination (covered by PTY test).
	args := []string{"--ansi", "--exact", "--disabled", "--sync", "--no-sort", "--no-hscroll", "--delimiter=\t", "--with-nth=5,6,7", "--nth=2,3", "--no-multi", "--listen=ui.sock", "--with-shell=/bin/sh -c", "--header=Updating index…", "--bind=change:reload:sleep 0.08; " + queryCmd, "--preview=" + previewCmd, "--preview-window=right:60%:wrap"}
	cmd := exec.CommandContext(ctx, "fzf", args...)
	cmd.Stderr = os.Stderr
	key := make([]byte, 24)
	if _, err = rand.Read(key); err != nil {
		return err
	}
	apiKey := envOr("FZF_API_KEY", hex.EncodeToString(key))
	cmd.Env = append(filterEnvironment(), "FZF_API_KEY="+apiKey)
	var initial strings.Builder
	if err = s.search(ctx, "", all, &initial); err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(initial.String())
	var result strings.Builder
	cmd.Stdout = &result
	if err = cmd.Start(); err != nil {
		return err
	}
	updaterDone := make(chan struct{})
	var updateErr error
	go func() {
		defer close(updaterDone)
		// One background refresh per invocation, like the old picker's snapshot.
		// Repeated refreshes while a live agent writes can continuously cancel
		// searches and reset selection; do not poll or install a daemon.
		_, e := s.update(ctx)
		if ctx.Err() != nil {
			return
		}
		updateErr = e
		header := ""
		if e != nil {
			header = "Index partially updated; details on exit"
		}
		action := "change-header(" + header + ")+reload(" + queryCmd + ")"
		notifyPicker(ctx, apiKey, action)
	}()
	err = cmd.Wait()
	cancel()
	<-updaterDone
	if updateErr != nil {
		fmt.Fprintln(os.Stderr, "ass: index:", updateErr)
	}
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok && (e.ExitCode() == 1 || e.ExitCode() == 130) {
			return errCanceled
		}
		return err
	}
	selected := strings.TrimSuffix(result.String(), "\n")
	if selected == "" {
		return errCanceled
	}
	fields := strings.Split(selected, "\t")
	if len(fields) != 7 || fields[0] == "" || strings.ContainsAny(fields[0], "\x00\r\n") || !uuidRE.MatchString(fields[1]) {
		return fmt.Errorf("invalid selection returned by fzf")
	}
	_, err = fmt.Fprintln(w, selected)
	return err
}
func notifyPicker(ctx context.Context, key, action string) {
	c := socketClient("ui.sock")
	defer c.CloseIdleConnections()
	for attempt := 0; attempt < 30; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fzf/", strings.NewReader(action))
		if err != nil {
			return
		}
		req.Header.Set("X-API-Key", key)
		resp, err := c.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}
