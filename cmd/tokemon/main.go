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
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	insightai "github.com/tokemon/tokemon/internal/insights"
	"github.com/tokemon/tokemon/internal/quota"
	"github.com/tokemon/tokemon/internal/usage"
	"github.com/tokemon/tokemon/internal/version"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
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
	defaultBackupRetention   = 10
	defaultBackupInterval    = 24 * time.Hour
	defaultStatusTimeout     = 10 * time.Second
	automatedBackupTimeout   = 5 * time.Minute
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tokemon:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("command required; try: tokemon serve, agent, status, import, export, inspect, discover, catalog, art, version, purge, or backup")
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "status":
		return runStatus(args[1:])
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
	case "passwd":
		return runPasswd(args[1:])
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
	dashboardUsername := flags.String("dashboard-username", os.Getenv("TOKEMON_DASHBOARD_USERNAME"), "dashboard login username for password authentication")
	dashboardPasswordHash := flags.String("dashboard-password-hash", os.Getenv("TOKEMON_DASHBOARD_PASSWORD_HASH"), "bcrypt hash of the dashboard login password (generate with: tokemon passwd)")
	devLoopback := flags.Bool("dev-loopback", envBool("TOKEMON_DEV_LOOPBACK"), "allow empty ingest token only on an explicit loopback listener")
	enableTokedex := flags.Bool("enable-tokedex", envBool("TOKEMON_ENABLE_TOKEDEX"), "expose the /tokedex catalog page and its navigation link (default off; see docs/tokedex-roadmap.md)")
	trustedProxyCIDRs := flags.String("trusted-proxy-cidrs", os.Getenv("TOKEMON_TRUSTED_PROXY_CIDRS"), "comma-separated proxy CIDRs allowed to assert X-Forwarded-User")
	readTimeout := flags.Duration("read-timeout", configDurationEnv("TOKEMON_SERVER_READ_TIMEOUT", defaultReadTimeout), "maximum duration for reading a request")
	readHeaderTimeout := flags.Duration("read-header-timeout", configDurationEnv("TOKEMON_SERVER_READ_HEADER_TIMEOUT", defaultReadHeaderTimeout), "maximum duration for reading request headers")
	writeTimeout := flags.Duration("write-timeout", configDurationEnv("TOKEMON_SERVER_WRITE_TIMEOUT", defaultWriteTimeout), "maximum duration for writing a response")
	idleTimeout := flags.Duration("idle-timeout", configDurationEnv("TOKEMON_SERVER_IDLE_TIMEOUT", defaultIdleTimeout), "maximum keep-alive idle duration")
	shutdownTimeout := flags.Duration("shutdown-timeout", configDurationEnv("TOKEMON_SERVER_SHUTDOWN_TIMEOUT", defaultShutdownTimeout), "bounded graceful shutdown duration")
	catalogPath := flags.String("catalog", envOr("TOKEMON_MODEL_CATALOG", "catalog/models.yaml"), "model catalog YAML path")
	timezone := flags.String("timezone", envOr("TOKEMON_ANALYTICS_TIMEZONE", "UTC"), "IANA timezone used for calendar bucketing")
	quotasPath := flags.String("quotas", envOr("TOKEMON_QUOTAS", ""), "usage quotas YAML path (optional; no file disables quota tracking)")
	backupDir := flags.String("backup-dir", envOr("TOKEMON_BACKUP_DIR", ""), "directory for automated SQLite backups (defaults to <database-dir>/backups)")
	backupRetention := flags.Int("backup-retention", configIntEnv("TOKEMON_BACKUP_RETENTION", defaultBackupRetention), "number of automated backups to retain")
	backupInterval := flags.Duration("backup-interval", configDurationEnv("TOKEMON_BACKUP_INTERVAL", defaultBackupInterval), "automated backup interval (0 disables automated backups)")
	alertWebhookURL := flags.String("alert-webhook-url", os.Getenv("TOKEMON_ALERT_WEBHOOK_URL"), "optional HTTP webhook URL for quota warning/overage alerts")
	alertNtfyURL := flags.String("alert-ntfy-url", os.Getenv("TOKEMON_ALERT_NTFY_URL"), "optional ntfy topic URL for quota warning/overage alerts")
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
	applyServerConfig(flags, configValues, addr, databasePath, ingestToken, catalogPath, timezone, quotasPath, backupDir, backupRetention, backupInterval)
	applyServerAlertConfig(flags, configValues, alertWebhookURL, alertNtfyURL)
	applyServerSecurityConfig(flags, configValues, dashboardToken, devLoopback, enableTokedex, trustedProxyCIDRs, dashboardUsername, dashboardPasswordHash)
	dashboardTokenBefore := *dashboardToken
	*dashboardToken = applyDashboardTokenFallback(*dashboardToken, *ingestToken, flags)
	dashboardTokenFallback := dashboardTokenBefore == "" && *dashboardToken != ""
	applyServerTimeoutConfig(flags, configValues, readTimeout, readHeaderTimeout, writeTimeout, idleTimeout, shutdownTimeout)
	if *readTimeout <= 0 || *readHeaderTimeout <= 0 || *writeTimeout <= 0 || *idleTimeout <= 0 || *shutdownTimeout <= 0 {
		return errors.New("server timeouts must be positive")
	}
	if *backupInterval < 0 {
		return errors.New("backup interval must be non-negative")
	}
	if *backupInterval > 0 && *backupRetention < 1 {
		return errors.New("backup retention must be at least 1")
	}
	if strings.TrimSpace(*backupDir) == "" && isFileBackedDB(*databasePath) {
		*backupDir = filepath.Join(filepath.Dir(*databasePath), "backups")
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
	var quotas []quota.Quota
	if strings.TrimSpace(*quotasPath) != "" {
		quotas, err = quota.Load(*quotasPath)
		if err != nil {
			return err
		}
	}
	store, err := database.OpenWithLocation(*databasePath, modelCatalog, location, quotas...)
	if err != nil {
		return err
	}
	defer store.Close()
	insightsAPIKey := configuredSecret(configValues, "TOKEMON_INSIGHTS_OPENAI_API_KEY")
	var insightsSynthesizer insightai.Synthesizer
	if insightsAPIKey != "" {
		insightsSynthesizer = insightai.New(insightai.Config{
			APIKey:  insightsAPIKey,
			Model:   configuredValue(configValues, "TOKEMON_INSIGHTS_OPENAI_MODEL"),
			BaseURL: configuredValue(configValues, "TOKEMON_INSIGHTS_OPENAI_BASE_URL"),
		})
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	server, err := api.NewWithConfigAndInsightsAI(store, api.Config{
		IngestToken:            *ingestToken,
		DashboardToken:         *dashboardToken,
		DashboardTokenFallback: dashboardTokenFallback,
		DashboardUsername:      *dashboardUsername,
		DashboardPasswordHash:  []byte(*dashboardPasswordHash),
		AllowLoopbackDev:       *devLoopback,
		EnableTokedex:          *enableTokedex,
		TrustedProxyCIDRs:      splitConfiguredPaths(*trustedProxyCIDRs),
		DatabasePath:           *databasePath,
		BackupDir:              *backupDir,
		BackupInterval:         *backupInterval,
		BackupRetention:        *backupRetention,
		AlertWebhookURL:        *alertWebhookURL,
		AlertNtfyURL:           *alertNtfyURL,
		Logger:                 logger,
	}, insightsSynthesizer)
	if err != nil {
		return err
	}
	defer server.Close()

	var backupWG sync.WaitGroup
	defer backupWG.Wait()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Warm the dashboard and default analytics while the listener and server
	// come up, so the compute overlaps process startup instead of queueing
	// behind it. Detached and time-bounded; never blocks startup or healthz.
	go server.WarmCaches(ctx)
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	startAutomatedBackups(ctx, &backupWG, logger, *databasePath, *backupDir, *backupRetention, *backupInterval)
	fmt.Printf("Tokemon listening on http://%s\n", listener.Addr().String())
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadTimeout:       *readTimeout,
		ReadHeaderTimeout: *readHeaderTimeout,
		WriteTimeout:      *writeTimeout,
		IdleTimeout:       *idleTimeout,
	}
	return serveHTTP(ctx, httpServer, listener, *shutdownTimeout)
}

