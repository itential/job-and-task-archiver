// Command itential-orphan-archiver finds and optionally exports/deletes
// orphaned Itential Platform documents in tasks, job_data, job_data.files,
// and job_data.chunks — records whose parent job no longer exists in the
// jobs collection. It is a companion to job-and-task-archiver, which is
// expected to be the normal path for removing job history; this tool exists
// to clean up records left behind when a job document was removed some other
// way (e.g. a manual deletion, or a legacy script that didn't follow the same
// safe deletion order).
//
// Unlike job-and-task-archiver, this tool does not filter by age or status:
// a record is either orphaned (its parent job is gone) or it isn't. There is
// no "too recent to touch" state to protect, since the parent is already gone.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"gopkg.in/natefinch/lumberjack.v2"
)

// version is set at build time via -ldflags "-X main.version=..." (see Makefile).
// The "dev" default applies to `go build` / `go run` invocations that bypass make.
var version = "dev"

// ----------------------------------------------------------------------------
// Collection names — fixed for Itential Platform job/task data
// ----------------------------------------------------------------------------

const (
	collJobs      = "jobs"
	collTasks     = "tasks"
	collJobData   = "job_data"
	collJobFiles  = "job_data.files"
	collJobChunks = "job_data.chunks"
)

// exportOrder and deleteOrder list the four collections this tool operates
// on. Delete order puts job_data.chunks before job_data.files, matching
// job-and-task-archiver's convention of never leaving a chunk referencing an
// already-deleted file document, even though both are being removed in the
// same run. There is no "jobs last" requirement here — this tool never
// writes to the jobs collection, only reads it for the orphan check.
var exportOrder = []string{collTasks, collJobData, collJobFiles, collJobChunks}
var deleteOrder = []string{collTasks, collJobData, collJobChunks, collJobFiles}

// ----------------------------------------------------------------------------
// Config
// ----------------------------------------------------------------------------

// Config holds all runtime configuration. Fields map 1:1 to CLI flags,
// environment variables (ORPHAN_<UPPER_SNAKE>), and YAML config keys.
type Config struct {
	URI            string
	Database       string
	ReportFile     string // cache of discovered orphan IDs, written after every run
	OutputDir      string // directory where per-collection JSONL files are written
	BatchSize      int
	BatchDelayMS   int
	Export         bool
	Delete         bool
	ReadPreference string

	// ProgressIntervalSecs controls how often discovery logs a heartbeat line
	// while scanning each collection. Each collection's orphan check is a
	// single long-running aggregation with no natural per-document progress
	// signal, so without this a large scan can go silent for minutes at a
	// time — indistinguishable from the process being stuck.
	ProgressIntervalSecs int

	// TLS — leave empty to rely on URI-embedded TLS (e.g. mongodb+srv://).
	// Set TLSCAFile for a custom CA (on-prem), and TLSCertFile+TLSKeyFile for
	// mutual TLS. TLSSkipVerify disables certificate verification (insecure).
	TLSCAFile     string
	TLSCertFile   string
	TLSKeyFile    string
	TLSSkipVerify bool

	// Logging — output is tee'd to stdout and to a rotating file under LogDir.
	// Setting LogDir to "" disables file logging (stdout only).
	LogDir        string
	LogFile       string
	LogMaxSizeMB  int
	LogMaxBackups int
	LogMaxAgeDays int
	LogCompress   bool
}

