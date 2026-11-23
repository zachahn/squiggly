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

	"github.com/zachahn/editor/internal/agent"
	"github.com/zachahn/editor/internal/config"
	"github.com/zachahn/editor/internal/initcmd"
	"github.com/zachahn/editor/internal/mcpserver"
	"github.com/zachahn/editor/internal/prune"
	"github.com/zachahn/editor/internal/session"
)

const usage = `Usage:
  editor init                            Interactively create or update the config file.
  editor mcp                             Run the MCP server over stdio.
  editor prune --older-than <duration>   Delete sessions older than a given age.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
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
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "editor: unknown command %q\n\n%s", args[0], usage)
	return 2
}

func runInit(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "editor init: unexpected arguments; all values are collected through interactive prompts\n")
		return 2
	}
	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "editor init: %v\n", err)
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
		fmt.Fprintf(stderr, "editor init: %v\n", err)
		return 1
	}
	return 0
}

func runMCP(args []string, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "editor mcp: unexpected arguments\n")
		return 2
	}
	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "editor mcp: %v\n", err)
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "editor mcp: %v\nRun `editor init` to create a valid config file.\n", err)
		return 1
	}
	root, err := session.Root()
	if err != nil {
		fmt.Fprintf(stderr, "editor mcp: %v\n", err)
		return 1
	}
	chatModel, err := agent.NewChatModel(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(stderr, "editor mcp: create chat model: %v\nRun `editor init` to create a valid config file.\n", err)
		return 1
	}

	srv := mcpserver.New(&mcpserver.Handlers{
		Store:        session.NewStore(root),
		Runner:       &agent.Runner{Model: chatModel, MaxSteps: cfg.MaxSteps},
		SystemPrompt: cfg.SystemPrompt,
	})
	if err := server.ServeStdio(srv); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "editor mcp: %v\n", err)
		return 1
	}
	return 0
}

func runPrune(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("prune", flag.ContinueOnError)
	flags.SetOutput(stderr)
	olderThan := flags.String("older-than", "", "delete sessions older than this duration, for example 30d, 12h, or 90m")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *olderThan == "" || flags.NArg() > 0 {
		fmt.Fprintf(stderr, "Usage: editor prune --older-than <duration>\n")
		return 2
	}
	duration, err := prune.ParseDuration(*olderThan)
	if err != nil {
		fmt.Fprintf(stderr, "editor prune: %v\n", err)
		return 2
	}
	root, err := session.Root()
	if err != nil {
		fmt.Fprintf(stderr, "editor prune: %v\n", err)
		return 1
	}
	if err := prune.Run(root, duration, time.Now(), stdout); err != nil {
		fmt.Fprintf(stderr, "editor prune: %v\n", err)
		return 1
	}
	return 0
}
