package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"diegoc-agent/internal/config"
	"diegoc-agent/internal/tools"
)

const version = "0.1.0"
const appName = "DiegoC Agent"

func main() {
	workspace := flag.String("workspace", "", "Workspace directory (default: current directory)")
	showVersion := flag.Bool("version", false, "Show version")
	flag.Parse()

	// Ensure MCP connections are cleaned up on exit
	defer tools.CleanupMCPConnections()

	if *showVersion {
		fmt.Printf("%s %s\n", appName, version)
		os.Exit(0)
	}
	if flag.NArg() >= 1 && flag.Arg(0) == "log" {
		var filename string
		if flag.NArg() >= 2 {
			filename = flag.Arg(1)
		}
		runLogSubcommand(filename)
		os.Exit(0)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		if os.IsNotExist(err) || strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "valid API key") {
			printConfigHelp()
		}
		os.Exit(1)
	}

	app, err := buildApplication(cfg, *workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer app.Close()
	runInteractive(app.agent, app.workspaceDir, app.logger)
}
