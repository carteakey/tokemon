package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/adapters/antigravity"
	"github.com/tokemon/tokemon/internal/adapters/claude"
	"github.com/tokemon/tokemon/internal/adapters/codex"
	"github.com/tokemon/tokemon/internal/adapters/copilot"
	"github.com/tokemon/tokemon/internal/adapters/generic"
	"github.com/tokemon/tokemon/internal/adapters/opencode"
	localagent "github.com/tokemon/tokemon/internal/agent"
	"github.com/tokemon/tokemon/internal/api"
	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

const defaultDatabase = "tokemon.db"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tokemon:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("command required; try: tokemon serve, agent, import, export, inspect, discover, catalog, or purge")
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "import":
		return runImport(args[1:])
	case "export":
		return runExport(args[1:])
	case "inspect":
		return runInspect(args[1:])
	case "discover":
		return runDiscover(args[1:])
	case "purge":
		return runPurge(args[1:])
	case "agent":
		return runAgent(args[1:])
	case "catalog":
		return runCatalog(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runCatalog(args []string) error {
	if len(args) == 0 || args[0] != "validate" {
		return errors.New("usage: tokemon catalog validate [--catalog catalog/models.yaml]")
	}
	flags := flag.NewFlagSet("catalog validate", flag.ContinueOnError)
	catalogPath := flags.String("catalog", "catalog/models.yaml", "model catalog YAML path")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: tokemon catalog validate [--catalog catalog/models.yaml]")
	}
	modelCatalog, err := catalog.Load(*catalogPath)
	if err != nil {
		return err
	}
	fmt.Printf("catalog valid: schema %s, %d models, %d resolvable names\n", catalog.SchemaVersion, len(modelCatalog.Models), len(modelCatalog.Aliases))
	return nil
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", envOr("TOKEMON_SERVER_ADDR", ":8080"), "HTTP listen address")
	databasePath := flags.String("database", envOr("TOKEMON_DATABASE", defaultDatabase), "SQLite database path")
	ingestToken := flags.String("ingest-token", os.Getenv("TOKEMON_INGEST_TOKEN"), "shared token for event ingestion")
	catalogPath := flags.String("catalog", envOr("TOKEMON_MODEL_CATALOG", "catalog/models.yaml"), "model catalog YAML path")
	configPath := flags.String("config", envOr("TOKEMON_SERVER_CONFIG", ""), "dotenv config path (defaults to ~/.config/tokemon/server.env)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		*configPath = localagent.DefaultServerConfigPath(home)
	}
	configValues, err := localagent.ReadEnvFile(*configPath)
	if err != nil {
		return err
	}
	applyServerConfig(flags, configValues, addr, databasePath, ingestToken, catalogPath)
	modelCatalog, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	store, err := database.Open(*databasePath, modelCatalog)
	if err != nil {
		return err
	}
	defer store.Close()
	server, err := api.New(store, *ingestToken)
	if err != nil {
		return err
	}
	fmt.Printf("Tokemon listening on http://%s\n", *addr)
	return http.ListenAndServe(*addr, server.Handler())
}

func runImport(args []string) error {
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	databasePath := flags.String("database", defaultDatabase, "SQLite database path")
	catalogPath := flags.String("catalog", "catalog/models.yaml", "model catalog YAML path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: tokemon import [flags] usage.jsonl")
	}
	events, err := readEvents(flags.Arg(0))
	if err != nil {
		return err
	}
	modelCatalog, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	store, err := database.Open(*databasePath, modelCatalog)
	if err != nil {
		return err
	}
	defer store.Close()
	result, err := store.Ingest(context.Background(), events)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func runExport(args []string) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	databasePath := flags.String("database", defaultDatabase, "SQLite database path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: tokemon export [flags] usage.jsonl")
	}
	store, err := database.Open(*databasePath, catalog.Empty())
	if err != nil {
		return err
	}
	defer store.Close()
	events, err := store.Events(context.Background())
	if err != nil {
		return err
	}
	output, closeOutput, err := openOutput(flags.Arg(0))
	if err != nil {
		return err
	}
	defer closeOutput()
	encoder := json.NewEncoder(output)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			return err
		}
	}
	return nil
}