// initConfig loads configuration in priority order:
//
//  1. Explicit CLI flag value
//  2. Environment variable  (ORPHAN_<KEY>, hyphens → underscores)
//  3. YAML config file      (--config path, or ./orphan-archiver.yaml auto-discovery)
//  4. Built-in default
func initConfig() (*Config, error) {
	v := viper.New()

	// --- Flags ----------------------------------------------------------
	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "itential-orphan-archiver: finds and optionally exports/deletes orphaned tasks, job_data, job_data.files, and job_data.chunks documents whose parent job no longer exists.\n\nUsage:\n")
		pflag.PrintDefaults()
	}

	pflag.Bool("version", false, "Print the version and exit.")
	pflag.String("config", "", "Path to YAML config file (optional; auto-discovers ./orphan-archiver.yaml)")
	pflag.String("uri", "mongodb://localhost:27017", "MongoDB connection URI")
	pflag.String("database", "", "Database name (required)")
	pflag.String("report-file", "orphan-report.json",
		"Path to the orphan report file. Records the discovered orphan IDs per collection for "+
			"post-run inspection. Not read back on the next run — discovery always runs fresh.")
	pflag.String("output-dir", "orphan-exports", "Directory where per-collection JSONL files are written (one file per collection).")
	pflag.Int("batch-size", 1000, "Documents per batch for both export and delete operations.")
	pflag.Int("batch-delay-ms", 100, "Milliseconds to sleep between batches (throttle).")
	pflag.Int("progress-interval-secs", 30,
		"Seconds between discovery heartbeat log lines per collection, so a long scan doesn't go silent. Set to 0 to disable.")
	pflag.Bool("export", true, "Export orphaned documents to the output directory. Set to false to skip export.")
	pflag.Bool("delete", false, "Delete orphaned documents after export. Deletion is skipped unless this flag is set.")
	pflag.String("read-preference", "secondaryPreferred",
		"MongoDB read preference: primary|primaryPreferred|secondary|secondaryPreferred|nearest")
	pflag.String("tls-ca-file", "", "Path to a PEM file containing the CA certificate.")
	pflag.String("tls-cert-file", "", "Path to a PEM file containing the client certificate (mutual TLS).")
	pflag.String("tls-key-file", "", "Path to a PEM file containing the client private key (mutual TLS).")
	pflag.Bool("tls-skip-verify", false, "Disable TLS certificate verification (insecure).")
	pflag.String("log-dir", "/var/log/orphan-archiver",
		"Directory for the rotating log file. Created if it does not exist. "+
			"Set to empty (\"\") to disable file logging and write only to stdout.")
	pflag.String("log-file", "orphan-archiver.log", "Log file name inside --log-dir.")
	pflag.Int("log-max-size-mb", 100, "Maximum size in megabytes of the log file before it is rotated.")
	pflag.Int("log-max-backups", 7, "Maximum number of rotated log files to retain. 0 keeps all.")
	pflag.Int("log-max-age-days", 30, "Maximum age in days to retain rotated log files. 0 keeps all.")
	pflag.Bool("log-compress", true, "Gzip rotated log files.")

	pflag.Parse()

	// Handle --version before any other resolution so users can call it
	// without supplying the otherwise-required flags.
	if vflag := pflag.Lookup("version"); vflag != nil && vflag.Changed {
		fmt.Printf("itential-orphan-archiver %s\n", version)
		os.Exit(0)
	}

	if err := v.BindPFlags(pflag.CommandLine); err != nil {
		return nil, err
	}

	// --- Environment variables ------------------------------------------
	// Example: ORPHAN_DATABASE, ORPHAN_URI, ORPHAN_TLS_CA_FILE
	v.SetEnvPrefix("ORPHAN")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()

	// --- Config file ----------------------------------------------------
	if cf := v.GetString("config"); cf != "" {
		v.SetConfigFile(cf)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config file %q: %w", cf, err)
		}
		log.Printf("Using config file: %s", cf)
	} else {
		v.SetConfigName("orphan-archiver")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		if err := v.ReadInConfig(); err == nil {
			log.Printf("Using config file: %s", v.ConfigFileUsed())
		}
	}

	// CLI flags must always win over config file values. Viper's pflag binding
	// only overrides config when it can detect a flag was explicitly changed,
	// which is unreliable for boolean flags that default to true. Force-setting
	// any changed flag guarantees the correct priority order.
	pflag.CommandLine.VisitAll(func(f *pflag.Flag) {
		if f.Changed {
			v.Set(f.Name, f.Value.String())
		}
	})

	// --- Validation -----------------------------------------------------
	if v.GetString("database") == "" {
		pflag.Usage()
		return nil, fmt.Errorf("--database is required")
	}

	return &Config{
		URI:                  v.GetString("uri"),
		Database:             v.GetString("database"),
		ReportFile:           v.GetString("report-file"),
		OutputDir:            v.GetString("output-dir"),
		BatchSize:            v.GetInt("batch-size"),
		BatchDelayMS:         v.GetInt("batch-delay-ms"),
		ProgressIntervalSecs: v.GetInt("progress-interval-secs"),
		Export:               v.GetBool("export"),
		Delete:               v.GetBool("delete"),
		ReadPreference:       v.GetString("read-preference"),
		TLSCAFile:            v.GetString("tls-ca-file"),
		TLSCertFile:          v.GetString("tls-cert-file"),
		TLSKeyFile:           v.GetString("tls-key-file"),
		TLSSkipVerify:        v.GetBool("tls-skip-verify"),
		LogDir:               v.GetString("log-dir"),
		LogFile:              v.GetString("log-file"),
		LogMaxSizeMB:         v.GetInt("log-max-size-mb"),
		LogMaxBackups:        v.GetInt("log-max-backups"),
		LogMaxAgeDays:        v.GetInt("log-max-age-days"),
		LogCompress:          v.GetBool("log-compress"),
	}, nil
}

