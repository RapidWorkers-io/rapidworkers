// Command rapidworkers-mcp is a stdio MCP server that relays every request to
// the hosted RapidWorkers MCP server.
//
// MCP clients that launch local servers (Claude Desktop, Cursor, Windsurf,
// Zed, Cline...) talk to this process over stdin/stdout. Each JSON-RPC
// request is sent unchanged to https://www.rapidworkers.io/api/mcp/ with the
// API key, and the answer is written back, so the tools, instructions,
// prompts and resources are always exactly the live ones.
//
// Without RAPIDWORKERS_API_KEY the server still starts and lists the tools
// from a bundled snapshot (so registries and inspectors can see them); every
// tool call then explains how to create a key.
//
// Install:
//
//	go install github.com/RapidWorkers-io/rapidworkers/go/cmd/rapidworkers-mcp@latest
//
// Environment: RAPIDWORKERS_API_KEY (required for tool calls),
// RAPIDWORKERS_MCP_URL (optional endpoint override).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	rapidworkers "github.com/RapidWorkers-io/rapidworkers/go"
)

// specJSON is spec/tools.json, copied here by scripts/codegen.py.
//
//go:embed tools.json
var specJSON []byte

const help = `rapidworkers-mcp: stdio MCP server that relays every request to the hosted
RapidWorkers MCP server (https://www.rapidworkers.io/api/mcp/).

MCP clients that launch local servers (Claude Desktop, Cursor, Windsurf, Zed,
Cline...) talk to this process over stdin/stdout. Each JSON-RPC request is sent
unchanged with the API key, and the answer is written back, so the tools,
instructions, prompts and resources are always exactly the live ones.

Without RAPIDWORKERS_API_KEY the server still starts and lists the tools from a
bundled snapshot; every tool call then explains how to create a key.

Usage: rapidworkers-mcp [-version] [-help]

Environment: RAPIDWORKERS_API_KEY (required for tool calls), RAPIDWORKERS_MCP_URL
(optional endpoint override).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	flags := flag.NewFlagSet("rapidworkers-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {} // -help prints to stdout below; errors are printed by flag
	var version bool
	flags.BoolVar(&version, "version", false, "print the version and exit")
	flags.BoolVar(&version, "V", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, help)
			return 0
		}
		fmt.Fprintln(stderr, "usage: rapidworkers-mcp [-version] [-help]")
		return 2
	}
	if version {
		fmt.Fprintf(stdout, "rapidworkers-mcp %s\n", rapidworkers.Version)
		return 0
	}

	logger := log.New(stderr, "[rapidworkers-mcp] ", 0)
	spec, err := loadSpec()
	if err != nil {
		logger.Printf("the bundled tools.json is unreadable: %v", err)
		return 1
	}
	apiKey := strings.TrimSpace(getenv(rapidworkers.APIKeyEnv))
	url := strings.TrimSpace(getenv(rapidworkers.BaseURLEnv))
	if url == "" {
		url = spec.Server.URL
	}
	if apiKey == "" {
		logger.Printf("%s is not set: tools are listed but calls will ask for a key (%s).",
			rapidworkers.APIKeyEnv, rapidworkers.APIKeysURL)
	}
	serve(context.Background(), newProxy(apiKey, url, spec, logger), stdin, stdout)
	return 0
}

type spec struct {
	Server struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		URL          string `json:"url"`
		Instructions string `json:"instructions"`
	} `json:"server"`
	Tools json.RawMessage `json:"tools"`
}

func loadSpec() (*spec, error) {
	var s spec
	if err := json.Unmarshal(specJSON, &s); err != nil {
		return nil, err
	}
	if s.Server.URL == "" || len(s.Tools) == 0 {
		return nil, errors.New("server.url or tools missing")
	}
	return &s, nil
}