func configuredValue(values map[string]string, name string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return strings.TrimSpace(values[name])
}

func configuredSecret(values map[string]string, name string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return values[name]
}

// bcrypt only digests the first 72 bytes of a password — the algorithm's
// input limit — so longer passphrases lose everything past that point.
// Current x/crypto refuses such passwords outright instead of truncating.
const maxBcryptPasswordBytes = 72

// runPasswd prints a bcrypt hash of a dashboard login password read from the
// terminal or, with --stdin, from standard input. Configure the hash together
// with TOKEMON_DASHBOARD_USERNAME to enable dashboard password authentication.
func runPasswd(args []string) error {
	flags := flag.NewFlagSet("passwd", flag.ContinueOnError)
	stdin := flags.Bool("stdin", false, "read the password from standard input")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: tokemon passwd [--stdin]")
	}
	var input io.Reader
	if *stdin {
		input = os.Stdin
	} else if file, err := os.Stdin.Stat(); err != nil || file.Mode()&os.ModeCharDevice == 0 {
		return errors.New("standard input is not a terminal; pass --stdin to read the password from a pipe")
	}
	password, err := readPassword(input)
	if err != nil {
		return err
	}
	if len(password) > maxBcryptPasswordBytes {
		fmt.Fprintf(os.Stderr, "warning: the password is %d bytes; bcrypt uses at most the first %d\n", len(password), maxBcryptPasswordBytes)
		return fmt.Errorf("password exceeds bcrypt's %d-byte input limit; use a shorter password", maxBcryptPasswordBytes)
	}
	hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	fmt.Println(string(hash))
	return nil
}