func runInspect(args []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: tokemon inspect usage.jsonl")
	}
	events, err := readEvents(flags.Arg(0))
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			return err
		}
	}
	return nil
}

func runPurge(args []string) error {
	flags := flag.NewFlagSet("purge", flag.ContinueOnError)
	databasePath := flags.String("database", defaultDatabase, "SQLite database path")
	before := flags.String("before", "", "delete events before YYYY-MM-DD")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *before == "" {
		return errors.New("--before is required")
	}
	store, err := database.Open(*databasePath, catalog.Empty())
	if err != nil {
		return err
	}
	defer store.Close()
	deleted, err := store.PurgeBefore(context.Background(), *before)
	if err != nil {
		return err
	}
	fmt.Printf("deleted %d events\n", deleted)
	return nil
}

func runDiscover(args []string) error {
	flags := flag.NewFlagSet("discover", flag.ContinueOnError)
	verbose := flags.Bool("verbose", false, "show every known search path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	paths := []struct {
		name      string
		path      string
		supported bool
	}{
		{"Claude Code", filepath.Join(home, ".claude"), true},
		{"Codex", filepath.Join(home, ".codex"), true},
		{"GitHub Copilot CLI", filepath.Join(home, ".copilot", "session-state"), true},
		{"OpenCode", filepath.Join(home, ".local", "share", "opencode", "opencode.db"), true},
		{"Antigravity CLI", filepath.Join(home, ".gemini", "antigravity-cli", "conversations"), true},
		{"Config", filepath.Join(home, ".config"), false},
		{"Local data", filepath.Join(home, ".local", "share"), false},
		{"Application Support", filepath.Join(home, "Library", "Application Support"), false},
	}
	found := false
	for _, item := range paths {
		if _, err := os.Stat(item.path); err == nil {
			if item.supported {
				fmt.Printf("✓ %s detected at %s\n", item.name, displayHome(item.path, home))
				found = true
			} else if *verbose {
				fmt.Printf("· known search path at %s\n", displayHome(item.path, home))
			}
		} else if *verbose && item.supported {
			fmt.Printf("– %s not detected\n", item.name)
		}
	}
	if !found {
		fmt.Println("No supported usage logs found.")
		fmt.Println("Run tokemon discover --verbose for all checked paths.")
	}
	return nil
}