// ----------------------------------------------------------------------------
// Logging
// ----------------------------------------------------------------------------

// initLogging configures the standard `log` package to tee its output to
// stdout and a rotating file under cfg.LogDir. Rotation is delegated to
// lumberjack. If cfg.LogDir is empty, file logging is disabled and output
// goes to stdout only.
//
// Returns a closer for the underlying file sink, or nil when file logging
// is disabled. Callers should defer the returned function.
func initLogging(cfg *Config) (func() error, error) {
	if cfg.LogDir == "" {
		log.SetOutput(os.Stdout)
		return nil, nil
	}

	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir %q: %w", cfg.LogDir, err)
	}

	logPath := filepath.Join(cfg.LogDir, cfg.LogFile)
	rotator := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    cfg.LogMaxSizeMB,
		MaxBackups: cfg.LogMaxBackups,
		MaxAge:     cfg.LogMaxAgeDays,
		Compress:   cfg.LogCompress,
		LocalTime:  false,
	}

	log.SetOutput(io.MultiWriter(os.Stdout, rotator))
	log.Printf("Logging to %s (rotate at %d MB, keep %d backups, max age %d days, compress=%v)",
		logPath, cfg.LogMaxSizeMB, cfg.LogMaxBackups, cfg.LogMaxAgeDays, cfg.LogCompress)

	return rotator.Close, nil
}

// ----------------------------------------------------------------------------
// ID helpers
// ----------------------------------------------------------------------------

// idKey returns a stable string representation of a document ID regardless of
// whether it is stored as a BSON ObjectID or a BSON string. Used for sorting
// and for serialising IDs to the report file.
func idKey(id interface{}) string {
	switch v := id.(type) {
	case primitive.ObjectID:
		return v.Hex()
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

// idType returns "objectid" or "string" for the given ID value.
func idType(id interface{}) string {
	if _, ok := id.(primitive.ObjectID); ok {
		return "objectid"
	}
	return "string"
}

// ----------------------------------------------------------------------------
// Orphan report — persists discovered orphan IDs for post-run inspection
// ----------------------------------------------------------------------------

// OrphanReport records the orphaned document IDs found in each collection
// during discovery. Saving it to disk lets an operator inspect exactly what
// was found (and, after a delete run, what was removed) without re-running
// discovery. It is never read back — the next run always discovers fresh.
type OrphanReport struct {
	CreatedAt string              `json:"created_at"`
	Counts    map[string]int      `json:"counts"`
	IDType    map[string]string   `json:"id_type"` // collection -> "objectid" or "string"
	IDs       map[string][]string `json:"ids"`     // collection -> sorted IDs
}

// saveOrphanReport serializes the discovered orphan IDs to path as indented
// JSON. The file is created or overwritten.
func saveOrphanReport(path string, orphans map[string][]interface{}) error {
	report := &OrphanReport{
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Counts:    make(map[string]int),
		IDType:    make(map[string]string),
		IDs:       make(map[string][]string),
	}
	for collection, ids := range orphans {
		report.Counts[collection] = len(ids)
		typ := "objectid"
		if len(ids) > 0 {
			typ = idType(ids[0])
		}
		report.IDType[collection] = typ

		strIDs := make([]string, len(ids))
		for i, id := range ids {
			strIDs[i] = idKey(id)
		}
		sort.Strings(strIDs)
		report.IDs[collection] = strIDs
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0644)
}

// ----------------------------------------------------------------------------
// MongoDB client construction
// ----------------------------------------------------------------------------

// buildMongoClient creates and connects a MongoDB client configured from cfg.
// It applies the read preference, a 30-second server selection timeout, and
// optional TLS settings. For Atlas mongodb+srv:// URIs, TLS is negotiated
// automatically from the SRV record and no TLS flags are needed.
func buildMongoClient(ctx context.Context, cfg *Config) (*mongo.Client, error) {
	rp, err := parseReadPref(cfg.ReadPreference)
	if err != nil {
		return nil, err
	}

	opts := options.Client().
		ApplyURI(cfg.URI).
		SetReadPreference(rp).
		SetServerSelectionTimeout(30 * time.Second)

	// Apply explicit TLS only when the caller has provided TLS material.
	// For Atlas mongodb+srv:// URIs, TLS is negotiated automatically from
	// the SRV record and no extra configuration is needed.
	if cfg.TLSCAFile != "" || cfg.TLSCertFile != "" || cfg.TLSSkipVerify {
		tlsCfg, err := buildTLSConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("tls config: %w", err)
		}
		opts.SetTLSConfig(tlsCfg)
	}

	return mongo.Connect(ctx, opts)
}

// buildTLSConfig constructs a *tls.Config from the TLS-related fields in cfg.
// It supports a custom CA certificate, mutual TLS via a cert/key pair, and
// optional certificate verification skip. Only called when at least one TLS
// flag is set; Atlas connections do not require it.
func buildTLSConfig(cfg *Config) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.TLSSkipVerify, //nolint:gosec — intentional, user-controlled
	}

	if cfg.TLSCAFile != "" {
		caPEM, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("no valid certificates found in %s", cfg.TLSCAFile)
		}
		tlsCfg.RootCAs = pool
	}

	if cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" {
		if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
			return nil, fmt.Errorf("--tls-cert-file and --tls-key-file must be provided together")
		}
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	return tlsCfg, nil
}