func readPassword(input io.Reader) ([]byte, error) {
	if input == nil {
		password, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return nil, err
		}
		fmt.Fprintln(os.Stderr)
		return password, nil
	}
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && err != io.EOF {
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
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
		if err := encoder.Encode(usage.SanitizeOutbound(event)); err != nil {
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
	verbose := flags.Bool("verbose", false, "show every search path")
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
		{"ZCode", filepath.Join(home, ".zcode", "cli", "db", "db.sqlite"), true},
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
				fmt.Printf("· search path at %s\n", displayHome(item.path, home))
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
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: agentLogLevel()})))
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
		// Per-source health: committed sync times survive agent restarts, so
		// a currently failing source can still report when it last worked.
		// A state-read failure must not skip collection or upload.
		lastSyncs, err := stateStore.LastSuccessfulSyncs(ctx, *machineID)
		if err != nil {
			slog.Default().Warn("source_health_state_error")
			lastSyncs = nil
		}
		var sourceErrors []error
		sourceCount := 0
		sourceHealth := make([]localagent.SourceHealthHeartbeat, 0, len(reports))
		for _, report := range reports {
			if report.Path != "" {
				sourceCount++
			}
			entry := localagent.SourceHealthHeartbeat{Adapter: report.Adapter}
			if report.Err != nil {
				err := fmt.Errorf("%s source collection failed", report.Adapter)
				sourceErrors = append(sourceErrors, err)
				slog.Default().Warn("source_error", "adapter", report.Adapter, "path_present", report.Path != "")
				entry.LastErrorClass = localagent.SourceErrorClass(report.Err)
				if report.Path != "" {
					if synced, ok := lastSyncs[localagent.SourceKey(report.Adapter, report.Path)]; ok {
						syncedUTC := synced.UTC()
						entry.LastSuccessfulSync = &syncedUTC
					}
				}
			} else {
				if *verbose {
					fmt.Printf("%s %s: %d session snapshots\n", report.Adapter, displayHome(report.Path, *home), report.Events)
				}
				syncedNow := time.Now().UTC()
				entry.LastSuccessfulSync = &syncedNow
			}
			// Same safe-identity chain as the state store's commit: the
			// cursor or source identity is already hashed; raw paths are
			// never sent. A failed discovery has no source at all, so it
			// reports under a stable adapter-level pseudo-source instead of
			// disappearing from the per-source view.
			identity := report.Cursor.Identity
			if identity == "" {
				identity = report.Source.Identity
			}
			if identity == "" && report.Path != "" {
				identity = adapters.HashIdentity(report.Adapter + ":" + report.Path)
			}
			if identity == "" {
				identity = adapters.HashIdentity(report.Adapter + ":discover")
			}
			entry.IdentityHash = identity
			sourceHealth = append(sourceHealth, entry)
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
			Sources:          sourceHealth,
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
			if result.Rejected > 0 {
				message := fmt.Sprintf("hub rejected %d of %d events", result.Rejected, len(pending))
				if len(result.Errors) > 0 {
					message += ": " + strings.Join(result.Errors, "; ")
				}
				return errors.New(message)
			}
			fmt.Printf("synced %d changed events (%d accepted, %d refreshed) · %d lifetime tokens\n", len(pending), result.Accepted, result.Duplicates, result.CurrentTotal)
		}
		if err := stateStore.Commit(ctx, *machineID, snapshot, reports, pending, time.Now().UTC()); err != nil {
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

// agentLogLevel maps TOKEMON_LOG_LEVEL (debug, info, warn, error; default
// info) onto the agent's JSON log handler so debug diagnostics such as the
// SQLite handle cache's reopen and evict events can be enabled per deployment.
func agentLogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKEMON_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
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

func applyServerConfig(flags *flag.FlagSet, values map[string]string, addr, databasePath, ingestToken, catalogPath, timezone, quotasPath, backupDir *string, backupRetention *int, backupInterval *time.Duration) {
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
	if quotasPath != nil && !flagWasSet(flags, "quotas") && os.Getenv("TOKEMON_QUOTAS") == "" {
		if value := strings.TrimSpace(values["TOKEMON_QUOTAS"]); value != "" {
			*quotasPath = value
		}
	}
	if backupDir != nil && !flagWasSet(flags, "backup-dir") && os.Getenv("TOKEMON_BACKUP_DIR") == "" {
		if value := strings.TrimSpace(values["TOKEMON_BACKUP_DIR"]); value != "" {
			*backupDir = value
		}
	}
	if backupRetention != nil && !flagWasSet(flags, "backup-retention") && strings.TrimSpace(os.Getenv("TOKEMON_BACKUP_RETENTION")) == "" {
		if value := strings.TrimSpace(values["TOKEMON_BACKUP_RETENTION"]); value != "" {
			if parsed, err := strconv.Atoi(value); err == nil {
				*backupRetention = parsed
			}
		}
	}
	if backupInterval != nil && !flagWasSet(flags, "backup-interval") && strings.TrimSpace(os.Getenv("TOKEMON_BACKUP_INTERVAL")) == "" {
		if value := strings.TrimSpace(values["TOKEMON_BACKUP_INTERVAL"]); value != "" {
			if parsed, err := time.ParseDuration(value); err == nil {
				*backupInterval = parsed
			}
		}
	}
}

func applyServerAlertConfig(flags *flag.FlagSet, values map[string]string, alertWebhookURL, alertNtfyURL *string) {
	if alertWebhookURL != nil && !flagWasSet(flags, "alert-webhook-url") && strings.TrimSpace(os.Getenv("TOKEMON_ALERT_WEBHOOK_URL")) == "" {
		if value := strings.TrimSpace(values["TOKEMON_ALERT_WEBHOOK_URL"]); value != "" {
			*alertWebhookURL = value
		}
	}
	if alertNtfyURL != nil && !flagWasSet(flags, "alert-ntfy-url") && strings.TrimSpace(os.Getenv("TOKEMON_ALERT_NTFY_URL")) == "" {
		if value := strings.TrimSpace(values["TOKEMON_ALERT_NTFY_URL"]); value != "" {
			*alertNtfyURL = value
		}
	}
}

func applyServerSecurityConfig(flags *flag.FlagSet, values map[string]string, dashboardToken *string, devLoopback *bool, enableTokedex *bool, trustedProxyCIDRs *string, dashboardUsername *string, dashboardPasswordHash *string) {
	if !flagWasSet(flags, "dashboard-token") && os.Getenv("TOKEMON_DASHBOARD_TOKEN") == "" {
		if value := values["TOKEMON_DASHBOARD_TOKEN"]; value != "" {
			*dashboardToken = value
		}
	}
	if !flagWasSet(flags, "dashboard-username") && os.Getenv("TOKEMON_DASHBOARD_USERNAME") == "" {
		if value := strings.TrimSpace(values["TOKEMON_DASHBOARD_USERNAME"]); value != "" {
			*dashboardUsername = value
		}
	}
	if !flagWasSet(flags, "dashboard-password-hash") && os.Getenv("TOKEMON_DASHBOARD_PASSWORD_HASH") == "" {
		if value := strings.TrimSpace(values["TOKEMON_DASHBOARD_PASSWORD_HASH"]); value != "" {
			*dashboardPasswordHash = value
		}
	}
	if !flagWasSet(flags, "dev-loopback") && os.Getenv("TOKEMON_DEV_LOOPBACK") == "" {
		if value := strings.TrimSpace(values["TOKEMON_DEV_LOOPBACK"]); value != "" {
			if parsed, err := strconv.ParseBool(value); err == nil {
				*devLoopback = parsed
			}
		}
	}
	if !flagWasSet(flags, "enable-tokedex") && os.Getenv("TOKEMON_ENABLE_TOKEDEX") == "" {
		if value := strings.TrimSpace(values["TOKEMON_ENABLE_TOKEDEX"]); value != "" {
			if parsed, err := strconv.ParseBool(value); err == nil {
				*enableTokedex = parsed
			}
		}
	}
	if !flagWasSet(flags, "trusted-proxy-cidrs") && os.Getenv("TOKEMON_TRUSTED_PROXY_CIDRS") == "" {
		if value := strings.TrimSpace(values["TOKEMON_TRUSTED_PROXY_CIDRS"]); value != "" {
			*trustedProxyCIDRs = value
		}
	}
}

// applyDashboardTokenFallback implements the --dashboard-token flag's
// documented default: with no dashboard token configured (flag, env, or config
// file) the ingest token governs the dashboard too, so one secret works for
// both ingestion and the web UI. An explicitly empty --dashboard-token keeps
// the dashboard closed.
func applyDashboardTokenFallback(dashboardToken, ingestToken string, flags *flag.FlagSet) string {
	if dashboardToken != "" || ingestToken == "" || flagWasSet(flags, "dashboard-token") {
		return dashboardToken
	}
	return ingestToken
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

func serveHTTP(ctx context.Context, server *http.Server, listener net.Listener, shutdownTimeout time.Duration) error {
	result := make(chan error, 1)
	go func() {
		result <- server.Serve(listener)
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

func configIntEnv(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func isFileBackedDB(path string) bool {
	trimmed := strings.TrimSpace(path)
	return trimmed != "" && !strings.HasPrefix(trimmed, ":") && !strings.HasPrefix(trimmed, "file:")
}

func startAutomatedBackups(ctx context.Context, wg *sync.WaitGroup, logger *slog.Logger, databasePath, backupDir string, retention int, interval time.Duration) {
	if interval <= 0 || strings.TrimSpace(backupDir) == "" || !isFileBackedDB(databasePath) {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		runOnce := func() {
			if ctx.Err() != nil {
				return
			}
			backupCtx, cancel := context.WithTimeout(ctx, automatedBackupTimeout)
			defer cancel()
			result, err := database.CreateBackup(backupCtx, database.BackupOptions{
				DatabasePath:   databasePath,
				DestinationDir: backupDir,
				Retention:      retention,
			})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				logger.Warn("automated_backup_failed", "error", err.Error())
				return
			}
			logger.Info("automated_backup_completed", "path", result.Path, "events", result.EventCount, "tokens", result.LifetimeTokens)
		}

		nextWait := interval
		info, err := database.LatestBackupInfo(databasePath, backupDir)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("automated_backup_inspect_failed", "error", err.Error())
			runOnce()
		} else if !info.Exists || info.Age >= interval {
			runOnce()
		} else {
			nextWait = interval - info.Age
			if nextWait < time.Millisecond {
				nextWait = time.Millisecond
			}
		}

		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(nextWait)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				runOnce()
				timer.Reset(interval)
			}
		}
	}()
}

type statusFreshness struct {
	State       string     `json:"state"`
	Age         string     `json:"age"`
	NewestEvent *time.Time `json:"newest_event,omitempty"`
}

type statusMachine struct {
	Name             string `json:"name"`
	Status           string `json:"status"`
	LastSeenAt       string `json:"last_seen_at,omitempty"`
	TodayTokens      int64  `json:"today_tokens"`
	WeekTokens       int64  `json:"week_tokens"`
	LifetimeTokens   int64  `json:"lifetime_tokens"`
	SourceCount      int    `json:"source_count"`
	SourceErrorCount int    `json:"source_error_count"`
}

type statusMachinesSummary struct {
	Total        int             `json:"total"`
	Connected    int             `json:"connected"`
	Stale        int             `json:"stale"`
	Offline      int             `json:"offline"`
	SourceErrors int             `json:"source_errors"`
	Items        []statusMachine `json:"items"`
}

type statusReport struct {
	Stage           int                   `json:"stage"`
	Form            string                `json:"form"`
	FormName        string                `json:"form_name"`
	Progress        float64               `json:"progress"`
	NextThreshold   *int64                `json:"next_threshold,omitempty"`
	TokensRemaining *int64                `json:"tokens_remaining,omitempty"`
	LifetimeTokens  int64                 `json:"lifetime_tokens"`
	TodayTokens     int64                 `json:"today_tokens"`
	TodayEvents     int64                 `json:"today_events"`
	WeekTokens      int64                 `json:"week_tokens"`
	WeekEvents      int64                 `json:"week_events"`
	Timezone        string                `json:"timezone"`
	Freshness       statusFreshness       `json:"freshness"`
	Machines        statusMachinesSummary `json:"machines"`
}

func runStatus(args []string) error {
	return runStatusWithOutput(args, os.Stdout)
}

func runStatusWithOutput(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := flags.String("config", envOr("TOKEMON_AGENT_CONFIG", ""), "agent dotenv config path (defaults to ~/.config/tokemon/agent.env)")
	serverConfigPath := flags.String("server-config", envOr("TOKEMON_SERVER_CONFIG", ""), "server dotenv config path (defaults to ~/.config/tokemon/server.env)")
	serverURL := flags.String("server", "", "Tokemon server URL")
	token := flags.String("token", "", "authentication token")
	short := flags.Bool("short", false, "print a compact single-line summary")
	jsonOut := flags.Bool("json", false, "print structured JSON output")
	timeout := flags.Duration("timeout", configDurationEnv("TOKEMON_STATUS_TIMEOUT", defaultStatusTimeout), "request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: tokemon status [--config PATH] [--server-config PATH] [--server URL] [--token TOKEN] [--short|--json] [--timeout DURATION]")
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if *short && *jsonOut {
		return errors.New("--short and --json cannot be used together")
	}
	resolvedURL, resolvedToken, err := resolveStatusConfig(flags, *configPath, *serverConfigPath, *serverURL, *token)
	if err != nil {
		return err
	}
	client := localagent.Client{
		ServerURL:      resolvedURL,
		Token:          resolvedToken,
		RequestTimeout: *timeout,
	}
	overview, err := client.Overview(context.Background())
	if err != nil {
		return formatStatusError(err)
	}
	report := buildStatusReport(overview, time.Now().UTC())
	return writeStatusReport(out, report, *short, *jsonOut)
}

func resolveStatusConfig(flags *flag.FlagSet, configPath, serverConfigPath, serverURL, token string) (string, string, error) {
	agentExplicit := flagWasSet(flags, "config") || strings.TrimSpace(os.Getenv("TOKEMON_AGENT_CONFIG")) != ""
	serverExplicit := flagWasSet(flags, "server-config") || strings.TrimSpace(os.Getenv("TOKEMON_SERVER_CONFIG")) != ""

	readAgent := !serverExplicit || agentExplicit
	readServer := !agentExplicit || serverExplicit

	needHome := (readAgent && strings.TrimSpace(configPath) == "") || (readServer && strings.TrimSpace(serverConfigPath) == "")
	home := ""
	if needHome {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
	}

	agentValues := map[string]string{}
	if readAgent {
		if strings.TrimSpace(configPath) == "" {
			configPath = localagent.DefaultConfigPath(home)
		}
		var err error
		agentValues, err = localagent.ReadEnvFile(configPath)
		if err != nil {
			return "", "", err
		}
	}

	serverValues := map[string]string{}
	if readServer {
		if strings.TrimSpace(serverConfigPath) == "" {
			serverConfigPath = localagent.DefaultServerConfigPath(home)
		}
		var err error
		serverValues, err = localagent.ReadEnvFile(serverConfigPath)
		if err != nil {
			return "", "", err
		}
	}

	var resolvedURL string
	switch {
	case flagWasSet(flags, "server"):
		resolvedURL = normalizeServerURL(serverURL)
		if resolvedURL == "" {
			return "", "", errors.New("--server must not be empty")
		}
	case strings.TrimSpace(os.Getenv("TOKEMON_SERVER_URL")) != "":
		resolvedURL = normalizeServerURL(os.Getenv("TOKEMON_SERVER_URL"))
	case strings.TrimSpace(agentValues["TOKEMON_SERVER_URL"]) != "":
		resolvedURL = normalizeServerURL(agentValues["TOKEMON_SERVER_URL"])
	case strings.TrimSpace(os.Getenv("TOKEMON_SERVER_ADDR")) != "":
		resolvedURL = normalizeServerURL(os.Getenv("TOKEMON_SERVER_ADDR"))
	case strings.TrimSpace(serverValues["TOKEMON_SERVER_ADDR"]) != "":
		resolvedURL = normalizeServerURL(serverValues["TOKEMON_SERVER_ADDR"])
	default:
		resolvedURL = "http://127.0.0.1:8080"
	}

	var resolvedToken string
	switch {
	case flagWasSet(flags, "token"):
		resolvedToken = token
	case os.Getenv("TOKEMON_DASHBOARD_TOKEN") != "":
		resolvedToken = os.Getenv("TOKEMON_DASHBOARD_TOKEN")
	case agentValues["TOKEMON_DASHBOARD_TOKEN"] != "":
		resolvedToken = agentValues["TOKEMON_DASHBOARD_TOKEN"]
	case serverValues["TOKEMON_DASHBOARD_TOKEN"] != "":
		resolvedToken = serverValues["TOKEMON_DASHBOARD_TOKEN"]
	case os.Getenv("TOKEMON_INGEST_TOKEN") != "":
		resolvedToken = os.Getenv("TOKEMON_INGEST_TOKEN")
	case agentValues["TOKEMON_INGEST_TOKEN"] != "":
		resolvedToken = agentValues["TOKEMON_INGEST_TOKEN"]
	case serverValues["TOKEMON_INGEST_TOKEN"] != "":
		resolvedToken = serverValues["TOKEMON_INGEST_TOKEN"]
	}

	return resolvedURL, resolvedToken, nil
}

func normalizeServerURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return ""
	}
	if !strings.Contains(trimmed, "://") {
		if host, port, err := net.SplitHostPort(trimmed); err == nil {
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			return "http://" + net.JoinHostPort(host, port)
		}
		return "http://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return trimmed
	}
	hostname := parsed.Hostname()
	if hostname == "0.0.0.0" || hostname == "::" {
		if port := parsed.Port(); port != "" {
			parsed.Host = net.JoinHostPort("127.0.0.1", port)
		} else {
			parsed.Host = "127.0.0.1"
		}
	}
	return strings.TrimRight(parsed.String(), "/")
}

