package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("orchestrator-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	profilePath := flags.String("profile", "", "explicit JSON project profile (no automatic discovery)")
	task := flags.String("task", "", "exact task identifier for dry-run")
	workspace := flags.String("workspace", "", "explicit workspace; must equal the profile root")
	permission := flags.String("permission", "", "one exact project-defined permission")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "positional arguments are not supported")
		return 2
	}
	var profile *config.Profile
	if *profilePath != "" {
		loaded, err := config.Load(*profilePath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		profile = &loaded
	}
	request := core.Request{TaskID: *task, Workspace: *workspace, Permission: *permission}
	if profile != nil && request.Workspace != "" {
		root, err := config.ResolveWorkspace(request.Workspace)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		request.Workspace = root
	}
	if err := json.NewEncoder(stdout).Encode(core.Plan(profile, request)); err != nil {
		fmt.Fprintln(stderr, "write decision:", err)
		return 1
	}
	return 0
}