// parseReadPref converts a case-insensitive mode string to a MongoDB
// *readpref.ReadPref. An empty string defaults to primary. Returns an error
// for any unrecognized value.
func parseReadPref(mode string) (*readpref.ReadPref, error) {
	switch strings.ToLower(mode) {
	case "primary", "":
		return readpref.Primary(), nil
	case "primarypreferred":
		return readpref.PrimaryPreferred(), nil
	case "secondary":
		return readpref.Secondary(), nil
	case "secondarypreferred":
		return readpref.SecondaryPreferred(), nil
	case "nearest":
		return readpref.Nearest(), nil
	default:
		return nil, fmt.Errorf(
			"unknown read preference %q; valid values: primary, primaryPreferred, secondary, secondaryPreferred, nearest",
			mode,
		)
	}
}

// ----------------------------------------------------------------------------
// MongoDB interfaces — abstractions over the driver types that allow unit
// testing without a real MongoDB connection.
// ----------------------------------------------------------------------------

// CursorAPI is the subset of *mongo.Cursor used by this application.
type CursorAPI interface {
	Next(ctx context.Context) bool
	Decode(v interface{}) error
	Close(ctx context.Context) error
	All(ctx context.Context, results interface{}) error
	Err() error
}

// CollectionAPI is the subset of *mongo.Collection used by this application.
// Unlike job-and-task-archiver, this tool never needs FindOne or
// CountDocuments: discovery is done entirely through Aggregate, and orphan
// counts fall out of the length of the discovered ID slices.
// EstimatedDocumentCount is used only to log the collection's approximate
// size before a scan starts, so operators know roughly what they're waiting
// on — it reads from collection metadata rather than scanning, so it's fast
// even on very large collections.
type CollectionAPI interface {
	Name() string
	Find(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (CursorAPI, error)
	Aggregate(ctx context.Context, pipeline interface{}) (CursorAPI, error)
	DeleteMany(ctx context.Context, filter interface{}) (*mongo.DeleteResult, error)
	EstimatedDocumentCount(ctx context.Context) (int64, error)
}

// DatabaseAPI is the subset of *mongo.Database used by this application.
type DatabaseAPI interface {
	Collection(name string) CollectionAPI
}

// mongoCollection wraps *mongo.Collection to satisfy CollectionAPI.
type mongoCollection struct{ coll *mongo.Collection }

// Name returns the name of the underlying MongoDB collection.
func (c *mongoCollection) Name() string { return c.coll.Name() }

// Find executes a find query against the collection and returns a cursor over
// the matching documents.
func (c *mongoCollection) Find(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (CursorAPI, error) {
	return c.coll.Find(ctx, filter, opts...)
}

// Aggregate runs an aggregation pipeline against the collection and returns a
// cursor over the result documents.
func (c *mongoCollection) Aggregate(ctx context.Context, pipeline interface{}) (CursorAPI, error) {
	return c.coll.Aggregate(ctx, pipeline)
}

// DeleteMany deletes all documents matching filter from the collection and
// returns the count of deleted documents.
func (c *mongoCollection) DeleteMany(ctx context.Context, filter interface{}) (*mongo.DeleteResult, error) {
	return c.coll.DeleteMany(ctx, filter)
}

// EstimatedDocumentCount returns the approximate number of documents in the
// collection, read from collection metadata rather than a scan.
func (c *mongoCollection) EstimatedDocumentCount(ctx context.Context) (int64, error) {
	return c.coll.EstimatedDocumentCount(ctx)
}

// mongoDatabase wraps *mongo.Database to satisfy DatabaseAPI.
type mongoDatabase struct{ db *mongo.Database }

// Collection returns a CollectionAPI wrapping the named MongoDB collection.
func (d *mongoDatabase) Collection(name string) CollectionAPI {
	return &mongoCollection{coll: d.db.Collection(name)}
}

// ----------------------------------------------------------------------------
// Orphan discovery
// ----------------------------------------------------------------------------

// directOrphanPipeline builds an aggregation pipeline that finds documents
// whose localField does not match any document's _id in the jobs collection.
// Used for tasks (job._id), job_data (job_id), and job_data.files
// (metadata.job) — all three reference a job ID directly.
func directOrphanPipeline(localField string) bson.A {
	return bson.A{
		bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: collJobs},
			{Key: "localField", Value: localField},
			{Key: "foreignField", Value: "_id"},
			{Key: "as", Value: "_job"},
		}}},
		bson.D{{Key: "$match", Value: bson.D{{Key: "_job", Value: bson.D{{Key: "$size", Value: 0}}}}}},
		bson.D{{Key: "$project", Value: bson.D{{Key: "_id", Value: 1}}}},
	}
}