func buildStatusReport(overview database.Overview, now time.Time) statusReport {
	freshness := statusFreshnessFor(overview.NewestEvent, now)
	machines := statusMachinesSummary{
		Total: len(overview.Machines),
		Items: make([]statusMachine, 0, len(overview.Machines)),
	}
	for _, m := range overview.Machines {
		switch m.Status {
		case "Connected":
			machines.Connected++
		case "Stale":
			machines.Stale++
		default:
			machines.Offline++
		}
		machines.SourceErrors += m.SourceErrorCount
		machines.Items = append(machines.Items, statusMachine{
			Name:             m.Name,
			Status:           m.Status,
			LastSeenAt:       m.LastSeenAt,
			TodayTokens:      m.TodayTokens,
			WeekTokens:       m.WeekTokens,
			LifetimeTokens:   m.LifetimeTokens,
			SourceCount:      m.SourceCount,
			SourceErrorCount: m.SourceErrorCount,
		})
	}
	return statusReport{
		Stage:           overview.Evolution.Stage,
		Form:            overview.Evolution.Form,
		FormName:        overview.Evolution.FormName,
		Progress:        overview.Evolution.Progress,
		NextThreshold:   overview.Evolution.NextThreshold,
		TokensRemaining: overview.Evolution.TokensRemaining,
		LifetimeTokens:  overview.LifetimeTokens,
		TodayTokens:     overview.Today.Tokens,
		TodayEvents:     overview.Today.Events,
		WeekTokens:      overview.Week.Tokens,
		WeekEvents:      overview.Week.Events,
		Timezone:        overview.Timezone,
		Freshness:       freshness,
		Machines:        machines,
	}
}

