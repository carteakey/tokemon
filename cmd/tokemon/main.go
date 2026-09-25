package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/adapters/builtin"
	localagent "github.com/tokemon/tokemon/internal/agent"
	"github.com/tokemon/tokemon/internal/api"
	"github.com/tokemon/tokemon/internal/art"
	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
	"github.com/tokemon/tokemon/internal/version"
)

const (
	defaultDatabase          = "tokemon.db"
	minAgentFailureBackoff   = 5 * time.Second
	maxAgentFailureBackoff   = 5 * time.Minute
	defaultReadTimeout       = 30 * time.Second
	defaultReadHeaderTimeout = 10 * time.Second
	defaultWriteTimeout      = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	defaultShutdownTimeout   = 10 * time.Second
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tokemon:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("command required; try: tokemon serve, agent, import, export, inspect, discover, catalog, art, version, purge, or backup")
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
	case "backup":
		return runBackup(args[1:])
	case "agent":
		return runAgent(args[1:])
	case "catalog":
		return runCatalog(args[1:])
	case "art":
		return runArt(args[1:])
	case "version":
		return runVersion(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runVersion(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: tokemon version")
	}
	fmt.Println(version.Current().String())
	return nil
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

func runArt(args []string) error {
	if len(args) == 0 || args[0] != "validate" {
		return errors.New("usage: tokemon art validate [--dir web/static/tokemon]")
	}
	flags := flag.NewFlagSet("art validate", flag.ContinueOnError)
	directory := flags.String("dir", "web/static/tokemon", "evolution art directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: tokemon art validate [--dir web/static/tokemon]")
	}
	report, err := art.ValidateDir(*directory)
	if err != nil {
		return err
	}
	fmt.Printf("art valid: %d stage assets, largest edge %dx%d, RGBA alpha verified\n", report.Assets, report.MaxWidth, report.MaxHeight)
	return nil
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", envOr("TOKEMON_SERVER_ADDR", ":8080"), "HTTP listen address")
	databasePath := flags.String("database", envOr("TOKEMON_DATABASE", defaultDatabase), "SQLite database path")
	ingestToken := flags.String("ingest-token", os.Getenv("TOKEMON_INGEST_TOKEN"), "shared token for event ingestion")
	dashboardToken := flags.String("dashboard-token", os.Getenv("TOKEMON_DASHBOARD_TOKEN"), "token for dashboard and read API access (defaults to ingest token)")
	devLoopback := flags.Bool("dev-loopback", envBool("TOKEMON_DEV_LOOPBACK"), "allow empty ingest token only on an explicit loopback listener")
	trustedProxyCIDRs := flags.String("trusted-proxy-cidrs", os.Getenv("TOKEMON_TRUSTED_PROXY_CIDRS"), "comma-separated proxy CIDRs allowed to assert X-Forwarded-User")
	readTimeout := flags.Duration("read-timeout", configDurationEnv("TOKEMON_SERVER_READ_TIMEOUT", defaultReadTimeout), "maximum duration for reading a request")
	readHeaderTimeout := flags.Duration("read-header-timeout", configDurationEnv("TOKEMON_SERVER_READ_HEADER_TIMEOUT", defaultReadHeaderTimeout), "maximum duration for reading request headers")
	writeTimeout := flags.Duration("write-timeout", configDurationEnv("TOKEMON_SERVER_WRITE_TIMEOUT", defaultWriteTimeout), "maximum duration for writing a response")
	idleTimeout := flags.Duration("idle-timeout", configDurationEnv("TOKEMON_SERVER_IDLE_TIMEOUT", defaultIdleTimeout), "maximum keep-alive idle duration")
	shutdownTimeout := flags.Duration("shutdown-timeout", configDurationEnv("TOKEMON_SERVER_SHUTDOWN_TIMEOUT", defaultShutdownTimeout), "bounded graceful shutdown duration")
	catalogPath := flags.String("catalog", envOr("TOKEMON_MODEL_CATALOG", "catalog/models.yaml"), "model catalog YAML path")
	timezone := flags.String("timezone", envOr("TOKEMON_ANALYTICS_TIMEZONE", "UTC"), "IANA timezone used for calendar bucketing")
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
	applyServerConfig(flags, configValues, addr, databasePath, ingestToken, catalogPath, timezone)
	applyServerSecurityConfig(flags, configValues, dashboardToken, devLoopback, trustedProxyCIDRs)
	applyServerTimeoutConfig(flags, configValues, readTimeout, readHeaderTimeout, writeTimeout, idleTimeout, shutdownTimeout)
	if *readTimeout <= 0 || *readHeaderTimeout <= 0 || *writeTimeout <= 0 || *idleTimeout <= 0 || *shutdownTimeout <= 0 {
		return errors.New("server timeouts must be positive")
	}
	if err := api.ValidateServeConfig(*addr, *ingestToken, *devLoopback); err != nil {
		return err
	}
	modelCatalog, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	location, err := loadTimezone(*timezone)
	if err != nil {
		return err
	}
	store, err := database.OpenWithLocation(*databasePath, modelCatalog, location)
	if err != nil {
		return err
	}
	defer store.Close()
	server, err := api.NewWithConfig(store, api.Config{
		IngestToken:       *ingestToken,
		DashboardToken:    *dashboardToken,
		AllowLoopbackDev:  *devLoopback,
		TrustedProxyCIDRs: splitConfiguredPaths(*trustedProxyCIDRs),
		Logger:            slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
	if err != nil {
		return err
	}
	fmt.Printf("Tokemon listening on http://%s\n", *addr)
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadTimeout:       *readTimeout,
		ReadHeaderTimeout: *readHeaderTimeout,
		WriteTimeout:      *writeTimeout,
		IdleTimeout:       *idleTimeout,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveHTTP(ctx, httpServer, *shutdownTimeout)
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

func runBackup(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tokemon backup create|restore|verify [flags]")
	}
	switch args[0] {
	case "create":
		return runBackupCreate(args[1:])
	case "restore":
		return runBackupRestore(args[1:])
	case "verify":
		return runBackupVerify(args[1:])
	default:
		return fmt.Errorf("unknown backup command %q", args[0])
	}
}

func runBackupCreate(args []string) error {
	flags := flag.NewFlagSet("backup create", flag.ContinueOnError)
	databasePath := flags.String("database", defaultDatabase, "SQLite database path")
	destination := flags.String("destination", "", "versioned backup destination directory")
	retention := flags.Int("retention", 10, "number of versioned backups to retain")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: tokemon backup create --destination DIR [--database PATH] [--retention N]")
	}
	result, err := database.CreateBackup(context.Background(), database.BackupOptions{
		DatabasePath: *databasePath, DestinationDir: *destination, Retention: *retention,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func runBackupRestore(args []string) error {
	flags := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	databasePath := flags.String("database", defaultDatabase, "destination SQLite database path")
	source := flags.String("source", "", "verified SQLite backup path")
	force := flags.Bool("force", false, "replace an existing destination and retain a pre-restore snapshot")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*source) == "" {
		return errors.New("usage: tokemon backup restore --source BACKUP.db [--database PATH] [--force]")
	}
	result, err := database.Restore(context.Background(), database.RestoreOptions{
		DatabasePath: *databasePath, BackupPath: *source, Force: *force,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func runBackupVerify(args []string) error {
	flags := flag.NewFlagSet("backup verify", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: tokemon backup verify BACKUP.db")
	}
	result, err := database.VerifyBackup(context.Background(), flags.Arg(0))
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
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
		{"OpenClaw", filepath.Join(home, ".openclaw", "agents"), true},
		{"Hermes Agent", filepath.Join(home, ".hermes"), true},
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
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	var jsonlPaths stringListFlag
	serverURL := flags.String("server", envOr("TOKEMON_SERVER_URL", "http://127.0.0.1:8080"), "Tokemon server URL")
	token := flags.String("token", os.Getenv("TOKEMON_INGEST_TOKEN"), "shared ingestion token")
	machineID := flags.String("machine-id", envOr("TOKEMON_MACHINE_ID", ""), "stable machine identifier (defaults to hostname)")
	home := flags.String("home", envOr("TOKEMON_HOME", ""), "home directory to scan (defaults to the current user's home)")
	adapterSelection := flags.String("adapters", envOr("TOKEMON_ADAPTERS", ""), "comma-separated adapter IDs (default: all built-ins)")
	interval := flags.Duration("interval", configDurationEnv("TOKEMON_SCAN_INTERVAL", time.Minute), "poll interval")
	onceTimeout := flags.Duration("timeout", 5*time.Minute, "maximum duration for a one-shot scan and upload")
	requestTimeout := flags.Duration("request-timeout", configDurationEnv("TOKEMON_AGENT_REQUEST_TIMEOUT", localagent.DefaultRequestTimeout), "per-request health, heartbeat, and ingest deadline")
	verbose := flags.Bool("verbose", false, "show every source during a scan")
	includeProjects := flags.Bool("include-projects", true, "include normalized project basenames (set false to opt out)")
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
	applyAgentConfig(flags, configValues, serverURL, token, machineID, home, adapterSelection, interval, statePath, includeProjects)
	applyAgentRequestTimeoutConfig(flags, configValues, requestTimeout)
	if len(jsonlPaths) == 0 {
		configuredJSONL := strings.TrimSpace(os.Getenv("TOKEMON_JSONL_PATHS"))
		if configuredJSONL == "" {
			configuredJSONL = configValues["TOKEMON_JSONL_PATHS"]
		}
		for _, path := range splitConfiguredPaths(configuredJSONL) {
			_ = jsonlPaths.Set(path)
		}
	}
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

	list, definitions, err := builtin.Build(builtin.Config{Home: *home, JSONLPaths: append([]string(nil), jsonlPaths...)}, splitConfiguredList(*adapterSelection))
	if err != nil {
		return err
	}
	if *requestTimeout <= 0 {
		return errors.New("--request-timeout must be positive")
	}
	client := localagent.Client{ServerURL: *serverURL, Token: *token, RequestTimeout: *requestTimeout}
	runtimeVersion := version.Current()
	heartbeatAdapters := make([]localagent.AdapterHeartbeat, 0, len(definitions))
	for index, definition := range definitions {
		heartbeatAdapters = append(heartbeatAdapters, localagent.AdapterHeartbeat{
			ID: definition.ID, Version: definition.Version, Capabilities: list[index].Capabilities(),
		})
	}
	pass := func(ctx context.Context) error {
		if err := client.Health(ctx); err != nil {
			return fmt.Errorf("hub health check: %w", err)
		}
		snapshot, err := stateStore.Snapshot(ctx, *machineID)
		if err != nil {
			return err
		}
		events, reports := adapters.CollectWithCursors(ctx, list, *machineID, snapshot.Cursors)
		var sourceErrors []error
		sourceCount := 0
		for _, report := range reports {
			if report.Path != "" {
				sourceCount++
			}
			if report.Err != nil {
				err := fmt.Errorf("%s source collection failed", report.Adapter)
				sourceErrors = append(sourceErrors, err)
				slog.Default().Warn("source_error", "adapter", report.Adapter, "path_present", report.Path != "")
				continue
			}
			if *verbose {
				fmt.Printf("%s %s: %d session snapshots\n", report.Adapter, displayHome(report.Path, *home), report.Events)
			}
		}
		snapshotCount := len(events)
		if !*includeProjects {
			for index := range events {
				events[index].Project = ""
			}
		}
		if !*verbose {
			fmt.Printf("scanned %d sources (%d session snapshots)\n", len(reports), snapshotCount)
		}
		if err := client.Heartbeat(ctx, localagent.HeartbeatRequest{
			MachineID:        *machineID,
			AgentVersion:     runtimeVersion.Version,
			OperatingSystem:  runtimeVersion.OS,
			Architecture:     runtimeVersion.Arch,
			Adapters:         heartbeatAdapters,
			SourceCount:      sourceCount,
			SourceErrorCount: len(sourceErrors),
		}); err != nil {
			// Heartbeats are diagnostic and must never prevent a successful data
			// upload or advance/rollback local cursor state.
			slog.Default().Warn("heartbeat_error")
		}
		pending, err := stateStore.Pending(ctx, *machineID, events)
		if err != nil {
			return err
		}
		eventCount := snapshotCount
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
			currentInterval = nextAgentFailureInterval(*interval, currentInterval)
			slog.Default().Warn("agent_pass_error", "retry_in_ms", currentInterval.Milliseconds(), "source_error", len(strings.Split(err.Error(), "\n")))
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

func nextAgentFailureInterval(configured, current time.Duration) time.Duration {
	baseline := configured
	if baseline < minAgentFailureBackoff {
		baseline = minAgentFailureBackoff
	}
	ceiling := maxAgentFailureBackoff
	if ceiling < baseline {
		ceiling = baseline
	}
	if current < baseline {
		return baseline
	}
	if current >= ceiling || current > ceiling/2 {
		return ceiling
	}
	return current * 2
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

func applyAgentConfig(flags *flag.FlagSet, values map[string]string, serverURL, token, machineID, home, adapterSelection *string, interval *time.Duration, statePath *string, includeProjects *bool) {
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
	if !flagWasSet(flags, "adapters") && os.Getenv("TOKEMON_ADAPTERS") == "" {
		if value := strings.TrimSpace(values["TOKEMON_ADAPTERS"]); value != "" {
			*adapterSelection = value
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
	if !flagWasSet(flags, "include-projects") {
		value := strings.TrimSpace(os.Getenv("TOKEMON_INCLUDE_PROJECTS"))
		if value == "" {
			value = strings.TrimSpace(values["TOKEMON_INCLUDE_PROJECTS"])
		}
		if value != "" {
			if parsed, err := strconv.ParseBool(value); err == nil {
				*includeProjects = parsed
			}
		}
	}
}

func applyAgentRequestTimeoutConfig(flags *flag.FlagSet, values map[string]string, timeout *time.Duration) {
	if flagWasSet(flags, "request-timeout") || os.Getenv("TOKEMON_AGENT_REQUEST_TIMEOUT") != "" {
		return
	}
	if value := strings.TrimSpace(values["TOKEMON_AGENT_REQUEST_TIMEOUT"]); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			*timeout = parsed
		}
	}
}

func applyServerConfig(flags *flag.FlagSet, values map[string]string, addr, databasePath, ingestToken, catalogPath, timezone *string) {
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
	if !flagWasSet(flags, "timezone") && os.Getenv("TOKEMON_ANALYTICS_TIMEZONE") == "" {
		if value := strings.TrimSpace(values["TOKEMON_ANALYTICS_TIMEZONE"]); value != "" {
			*timezone = value
		}
	}
}

func applyServerSecurityConfig(flags *flag.FlagSet, values map[string]string, dashboardToken *string, devLoopback *bool, trustedProxyCIDRs *string) {
	if !flagWasSet(flags, "dashboard-token") && os.Getenv("TOKEMON_DASHBOARD_TOKEN") == "" {
		if value := values["TOKEMON_DASHBOARD_TOKEN"]; value != "" {
			*dashboardToken = value
		}
	}
	if !flagWasSet(flags, "dev-loopback") && os.Getenv("TOKEMON_DEV_LOOPBACK") == "" {
		if value := strings.TrimSpace(values["TOKEMON_DEV_LOOPBACK"]); value != "" {
			if parsed, err := strconv.ParseBool(value); err == nil {
				*devLoopback = parsed
			}
		}
	}
	if !flagWasSet(flags, "trusted-proxy-cidrs") && os.Getenv("TOKEMON_TRUSTED_PROXY_CIDRS") == "" {
		if value := strings.TrimSpace(values["TOKEMON_TRUSTED_PROXY_CIDRS"]); value != "" {
			*trustedProxyCIDRs = value
		}
	}
}

func applyServerTimeoutConfig(flags *flag.FlagSet, values map[string]string, readTimeout, readHeaderTimeout, writeTimeout, idleTimeout, shutdownTimeout *time.Duration) {
	items := []struct {
		flagName string
		envName  string
		key      string
		target   *time.Duration
	}{
		{"read-timeout", "TOKEMON_SERVER_READ_TIMEOUT", "TOKEMON_SERVER_READ_TIMEOUT", readTimeout},
		{"read-header-timeout", "TOKEMON_SERVER_READ_HEADER_TIMEOUT", "TOKEMON_SERVER_READ_HEADER_TIMEOUT", readHeaderTimeout},
		{"write-timeout", "TOKEMON_SERVER_WRITE_TIMEOUT", "TOKEMON_SERVER_WRITE_TIMEOUT", writeTimeout},
		{"idle-timeout", "TOKEMON_SERVER_IDLE_TIMEOUT", "TOKEMON_SERVER_IDLE_TIMEOUT", idleTimeout},
		{"shutdown-timeout", "TOKEMON_SERVER_SHUTDOWN_TIMEOUT", "TOKEMON_SERVER_SHUTDOWN_TIMEOUT", shutdownTimeout},
	}
	for _, item := range items {
		if flagWasSet(flags, item.flagName) || os.Getenv(item.envName) != "" {
			continue
		}
		if value := strings.TrimSpace(values[item.key]); value != "" {
			if parsed, err := time.ParseDuration(value); err == nil {
				*item.target = parsed
			}
		}
	}
}

func serveHTTP(ctx context.Context, server *http.Server, shutdownTimeout time.Duration) error {
	result := make(chan error, 1)
	go func() {
		result <- server.ListenAndServe()
	}()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			// Shutdown leaves handlers that ignore their context running after the
			// deadline. Force-close those connections before the caller closes
			// SQLite so a stuck request cannot outlive the store.
			_ = server.Close()
		}
		serveErr := <-result
		if shutdownErr != nil {
			return shutdownErr
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	}
}

func envBool(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
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

func splitConfiguredList(value string) []string {
	var result []string
	for _, item := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	}) {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func splitConfiguredPaths(value string) []string {
	var result []string
	for _, item := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	}) {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func loadTimezone(value string) (*time.Location, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "UTC"
	}
	location, err := time.LoadLocation(value)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", value, err)
	}
	return location, nil
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
		event, err := usage.DecodeOutboundEvent(scanner.Bytes())
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if err := event.Validate(); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if err := usage.ValidateOutbound(event); err != nil {
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
  agent       scan local provider usage and upload metadata
  version     print build and platform identity
  import      ingest normalized JSONL into SQLite
  export      export normalized JSONL from SQLite
  inspect     validate and print the outgoing JSONL payload
  discover    report supported local tool locations
  catalog     validate the model pricing catalog
  art         validate evolution art assets and manifest
  purge       delete events before a date
  backup      create, verify, or restore SQLite snapshots

Examples:
  tokemon serve --config ~/.config/tokemon/server.env
  tokemon agent --server http://127.0.0.1:8080 --adapters claude-code,codex --once
  tokemon version
  tokemon import --database ./data/tokemon.db usage.jsonl
  tokemon backup create --database ./data/tokemon.db --destination /mnt/off-host/tokemon
  tokemon backup verify /mnt/off-host/tokemon/tokemon-backup-v4-20260810T000000.000000000Z.db
  tokemon inspect usage.jsonl
  tokemon catalog validate --catalog catalog/models.yaml
  tokemon art validate --dir web/static/tokemon`)
}