// chunksOrphanPipeline builds an aggregation pipeline that finds
// job_data.chunks documents whose files_id does not resolve to a "valid"
// job_data.files document — one that both exists and whose own metadata.job
// still points to a live job. This single query captures both orphan cases
// for chunks: a files_id with no job_data.files document at all (dangling),
// and a files_id whose job_data.files document exists but is itself already
// orphaned (its job is gone). files_id references job_data.files._id, not a
// job ID directly, so this needs a nested $lookup rather than the direct
// pattern used for the other three collections.
func chunksOrphanPipeline() bson.A {
	return bson.A{
		bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: collJobFiles},
			{Key: "let", Value: bson.D{{Key: "fid", Value: "$files_id"}}},
			{Key: "pipeline", Value: bson.A{
				bson.D{{Key: "$match", Value: bson.D{
					{Key: "$expr", Value: bson.D{{Key: "$eq", Value: bson.A{"$_id", "$$fid"}}}},
				}}},
				bson.D{{Key: "$lookup", Value: bson.D{
					{Key: "from", Value: collJobs},
					{Key: "localField", Value: "metadata.job"},
					{Key: "foreignField", Value: "_id"},
					{Key: "as", Value: "_job"},
				}}},
				bson.D{{Key: "$match", Value: bson.D{{Key: "_job", Value: bson.D{{Key: "$ne", Value: bson.A{}}}}}}},
				bson.D{{Key: "$limit", Value: 1}},
			}},
			{Key: "as", Value: "_validFile"},
		}}},
		bson.D{{Key: "$match", Value: bson.D{{Key: "_validFile", Value: bson.D{{Key: "$size", Value: 0}}}}}},
		bson.D{{Key: "$project", Value: bson.D{{Key: "_id", Value: 1}}}},
	}
}

// aggregateIDs runs an aggregation pipeline against coll and returns the _id
// values of each result document.
//
// This is a single long-running server-side scan with no natural per-document
// progress signal — a large collection can go silent for minutes between the
// call starting and the first result arriving. When heartbeat > 0, a ticker
// logs an elapsed-time / matches-found-so-far line every heartbeat interval
// for the full duration of the call (including the initial wait for the first
// batch), so the operator can tell the process is still working rather than
// stuck or hung on a dead connection. Pass heartbeat <= 0 to disable it.
func aggregateIDs(ctx context.Context, coll CollectionAPI, pipeline bson.A, heartbeat time.Duration) ([]interface{}, error) {
	var found int64
	start := time.Now()

	if heartbeat > 0 {
		done := make(chan struct{})
		defer close(done)
		go func() {
			ticker := time.NewTicker(heartbeat)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					log.Printf("  %s: still scanning... %s elapsed, %d orphans found so far",
						coll.Name(), time.Since(start).Round(time.Second), atomic.LoadInt64(&found))
				}
			}
		}()
	}

	cur, err := coll.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var ids []interface{}
	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		ids = append(ids, doc["_id"])
		atomic.AddInt64(&found, 1)
	}
	return ids, cur.Err()
}