func statusFreshnessFor(newest *time.Time, now time.Time) statusFreshness {
	if newest == nil {
		return statusFreshness{State: "Unknown", Age: "no events yet"}
	}
	age := now.Sub(*newest)
	if age < 0 {
		age = 0
	}
	state := "Stale"
	if age <= 30*time.Minute {
		state = "Fresh"
	}
	utcNewest := newest.UTC()
	return statusFreshness{
		State:       state,
		Age:         statusDurationLabel(age.Milliseconds()) + " ago",
		NewestEvent: &utcNewest,
	}
}

func writeStatusReport(out io.Writer, report statusReport, short, jsonOut bool) error {
	if jsonOut {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	formName := report.FormName
	if strings.TrimSpace(formName) == "" {
		formName = "Unknown"
	}
	if short {
		machineSummary := "0 machines"
		if report.Machines.Total > 0 {
			machineSummary = fmt.Sprintf("%d/%d online", report.Machines.Connected, report.Machines.Total)
		}
		_, err := fmt.Fprintf(out, "Stage %02d %s · %s total · %s today · %s (%s) · %s\n",
			report.Stage, formName,
			formatCompactTokens(report.LifetimeTokens),
			formatCompactTokens(report.TodayTokens),
			report.Freshness.State, report.Freshness.Age,
			machineSummary,
		)
		return err
	}

	stageLine := fmt.Sprintf("Stage %02d %s", report.Stage, formName)
	if report.NextThreshold != nil && report.TokensRemaining != nil {
		stageLine += fmt.Sprintf(" (%.0f%% · %s to %s)", math.Round(report.Progress*100), formatCompactTokens(*report.TokensRemaining), formatCompactTokens(*report.NextThreshold))
	} else {
		stageLine += " (max stage)"
	}
	if _, err := fmt.Fprintln(out, stageLine); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Lifetime: %s (%s)\n", formatCommas(report.LifetimeTokens), formatCompactTokens(report.LifetimeTokens)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Today:    %s (%s, %s events)\n", formatCommas(report.TodayTokens), formatCompactTokens(report.TodayTokens), formatCommas(report.TodayEvents)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Week:     %s (%s, %s events)\n", formatCommas(report.WeekTokens), formatCompactTokens(report.WeekTokens), formatCommas(report.WeekEvents)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Data:     %s · %s\n", report.Freshness.State, report.Freshness.Age); err != nil {
		return err
	}
	if report.Machines.Total == 0 {
		_, err := fmt.Fprintln(out, "Machines: 0")
		return err
	}
	machineLine := fmt.Sprintf("Machines: %d connected, %d stale, %d offline", report.Machines.Connected, report.Machines.Stale, report.Machines.Offline)
	if report.Machines.SourceErrors > 0 {
		machineLine += fmt.Sprintf(" (%d source errors)", report.Machines.SourceErrors)
	}
	if _, err := fmt.Fprintln(out, machineLine); err != nil {
		return err
	}
	for _, m := range report.Machines.Items {
		if _, err := fmt.Fprintf(out, "  - %s: %s · %s today · %s total\n", m.Name, m.Status, formatCompactTokens(m.TodayTokens), formatCompactTokens(m.LifetimeTokens)); err != nil {
			return err
		}
	}
	return nil
}

