package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/term"

	"github.com/zachahn/squiggly/internal/agent"
	"github.com/zachahn/squiggly/internal/config"
	"github.com/zachahn/squiggly/internal/initcmd"
	"github.com/zachahn/squiggly/internal/mcpserver"
	"github.com/zachahn/squiggly/internal/prose"
	"github.com/zachahn/squiggly/internal/prune"
	"github.com/zachahn/squiggly/internal/session"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, prose.Usage)
		return 2
	}
	switch args[0] {
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "mcp":
		return runMCP(args[1:], stderr)
	case "prune":
		return runPrune(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, prose.Usage)
		return 0
	}
	fmt.Fprintf(stderr, "squiggly: unknown command %q\n\n%s", args[0], prose.Usage)
	return 2
}

func runInit(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "squiggly init: unexpected arguments; all values are collected through interactive prompts\n")
		return 2
	}
	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "squiggly init: %v\n", err)
		return 1
	}
	stdinFD := int(os.Stdin.Fd())
	err = initcmd.Run(context.Background(), initcmd.Options{
		In:         os.Stdin,
		Out:        stdout,
		IsTerminal: term.IsTerminal(stdinFD),
		ReadSecret: func() (string, error) {
			secret, err := term.ReadPassword(stdinFD)
			return string(secret), err
		},
		Path: path,
	})
	if err != nil {
		fmt.Fprintf(stderr, "squiggly init: %v\n", err)
		return 1
	}
	return 0
}

func runMCP(args []string, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "squiggly mcp: unexpected arguments\n")
		return 2
	}
	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "squiggly mcp: %v\n", err)
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "squiggly mcp: %v\nRun `squiggly init` to create a valid config file.\n", err)
		return 1
	}
	dbPath, err := session.DBPath()
	if err != nil {
		fmt.Fprintf(stderr, "squiggly mcp: %v\n", err)
		return 1
	}
	chatModel, err := agent.NewChatModel(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(stderr, "squiggly mcp: create chat model: %v\nRun `squiggly init` to create a valid config file.\n", err)
		return 1
	}
	db, err := session.Open(context.Background(), dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "squiggly mcp: %v\n", err)
		return 1
	}
	defer db.Close()

	store := session.NewStore(db, session.RequestSettings{
		Platform:              cfg.Platform,
		Model:                 cfg.Model,
		MaxSteps:              cfg.MaxSteps,
		RequestTimeoutSeconds: cfg.RequestTimeoutSeconds,
	})
	srv := mcpserver.New(&mcpserver.Handlers{
		Store:        store,
		Runner:       &agent.Runner{Model: chatModel, MaxSteps: cfg.MaxSteps},
		SystemPrompt: cfg.SystemPrompt,
	})
	if err := server.ServeStdio(srv); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "squiggly mcp: %v\n", err)
		return 1
	}
	return 0
}

func runPrune(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("prune", flag.ContinueOnError)
	flags.SetOutput(stderr)
	olderThan := flags.String("older-than", "", prose.OlderThanFlagDescription)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *olderThan == "" || flags.NArg() > 0 {
		fmt.Fprint(stderr, prose.PruneUsage)
		return 2
	}
	duration, err := prune.ParseDuration(*olderThan)
	if err != nil {
		fmt.Fprintf(stderr, "squiggly prune: %v\n", err)
		return 2
	}
	dbPath, err := session.DBPath()
	if err != nil {
		fmt.Fprintf(stderr, "squiggly prune: %v\n", err)
		return 1
	}
	if err := prune.Run(context.Background(), dbPath, duration, time.Now(), stdout); err != nil {
		fmt.Fprintf(stderr, "squiggly prune: %v\n", err)
		return 1
	}
	return 0
}