// discoverOrphans runs the orphan-detection aggregation for each of the four
// collections and returns the discovered IDs keyed by collection name. Each
// collection is queried independently — there is no dependency between them,
// since chunksOrphanPipeline resolves job_data.files validity itself rather
// than depending on a separately-discovered orphan file ID list.
//
// This performs a full collection scan on each of the four collections: there
// is no field that flags a document as orphaned ahead of time, so every
// document must be checked. The $lookup joins themselves are indexed (against
// jobs._id and job_data.files._id, both primary keys), but the base scan is
// not avoidable. Consider running this during low-traffic windows and against
// a secondary (the default --read-preference).
//
// Before each collection's scan, its estimated document count is logged (a
// fast, metadata-only read) so the operator knows roughly what they're
// waiting on. progressIntervalSecs controls the heartbeat frequency during
// the scan itself; pass 0 to disable it.
func discoverOrphans(ctx context.Context, db DatabaseAPI, progressIntervalSecs int) (map[string][]interface{}, error) {
	specs := []struct {
		collection string
		pipeline   bson.A
	}{
		{collTasks, directOrphanPipeline("job._id")},
		{collJobData, directOrphanPipeline("job_id")},
		{collJobFiles, directOrphanPipeline("metadata.job")},
		{collJobChunks, chunksOrphanPipeline()},
	}

	heartbeat := time.Duration(progressIntervalSecs) * time.Second

	result := make(map[string][]interface{}, len(specs))
	for _, spec := range specs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		coll := db.Collection(spec.collection)
		if n, err := coll.EstimatedDocumentCount(ctx); err != nil {
			log.Printf("  %s: could not estimate document count: %v", spec.collection, err)
		} else {
			log.Printf("  %s: ~%d documents to scan", spec.collection, n)
		}

		t0 := time.Now()
		ids, err := aggregateIDs(ctx, coll, spec.pipeline, heartbeat)
		if err != nil {
			return nil, fmt.Errorf("discover orphans in %s: %w", spec.collection, err)
		}
		log.Printf("Discovery: %d orphaned documents found in %s  (%s)",
			len(ids), spec.collection, time.Since(t0).Round(time.Second))
		result[spec.collection] = ids
	}

	return result, nil
}

// ----------------------------------------------------------------------------
// Export
// ----------------------------------------------------------------------------

// exportCollection fetches full documents from coll where "_id" is in ids and
// writes them as JSONL to filePath (one document per line, no schema
// modifications). The file is created lazily — if no documents match, no file
// is created. Returns the number of documents written.
func exportCollection(
	ctx context.Context,
	coll CollectionAPI,
	ids []interface{},
	cfg *Config,
	filePath string,
) (int64, error) {
	batchDelay := time.Duration(cfg.BatchDelayMS) * time.Millisecond
	total := len(ids)
	collName := coll.Name()
	var written int64

	// File is opened lazily on first write so that collections with no
	// matching documents do not produce empty files.
	var f *os.File
	var writer *bufio.Writer

	for i := 0; i < total; i += cfg.BatchSize {
		if ctx.Err() != nil {
			return written, ctx.Err()
		}

		end := i + cfg.BatchSize
		if end > total {
			end = total
		}
		batch := ids[i:end]

		t0 := time.Now()
		cur, err := coll.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: batch}}}})
		if err != nil {
			return written, fmt.Errorf("find export batch [%d:%d]: %w", i, end, err)
		}
		var docs []bson.M
		if err := cur.All(ctx, &docs); err != nil {
			_ = cur.Close(ctx)
			return written, fmt.Errorf("read export batch [%d:%d]: %w", i, end, err)
		}
		_ = cur.Close(ctx)
		queryDur := time.Since(t0).Round(time.Millisecond)

		if len(docs) > 0 && f == nil {
			f, err = os.OpenFile(filePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
			if err != nil {
				return written, fmt.Errorf("open output file %s: %w", filePath, err)
			}
			defer f.Close()
			writer = bufio.NewWriterSize(f, 1<<20)
			defer writer.Flush()
		}

		for _, doc := range docs {
			line, err := json.Marshal(doc)
			if err != nil {
				return written, fmt.Errorf("marshal doc: %w", err)
			}
			if _, err := writer.Write(append(line, '\n')); err != nil {
				return written, fmt.Errorf("write doc: %w", err)
			}
		}
		if writer != nil {
			if err := writer.Flush(); err != nil {
				return written, fmt.Errorf("flush batch [%d:%d]: %w", i, end, err)
			}
		}

		written += int64(len(docs))
		log.Printf("Export [%s] batch [%d–%d]: found=%d  query=%s  total_written=%d",
			collName, i, end-1, len(docs), queryDur, written)

		if batchDelay > 0 {
			select {
			case <-ctx.Done():
				return written, ctx.Err()
			case <-time.After(batchDelay):
			}
		}
	}
	return written, nil
}