func formatStatusError(err error) error {
	var statusErr *localagent.StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusUnauthorized:
			return errors.New("authentication failed (401 Unauthorized): check --token, TOKEMON_DASHBOARD_TOKEN, or TOKEMON_INGEST_TOKEN")
		case http.StatusForbidden:
			return errors.New("access forbidden (403 Forbidden): check token or proxy authorization")
		case http.StatusTooManyRequests:
			return errors.New("rate limited (429 Too Many Requests): try again shortly")
		default:
			if statusErr.StatusCode >= http.StatusInternalServerError {
				return fmt.Errorf("server error (%s)", statusErr.Status)
			}
			return fmt.Errorf("request failed (%s)", statusErr.Status)
		}
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("request failed: %w", urlErr.Err)
	}
	return fmt.Errorf("request failed: %w", err)
}

func formatCommas(value int64) string {
	text := strconv.FormatInt(value, 10)
	start := 0
	if strings.HasPrefix(text, "-") {
		start = 1
	}
	first := (len(text) - start) % 3
	if first == 0 {
		first = 3
	}
	var builder strings.Builder
	builder.WriteString(text[:start+first])
	for index := start + first; index < len(text); index += 3 {
		builder.WriteByte(',')
		builder.WriteString(text[index : index+3])
	}
	return builder.String()
}

