package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// oneShot is a run-once subcommand that prints JSON.
type oneShot struct {
	usage string
	run   func(args []string)
}

var oneShots map[string]oneShot

// init fills oneShots; a literal would cycle through newFlags.
func init() {
	oneShots = map[string]oneShot{
		"login":   {"[--server NAME] [--url URL] [--cookie | --issuer URL --client-id ID] [--default]", loginMain},
		"servers": {"", serversMain},
		"ls":      {"[--server NAME] [--all] [--active] [--limit N]", lsMain},
		"new":     {"[--server NAME] [--model M] [--title T] [--private] [--project] [--wait] PROMPT|-", newMain},
		"send":    {"[--server NAME] [--wait] ID PROMPT|-", sendMain},
		"wait":    {"[--server NAME] [--timeout 10m] ID", waitMain},
		"show":    {"[--server NAME] [--last N] ID", showMain},
		"cancel":  {"[--server NAME] ID", func(a []string) { postMain("cancel", a) }},
		"suspend": {"[--server NAME] ID", func(a []string) { postMain("suspend", a) }},
		"url":     {"[--server NAME] ID", urlMain},
	}
}

func oneShotUsage() string {
	var b strings.Builder
	for _, name := range []string{"login", "servers", "ls", "new", "send", "wait", "show", "cancel", "suspend", "url"} {
		fmt.Fprintf(&b, "       scooter %s %s\n", name, oneShots[name].usage)
	}
	return b.String()
}

// parseArgs parses flags anywhere, returning the other arguments.
func parseArgs(fs *flag.FlagSet, args []string, want int) []string {
	var rest []string
	for {
		_ = fs.Parse(args) // ExitOnError: it exits rather than returning one.
		left := fs.Args()
		if len(left) == 0 {
			break
		}
		// After "--" everything is an argument.
		if n := len(args) - len(left); n > 0 && args[n-1] == "--" {
			rest = append(rest, left...)
			break
		}
		rest, args = append(rest, left[0]), left[1:]
	}
	if len(rest) < want {
		fs.Usage()
		os.Exit(2)
	}
	return rest
}

func newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: scooter %s %s\n", name, oneShots[name].usage)
		fs.PrintDefaults()
	}
	return fs, fs.String("server", "", "server name or URL (default: $SCOOTER_SERVER, then the configured default)")
}

func serversMain(args []string) {
	fs, _ := newFlags("servers")
	parseArgs(fs, args, 0)
	cfg, err := loadConfig()
	if err != nil {
		fatal(err)
	}
	creds, err := loadCredentials()
	if err != nil {
		fatal(err)
	}
	printJSON(serverList(cfg, creds))
}

// serverList is what scooter servers prints.
func serverList(cfg config, creds map[string]credential) []map[string]any {
	out := []map[string]any{}
	for _, n := range cfg.serverNames() {
		s := cfg.Servers[n]
		c := creds[n]
		row := map[string]any{
			"name": n, "url": s.URL, "auth": s.Auth.Kind, "default": n == cfg.Default,
			"signed_in": s.Auth.Kind == "" || c.Cookie != "" || c.APIKey != "" || c.IDToken != "" || c.RefreshToken != "",
		}
		if s.WebhooksURL != "" {
			row["webhooks_url"] = s.WebhooksURL
		}
		out = append(out, row)
	}
	return out
}

func lsMain(args []string) {
	fs, server := newFlags("ls")
	all := fs.Bool("all", false, "include other people's shared conversations")
	active := fs.Bool("active", false, "only conversations that are running or waiting on the user")
	limit := fs.Int("limit", 20, "at most this many, newest first (0 for all)")
	parseArgs(fs, args, 0)
	ctx, stop := commandContext()
	defer stop()
	out, err := listConversations(ctx, openServer(*server), *all, *active, *limit)
	if err != nil {
		fatal(err)
	}
	printJSON(out)
}

func newMain(args []string) {
	fs, server := newFlags("new")
	model := fs.String("model", "", "model (default: the server's)")
	title := fs.String("title", "", "conversation title (default: the agent picks one)")
	private := fs.Bool("private", false, "make the conversation private to you")
	project := fs.Bool("project", false, "ask the agent to clone the working directory's git remote and branch")
	wait := fs.Bool("wait", false, "wait for the first turn to end, as scooter wait does")
	timeout := fs.Duration("timeout", 10*time.Minute, "with --wait, give up after this long (0 for never)")
	text, err := promptArg(parseArgs(fs, args, 1))
	if err != nil {
		fatal(err)
	}
	ctx, stop := commandContext()
	defer stop()
	c := openServer(*server)
	if *project {
		cwd, _ := os.Getwd()
		if p := projectContext(ctx, cwd); p != "" {
			text = p + "\n\n" + text
		}
	}
	body := map[string]any{}
	if *model != "" {
		body["model"] = *model
	}
	if *title != "" {
		body["title"] = *title
	}
	if *private {
		body["visibility"] = "private"
	}
	var conv struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, "POST", "/conversations", body, &conv); err != nil {
		fatal(err)
	}
	out, err := sendMessage(ctx, c, conv.ID, text, *wait, *timeout)
	if err != nil {
		fatal(fmt.Errorf("created %s, but: %w", conv.ID, err))
	}
	printJSON(out)
}

func sendMain(args []string) {
	fs, server := newFlags("send")
	wait := fs.Bool("wait", false, "wait for the turn to end, as scooter wait does")
	timeout := fs.Duration("timeout", 10*time.Minute, "with --wait, give up after this long (0 for never)")
	rest := parseArgs(fs, args, 2)
	text, err := promptArg(rest[1:])
	if err != nil {
		fatal(err)
	}
	ctx, stop := commandContext()
	defer stop()
	out, err := sendMessage(ctx, openServer(*server), rest[0], text, *wait, *timeout)
	if err != nil {
		fatal(err)
	}
	printJSON(out)
}

func waitMain(args []string) {
	fs, server := newFlags("wait")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up after this long and report state running (0 for never)")
	settle := fs.Duration("settle", 3*time.Second, "how long an idle conversation may take to start a run just asked for")
	rest := parseArgs(fs, args, 1)
	ctx, stop := commandContext()
	defer stop()
	out, err := waitTurn(ctx, openServer(*server), rest[0], *timeout, *settle, -1)
	if err != nil {
		fatal(err)
	}
	printJSON(out)
}

func showMain(args []string) {
	fs, server := newFlags("show")
	last := fs.Int("last", 5, "show this many recent messages (0 for all)")
	rest := parseArgs(fs, args, 1)
	ctx, stop := commandContext()
	defer stop()
	out, err := showConversation(ctx, openServer(*server), rest[0], *last)
	if err != nil {
		fatal(err)
	}
	printJSON(out)
}

// postMain runs a bodiless action: cancel or suspend.
func postMain(action string, args []string) {
	fs, server := newFlags(action)
	rest := parseArgs(fs, args, 1)
	ctx, stop := commandContext()
	defer stop()
	c := openServer(*server)
	if err := c.call(ctx, "POST", convPath(rest[0], "/"+action), nil, nil); err != nil {
		fatal(err)
	}
	printJSON(map[string]any{"id": rest[0], "ok": true, "url": chatURL(c, rest[0])})
}

func urlMain(args []string) {
	fs, server := newFlags("url")
	rest := parseArgs(fs, args, 1)
	fmt.Println(chatURL(openServer(*server), rest[0]))
}