// runOrphanExport exports all four orphan collections in exportOrder to
// per-collection JSONL files under cfg.OutputDir. Returns the total number of
// documents written.
func runOrphanExport(ctx context.Context, db DatabaseAPI, orphans map[string][]interface{}, cfg *Config) (int64, error) {
	if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
		return 0, fmt.Errorf("create output directory %s: %w", cfg.OutputDir, err)
	}

	var totalWritten int64
	for _, collection := range exportOrder {
		if ctx.Err() != nil {
			return totalWritten, ctx.Err()
		}

		ids := orphans[collection]
		if len(ids) == 0 {
			log.Printf("Exporting collection: %s — 0 orphaned documents, skipping", collection)
			continue
		}

		log.Printf("Exporting collection: %s (%d orphaned documents)", collection, len(ids))
		filePath := fmt.Sprintf("%s/%s.jsonl", cfg.OutputDir, collection)
		n, err := exportCollection(ctx, db.Collection(collection), ids, cfg, filePath)
		totalWritten += n
		if err != nil {
			return totalWritten, err
		}
	}
	return totalWritten, nil
}

// ----------------------------------------------------------------------------
// Delete
// ----------------------------------------------------------------------------

// batchDelete deletes all documents from coll whose _id is in successive
// batches of ids, throttled by cfg.BatchDelayMS between batches.
func batchDelete(ctx context.Context, coll CollectionAPI, ids []interface{}, cfg *Config) (int64, error) {
	batchDelay := time.Duration(cfg.BatchDelayMS) * time.Millisecond
	var totalDeleted int64
	totalBatches := (len(ids) + cfg.BatchSize - 1) / cfg.BatchSize
	collName := coll.Name()

	for i := 0; i < len(ids); i += cfg.BatchSize {
		if ctx.Err() != nil {
			return totalDeleted, ctx.Err()
		}

		end := i + cfg.BatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		batchNum := i/cfg.BatchSize + 1

		t0 := time.Now()
		filter := bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: batch}}}}
		res, err := coll.DeleteMany(ctx, filter)
		if err != nil {
			return totalDeleted, fmt.Errorf("batch [%d:%d]: %w", i, end, err)
		}
		totalDeleted += res.DeletedCount

		log.Printf("  %s batch %d/%d: deleted %d  (%s)  total_deleted=%d",
			collName, batchNum, totalBatches, res.DeletedCount,
			time.Since(t0).Round(time.Millisecond), totalDeleted)

		if batchDelay > 0 {
			select {
			case <-ctx.Done():
				return totalDeleted, ctx.Err()
			case <-time.After(batchDelay):
			}
		}
	}

	return totalDeleted, nil
}

// deleteAllOrphans deletes the discovered orphan documents from all four
// collections in deleteOrder (chunks before files) and returns the deleted
// count per collection.
func deleteAllOrphans(ctx context.Context, db DatabaseAPI, orphans map[string][]interface{}, cfg *Config) (map[string]int64, error) {
	totals := make(map[string]int64)

	for _, collection := range deleteOrder {
		ids := orphans[collection]
		if len(ids) == 0 {
			log.Printf("Deleting from %s — 0 orphaned documents, skipping", collection)
			continue
		}

		if ctx.Err() != nil {
			return totals, ctx.Err()
		}

		log.Printf("Deleting from %s...", collection)
		t0 := time.Now()
		n, err := batchDelete(ctx, db.Collection(collection), ids, cfg)
		totals[collection] = n
		if err != nil {
			return totals, fmt.Errorf("delete %s: %w", collection, err)
		}
		log.Printf("  %s: deleted %d documents  (%s)", collection, n, time.Since(t0).Round(time.Second))
	}

	return totals, nil
}