func formatCompactTokens(value int64) string {
	abs := float64(value)
	if abs < 0 {
		abs = -abs
	}
	for _, unit := range []struct {
		threshold float64
		suffix    string
	}{
		{1_000_000_000_000, "T"},
		{1_000_000_000, "B"},
		{1_000_000, "M"},
		{1_000, "K"},
	} {
		if abs >= unit.threshold {
			return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(value)/unit.threshold), ".0") + unit.suffix
		}
	}
	return strconv.FormatInt(value, 10)
}

func statusDurationLabel(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	seconds := (ms + 500) / 1000
	if seconds < 3600 {
		return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh %02dm", seconds/3600, seconds%3600/60)
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
  status      check server health, evolution stage, and token totals
  passwd      generate a bcrypt hash for the dashboard login password (at most 72 bytes)
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
  tokemon status --short
  tokemon version
  tokemon import --database ./data/tokemon.db usage.jsonl
  tokemon backup create --database ./data/tokemon.db --destination /mnt/off-host/tokemon
  tokemon backup verify /mnt/off-host/tokemon/tokemon-backup-v4-20260810T000000.000000000Z.db
  tokemon inspect usage.jsonl
  tokemon catalog validate --catalog catalog/models.yaml
  tokemon art validate --dir web/static/tokemon`)
}
