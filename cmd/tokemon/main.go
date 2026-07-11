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
	"path/filepath"
	"strings"

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
		return errors.New("command required; try: tokemon serve, import, export, inspect, discover, or purge")
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
		return errors.New("agent polling is the next implementation slice; use discover and import while the ingestion foundation is being built")
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", ":8080", "HTTP listen address")
	databasePath := flags.String("database", defaultDatabase, "SQLite database path")
	ingestToken := flags.String("ingest-token", os.Getenv("TOKEMON_INGEST_TOKEN"), "shared token for event ingestion")
	catalogPath := flags.String("catalog", "catalog/models.yaml", "model catalog YAML path")
	if err := flags.Parse(args); err != nil {
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

func displayHome(path, home string) string {
	return strings.Replace(path, home, "~", 1)
}

func printUsage() {
	fmt.Println(`tokemon — local-first token analytics

Commands:
  serve       start the HTTP API and dashboard
  import      ingest normalized JSONL into SQLite
  export      export normalized JSONL from SQLite
  inspect     validate and print the outgoing JSONL payload
  discover    report supported local tool locations
  purge       delete events before a date

Examples:
  tokemon serve --database ./data/tokemon.db
  tokemon import --database ./data/tokemon.db usage.jsonl
  tokemon inspect usage.jsonl`)
}