// ----------------------------------------------------------------------------
// Main
// ----------------------------------------------------------------------------

// main is the entry point for itential-orphan-archiver. It loads
// configuration, connects to MongoDB, discovers orphaned documents in tasks,
// job_data, job_data.files, and job_data.chunks, and then optionally exports
// and deletes them.
func main() {
	start := time.Now()

	cfg, err := initConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	closeLog, err := initLogging(cfg)
	if err != nil {
		log.Fatalf("logging: %v", err)
	}
	if closeLog != nil {
		defer func() { _ = closeLog() }()
	}

	log.Printf("itential-orphan-archiver %s", version)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if !cfg.Export {
		log.Println("Export disabled (export=false in config, env, or flag)")
	}
	if !cfg.Delete {
		log.Println("Delete disabled — set delete=true in config, ORPHAN_DELETE=true, or pass --delete to enable")
	}

	client, err := buildMongoClient(ctx, cfg)
	if err != nil {
		log.Fatalf("mongo client: %v", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	if err := client.Ping(ctx, nil); err != nil {
		log.Fatalf("ping: %v", err)
	}

	db := &mongoDatabase{db: client.Database(cfg.Database)}

	log.Printf("Connected to %s  |  read-preference: %s", cfg.Database, cfg.ReadPreference)

	// --- Discover orphaned documents ---------------------------------------
	//
	// Discovery always runs fresh every invocation, exactly like
	// job-and-task-archiver. The report file is written after discovery for
	// inspection, but it is never read back — the next run always
	// re-discovers.
	log.Println("Discovering orphaned documents — this scans all four collections in full...")

	orphans, err := discoverOrphans(ctx, db, cfg.ProgressIntervalSecs)
	if err != nil {
		log.Fatalf("discover: %v", err)
	}

	// Sort each collection's IDs for deterministic ordering, matching
	// job-and-task-archiver's approach for stable export checkpointing.
	for collection, ids := range orphans {
		sort.Slice(ids, func(i, j int) bool { return idKey(ids[i]) < idKey(ids[j]) })
		orphans[collection] = ids
	}

	if err := saveOrphanReport(cfg.ReportFile, orphans); err != nil {
		log.Printf("WARN: could not save orphan report: %v", err)
	} else {
		log.Printf("Saved orphan report to %s", cfg.ReportFile)
	}

	var total int
	for _, collection := range exportOrder {
		n := len(orphans[collection])
		total += n
		log.Printf("  %-25s %d orphaned documents", collection, n)
	}
	log.Printf("  %-25s %d orphaned documents total", "TOTAL", total)

	if total == 0 {
		log.Println("No orphaned documents found.")
		return
	}

	// --- Export -------------------------------------------------------------
	if !cfg.Export {
		log.Println("Skipping export phase (--export=false)")
	} else {
		exported, err := runOrphanExport(ctx, db, orphans, cfg)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				log.Printf("Interrupted during export after %d documents written", exported)
				return
			}
			log.Fatalf("export: %v", err)
		}
		log.Printf("Export complete: %d documents written", exported)
	}

	// --- Delete ---------------------------------------------------------
	if !cfg.Delete {
		log.Println("Delete disabled — set delete=true in config, ORPHAN_DELETE=true, or pass --delete to remove documents")
		log.Printf("Total runtime: %s", time.Since(start).Round(time.Second))
		return
	}

	totals, err := deleteAllOrphans(ctx, db, orphans, cfg)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Printf("Interrupted during delete — re-run to continue (discovery and delete are idempotent)")
			return
		}
		log.Fatalf("delete: %v", err)
	}

	log.Printf("Delete complete: tasks=%d  job_data=%d  job_data.files=%d  job_data.chunks=%d",
		totals[collTasks], totals[collJobData], totals[collJobFiles], totals[collJobChunks])
	log.Printf("Total runtime: %s", time.Since(start).Round(time.Second))
}