func runAgent(args []string) error {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	var jsonlPaths stringListFlag
	serverURL := flags.String("server", envOr("TOKEMON_SERVER_URL", "http://127.0.0.1:8080"), "Tokemon server URL")
	token := flags.String("token", os.Getenv("TOKEMON_INGEST_TOKEN"), "shared ingestion token")
	machineID := flags.String("machine-id", envOr("TOKEMON_MACHINE_ID", ""), "stable machine identifier (defaults to hostname)")
	home := flags.String("home", envOr("TOKEMON_HOME", ""), "home directory to scan (defaults to the current user's home)")
	interval := flags.Duration("interval", configDurationEnv("TOKEMON_SCAN_INTERVAL", time.Minute), "poll interval")
	onceTimeout := flags.Duration("timeout", 5*time.Minute, "maximum duration for a one-shot scan and upload")
	configPath := flags.String("config", envOr("TOKEMON_AGENT_CONFIG", ""), "dotenv config path (defaults to ~/.config/tokemon/agent.env)")
	statePath := flags.String("state", envOr("TOKEMON_STATE", ""), "local state database (defaults to ~/.local/share/tokemon/state.db)")
	once := flags.Bool("once", false, "scan and upload once, then exit")
	flags.Var(&jsonlPaths, "jsonl", "normalized generic JSONL path or glob (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *interval <= 0 {
		return errors.New("--interval must be positive")
	}
	if *onceTimeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if *home == "" {
		var err error
		*home, err = os.UserHomeDir()
		if err != nil {
			return err
		}
	}
	if *configPath == "" {
		*configPath = localagent.DefaultConfigPath(*home)
	}
	configValues, err := localagent.ReadEnvFile(*configPath)
	if err != nil {
		return err
	}
	applyAgentConfig(flags, configValues, serverURL, token, machineID, home, interval, statePath)
	if *machineID == "" {
		var err error
		*machineID, err = os.Hostname()
		if err != nil || strings.TrimSpace(*machineID) == "" {
			*machineID = "unknown-" + runtime.GOOS
		}
	}
	if *statePath == "" {
		*statePath = localagent.DefaultStatePath(*home)
	}
	stateStore, err := localagent.OpenState(*statePath)
	if err != nil {
		return err
	}
	defer stateStore.Close()

	list := []adapters.Adapter{claude.New(*home), codex.New(*home), copilot.New(*home), opencode.New(*home), antigravity.New(*home)}
	if len(jsonlPaths) > 0 {
		list = append(list, generic.New(jsonlPaths...))
	}
	client := localagent.Client{ServerURL: *serverURL, Token: *token}
	pass := func(ctx context.Context) error {
		snapshot, err := stateStore.Snapshot(ctx, *machineID)
		if err != nil {
			return err
		}
		events, reports := adapters.CollectWithCursors(ctx, list, *machineID, snapshot.Cursors)
		var sourceErrors []error
		for _, report := range reports {
			if report.Err != nil {
				err := fmt.Errorf("%s %s: %w", report.Adapter, displayHome(report.Path, *home), report.Err)
				sourceErrors = append(sourceErrors, err)
				fmt.Fprintln(os.Stderr, "tokemon agent:", err)
				continue
			}
			fmt.Printf("%s %s: %d session snapshots\n", report.Adapter, displayHome(report.Path, *home), report.Events)
		}
		pending, err := stateStore.Pending(ctx, *machineID, events)
		if err != nil {
			return err
		}
		eventCount := len(events)
		events = nil
		if len(pending) > 0 {
			result, err := client.Ingest(ctx, pending)
			if err != nil {
				return err
			}
			fmt.Printf("synced %d changed events (%d accepted, %d refreshed) · %d lifetime tokens\n", len(pending), result.Accepted, result.Duplicates, result.CurrentTotal)
		}
		if err := stateStore.Commit(ctx, *machineID, reports, pending, time.Now().UTC()); err != nil {
			return err
		}
		if len(pending) == 0 {
			if eventCount == 0 {
				fmt.Println("No supported local usage records found.")
			} else {
				fmt.Println("No new or changed usage records.")
			}
		}
		if len(sourceErrors) > 0 {
			return errors.Join(sourceErrors...)
		}
		return nil
	}

	if *once {
		ctx, cancel := context.WithTimeout(context.Background(), *onceTimeout)
		defer cancel()
		return pass(ctx)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	currentInterval := *interval
	for {
		err := pass(ctx)
		if err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "tokemon agent:", err)
			currentInterval = currentInterval * 2
			if currentInterval > 5*time.Minute {
				currentInterval = 5 * time.Minute
			}
			if currentInterval < *interval {
				currentInterval = *interval
			}
		} else {
			currentInterval = *interval
		}
		timer := time.NewTimer(currentInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

type stringListFlag []string

func (values *stringListFlag) String() string { return strings.Join(*values, ",") }

func (values *stringListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("path or glob must not be empty")
	}
	*values = append(*values, value)
	return nil
}

func applyAgentConfig(flags *flag.FlagSet, values map[string]string, serverURL, token, machineID, home *string, interval *time.Duration, statePath *string) {
	if !flagWasSet(flags, "server") && os.Getenv("TOKEMON_SERVER_URL") == "" {
		if value := strings.TrimSpace(values["TOKEMON_SERVER_URL"]); value != "" {
			*serverURL = value
		}
	}
	if !flagWasSet(flags, "token") && os.Getenv("TOKEMON_INGEST_TOKEN") == "" {
		if value := values["TOKEMON_INGEST_TOKEN"]; value != "" {
			*token = value
		}
	}
	if !flagWasSet(flags, "machine-id") && os.Getenv("TOKEMON_MACHINE_ID") == "" {
		if value := strings.TrimSpace(values["TOKEMON_MACHINE_ID"]); value != "" {
			*machineID = value
		}
	}
	if !flagWasSet(flags, "home") && os.Getenv("TOKEMON_HOME") == "" {
		if value := strings.TrimSpace(values["TOKEMON_HOME"]); value != "" {
			*home = value
		}
	}
	if !flagWasSet(flags, "interval") && os.Getenv("TOKEMON_SCAN_INTERVAL") == "" {
		if value := strings.TrimSpace(values["TOKEMON_SCAN_INTERVAL"]); value != "" {
			if parsed, err := time.ParseDuration(value); err == nil {
				*interval = parsed
			}
		}
	}
	if !flagWasSet(flags, "state") && os.Getenv("TOKEMON_STATE") == "" {
		if value := strings.TrimSpace(values["TOKEMON_STATE"]); value != "" {
			*statePath = value
		}
	}
}

func applyServerConfig(flags *flag.FlagSet, values map[string]string, addr, databasePath, ingestToken, catalogPath *string) {
	if !flagWasSet(flags, "addr") && os.Getenv("TOKEMON_SERVER_ADDR") == "" {
		if value := strings.TrimSpace(values["TOKEMON_SERVER_ADDR"]); value != "" {
			*addr = value
		}
	}
	if !flagWasSet(flags, "database") && os.Getenv("TOKEMON_DATABASE") == "" {
		if value := strings.TrimSpace(values["TOKEMON_DATABASE"]); value != "" {
			*databasePath = value
		}
	}
	if !flagWasSet(flags, "ingest-token") && os.Getenv("TOKEMON_INGEST_TOKEN") == "" {
		if value := values["TOKEMON_INGEST_TOKEN"]; value != "" {
			*ingestToken = value
		}
	}
	if !flagWasSet(flags, "catalog") && os.Getenv("TOKEMON_MODEL_CATALOG") == "" {
		if value := strings.TrimSpace(values["TOKEMON_MODEL_CATALOG"]); value != "" {
			*catalogPath = value
		}
	}
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			set = true
		}
	})
	return set
}

func configDurationEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func readEvents(path string) ([]usage.Event, error) {
	input, closeInput, err := openInput(path)
	if err != nil {
		return nil, err
	}
	defer closeInput()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	var events []usage.Event
	for line := 1; scanner.Scan(); line++ {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var event usage.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if err := event.Validate(); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func openInput(path string) (io.Reader, func(), error) {
	if path == "-" {
		return os.Stdin, func() {}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return file, func() { _ = file.Close() }, nil
}

func openOutput(path string) (io.Writer, func(), error) {
	if path == "-" {
		return os.Stdout, func() {}, nil
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return file, func() { _ = file.Close() }, nil
}

func loadCatalog(path string) (*catalog.Catalog, error) {
	if path == "" {
		return catalog.Empty(), nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return catalog.Empty(), nil
	}
	return catalog.Load(path)
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func displayHome(path, home string) string {
	return strings.Replace(path, home, "~", 1)
}

func printUsage() {
	fmt.Println(`tokemon — local-first token analytics

Commands:
  serve       start the HTTP API and dashboard
	 agent       scan local Claude Code/Codex/Copilot/OpenCode usage and upload metadata
  import      ingest normalized JSONL into SQLite
  export      export normalized JSONL from SQLite
  inspect     validate and print the outgoing JSONL payload
  discover    report supported local tool locations
  catalog     validate the model pricing catalog
  purge       delete events before a date

Examples:
  tokemon serve --config ~/.config/tokemon/server.env
  tokemon agent --server http://127.0.0.1:8080 --once
  tokemon import --database ./data/tokemon.db usage.jsonl
  tokemon inspect usage.jsonl
  tokemon catalog validate --catalog catalog/models.yaml`)
}
