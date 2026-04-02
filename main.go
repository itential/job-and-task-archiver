package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

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

// deleteTargets lists the collections that can be deleted using job IDs as a
// direct filter. Jobs are deleted last so their IDs remain discoverable if the
// process is interrupted and re-run.
//
// job_data.files and job_data.chunks are NOT in this list: chunks must be
// deleted via the _id values of their parent job_data.files documents
// (files_id references job_data.files._id, not the job ID). Those two
// collections are handled as a pair by deleteGridFS.
var deleteTargets = []struct {
	collection string
	filterKey  string
}{
	{collTasks, "job._id"},
	{collJobData, "job_id"},
	// job_data.files and job_data.chunks handled by deleteGridFS
	{collJobs, "_id"},
}

// ----------------------------------------------------------------------------
// Config
// ----------------------------------------------------------------------------

// Config holds all runtime configuration. Fields map 1:1 to CLI flags,
// environment variables (ARCHIVER_<UPPER_SNAKE>), and YAML config keys.
type Config struct {
	URI            string
	Database       string
	CutoffDays     int
	IDsFile   string // cache of discovered job IDs shared across phases
	OutputDir string // directory where per-collection JSONL files are written
	BatchSize      int
	BatchDelayMS   int
	Export         bool
	Delete         bool
	SkipCount      bool
	ReadPreference string

	// TLS — leave empty to rely on URI-embedded TLS (e.g. mongodb+srv://).
	// Set TLSCAFile for a custom CA (on-prem), and TLSCertFile+TLSKeyFile for
	// mutual TLS. TLSSkipVerify disables certificate verification (insecure).
	TLSCAFile     string
	TLSCertFile   string
	TLSKeyFile    string
	TLSSkipVerify bool
}

// initConfig loads configuration in priority order:
//
//  1. Explicit CLI flag value
//  2. Environment variable  (ARCHIVER_<KEY>, hyphens → underscores)
//  3. YAML config file      (--config path, or ./archiver.yaml auto-discovery)
//  4. Built-in default
func initConfig() (*Config, error) {
	v := viper.New()

	// --- Flags ----------------------------------------------------------
	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "job-and-task-archiver: exports and optionally deletes job and task history from an Itential Platform MongoDB database using a given cutoff date.\n\nUsage:\n")
		pflag.PrintDefaults()
	}

	pflag.String("config", "", "Path to YAML config file (optional; auto-discovers ./archiver.yaml)")
	pflag.String("uri", "mongodb://localhost:27017", "MongoDB connection URI")
	pflag.String("database", "", "Database name (required)")
	pflag.Int("cutoff-days", 0, "Delete jobs completed or canceled more than this many days ago (required)")
	pflag.String("ids-file", "job-ids.json",
		"Path to the job IDs cache file. Stores discovered IDs so both the export "+
			"and delete phases operate on the exact same set of jobs. Delete this file to force re-discovery.")
	pflag.String("output-dir", "exports", "Directory where per-collection JSONL files are written (one file per collection).")
	pflag.Int("batch-size", 1000, "Documents per batch for both export and delete operations.")
	pflag.Int("batch-delay-ms", 100, "Milliseconds to sleep between batches (throttle).")
	pflag.Bool("export", true, "Export job documents to the output file. Set to false to skip export.")
	pflag.Bool("delete", false, "Delete documents after export. Deletion is skipped unless this flag is set.")
	pflag.Bool("skip-count", false, "Skip the per-collection document count summary after discovery.")
	pflag.String("read-preference", "secondaryPreferred",
		"MongoDB read preference: primary|primaryPreferred|secondary|secondaryPreferred|nearest")
	pflag.String("tls-ca-file", "", "Path to a PEM file containing the CA certificate.")
	pflag.String("tls-cert-file", "", "Path to a PEM file containing the client certificate (mutual TLS).")
	pflag.String("tls-key-file", "", "Path to a PEM file containing the client private key (mutual TLS).")
	pflag.Bool("tls-skip-verify", false, "Disable TLS certificate verification (insecure).")

	pflag.Parse()

	if err := v.BindPFlags(pflag.CommandLine); err != nil {
		return nil, err
	}

	// --- Environment variables ------------------------------------------
	// Example: ARCHIVER_DATABASE, ARCHIVER_CUTOFF_DAYS, ARCHIVER_TLS_CA_FILE
	v.SetEnvPrefix("ARCHIVER")
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
		v.SetConfigName("archiver")
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
	if v.GetInt("cutoff-days") <= 0 {
		pflag.Usage()
		return nil, fmt.Errorf("--cutoff-days must be a positive integer")
	}

	return &Config{
		URI:            v.GetString("uri"),
		Database:       v.GetString("database"),
		CutoffDays:     v.GetInt("cutoff-days"),
		IDsFile:        v.GetString("ids-file"),
		OutputDir: v.GetString("output-dir"),
		BatchSize:      v.GetInt("batch-size"),
		BatchDelayMS:   v.GetInt("batch-delay-ms"),
		Export:         v.GetBool("export"),
		Delete:         v.GetBool("delete"),
		SkipCount:      v.GetBool("skip-count"),
		ReadPreference: v.GetString("read-preference"),
		TLSCAFile:      v.GetString("tls-ca-file"),
		TLSCertFile:    v.GetString("tls-cert-file"),
		TLSKeyFile:     v.GetString("tls-key-file"),
		TLSSkipVerify:  v.GetBool("tls-skip-verify"),
	}, nil
}

// ----------------------------------------------------------------------------
// ID helpers
// ----------------------------------------------------------------------------

// idKey returns a stable string representation of a job ID regardless of
// whether it is stored as a BSON ObjectID or a BSON string. Used for
// sorting and for serialising IDs to the cache file.
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
// ID cache — persists discovered job IDs across export and delete phases
// ----------------------------------------------------------------------------

// IDCache stores the list of job IDs discovered during the find phase.
// Saving it to disk allows the export and delete phases — even when run as
// separate commands — to operate on exactly the same set of documents.
// Delete the cache file to force a fresh discovery run.
//
// IDType records whether the IDs are stored as BSON ObjectIDs or plain
// strings in MongoDB; this is needed to reconstruct the correct Go type on
// load so that $in filters are sent with the matching BSON type.
type IDCache struct {
	CutoffMS  int64    `json:"cutoff_ms"`
	IDType    string   `json:"id_type"`  // "objectid" or "string"
	JobIDs    []string `json:"job_ids"`  // sorted; hex for ObjectIDs, raw for strings
	CreatedAt string   `json:"created_at"`
}


// saveIDCache serializes cache to path as indented JSON, setting CreatedAt to
// the current UTC time before writing. The file is created or overwritten.
func saveIDCache(path string, cache *IDCache) error {
	cache.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
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

// SingleResultAPI is the subset of *mongo.SingleResult used by this application.
type SingleResultAPI interface {
	Decode(v interface{}) error
}

// CollectionAPI is the subset of *mongo.Collection used by this application.
type CollectionAPI interface {
	Name() string
	Find(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (CursorAPI, error)
	FindOne(ctx context.Context, filter interface{}) SingleResultAPI
	DeleteMany(ctx context.Context, filter interface{}) (*mongo.DeleteResult, error)
	CountDocuments(ctx context.Context, filter interface{}) (int64, error)
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

// FindOne executes a find query and returns a single result for the first
// matching document.
func (c *mongoCollection) FindOne(ctx context.Context, filter interface{}) SingleResultAPI {
	return c.coll.FindOne(ctx, filter)
}

// DeleteMany deletes all documents matching filter from the collection and
// returns the count of deleted documents.
func (c *mongoCollection) DeleteMany(ctx context.Context, filter interface{}) (*mongo.DeleteResult, error) {
	return c.coll.DeleteMany(ctx, filter)
}

// CountDocuments returns the number of documents in the collection that match
// filter.
func (c *mongoCollection) CountDocuments(ctx context.Context, filter interface{}) (int64, error) {
	return c.coll.CountDocuments(ctx, filter)
}

// mongoDatabase wraps *mongo.Database to satisfy DatabaseAPI.
type mongoDatabase struct{ db *mongo.Database }

// Collection returns a CollectionAPI wrapping the named MongoDB collection.
func (d *mongoDatabase) Collection(name string) CollectionAPI {
	return &mongoCollection{coll: d.db.Collection(name)}
}

// ----------------------------------------------------------------------------
// Job ID discovery
// ----------------------------------------------------------------------------

// discoverJobIDs finds all job IDs eligible for deletion using a two-phase
// query that matches the logic in the reference Node.js delete-jobs script.
//
// Phase 1: Find parent jobs that are completed or canceled and older than the
// cutoff. Parent jobs are identified by having exactly one ancestor — themselves.
//
// Phase 2: Expand to all jobs (parents + children) whose first ancestor matches
// any of the parent IDs found in phase 1. This captures child jobs that may
// not individually meet the age/status criteria but belong to an eligible parent.
//
// metrics.end_time is stored as milliseconds since epoch (not a BSON date).
//
// IDs are returned as []interface{} where each element is either a
// primitive.ObjectID or a string, matching the actual BSON type stored in
// MongoDB. This preserves the correct type for $in filters: passing the wrong
// BSON type causes MongoDB to return 0 results silently.
//
// Phase 2 is batched (1000 parent IDs per query) to avoid oversized $in clauses,
// and detects whether ancestors values are stored as BSON ObjectIDs or hex strings
// by sampling one parent document — Itential versions differ on this.
func discoverJobIDs(ctx context.Context, db DatabaseAPI, cutoffMS int64) ([]interface{}, error) {
	const phase2Batch = 1000

	jobs := db.Collection(collJobs)

	// Phase 1: find completed/canceled parent jobs older than the cutoff.
	log.Println("Phase 1: Finding completed and canceled parent jobs older than the cutoff...")
	t0 := time.Now()
	parentIDs, err := findIDs(ctx, jobs, bson.D{
		{Key: "$and", Value: bson.A{
			bson.D{{Key: "metrics.end_time", Value: bson.D{{Key: "$lt", Value: cutoffMS}}}},
			bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"complete", "canceled"}}}}},
			bson.D{{Key: "ancestors", Value: bson.D{{Key: "$size", Value: 1}}}},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("find parent jobs: %w", err)
	}
	log.Printf("Phase 1: %d parent jobs found in %s", len(parentIDs), time.Since(t0).Round(time.Millisecond))
	if len(parentIDs) == 0 {
		return nil, nil
	}

	// Detect whether ancestors values are stored as BSON ObjectIDs or hex strings.
	useStrings, err := ancestorsStoredAsStrings(ctx, jobs, cutoffMS)
	if err != nil {
		return nil, fmt.Errorf("inspect ancestors field type: %w", err)
	}

	// Phase 2: expand to all child jobs that belong to an eligible parent.
	totalBatches := (len(parentIDs) + phase2Batch - 1) / phase2Batch
	log.Printf("Phase 2: Expanding to all child jobs across %d batches of %d parent IDs...", totalBatches, phase2Batch)

	// primitive.ObjectID and string are both comparable, so interface{} works as
	// a map key here.
	idSet := make(map[interface{}]struct{})

	for i := 0; i < len(parentIDs); i += phase2Batch {
		end := i + phase2Batch
		if end > len(parentIDs) {
			end = len(parentIDs)
		}
		batch := parentIDs[i:end]

		// Build the ancestors.0 $in values in the type that MongoDB expects.
		ancestorValues := make([]interface{}, len(batch))
		for j, id := range batch {
			if useStrings {
				// ancestors stores hex strings — send the string representation.
				ancestorValues[j] = idKey(id)
			} else {
				// ancestors stores BSON ObjectIDs — send an ObjectID.
				switch v := id.(type) {
				case primitive.ObjectID:
					ancestorValues[j] = v
				case string:
					oid, parseErr := primitive.ObjectIDFromHex(v)
					if parseErr != nil {
						return nil, fmt.Errorf("convert string ID to ObjectID for phase 2: %w", parseErr)
					}
					ancestorValues[j] = oid
				default:
					return nil, fmt.Errorf("unexpected ID type in phase 2 batch: %T", id)
				}
			}
		}

		filter := bson.D{{Key: "ancestors.0", Value: bson.D{{Key: "$in", Value: ancestorValues}}}}

		batchNum := i/phase2Batch + 1
		t1 := time.Now()
		batchIDs, err := findIDs(ctx, jobs, filter)
		if err != nil {
			return nil, fmt.Errorf("find child jobs (batch %d): %w", batchNum, err)
		}
		for _, id := range batchIDs {
			idSet[id] = struct{}{}
		}
		log.Printf("Phase 2 batch %d/%d: %d jobs found in %s  (running total: %d jobs / %d parents scanned)",
			batchNum, totalBatches, len(batchIDs), time.Since(t1).Round(time.Millisecond), len(idSet), len(parentIDs))
	}

	allIDs := make([]interface{}, 0, len(idSet))
	for id := range idSet {
		allIDs = append(allIDs, id)
	}

	return allIDs, nil
}

// ancestorsStoredAsStrings samples one parent job document to determine whether
// the ancestors array holds hex strings or native BSON ObjectIDs. It re-runs
// the Phase 1 filter with a limit of 1 rather than fetching by _id, which
// avoids a "no documents" error when secondaryPreferred routes the two queries
// to different replica set members.
func ancestorsStoredAsStrings(ctx context.Context, coll CollectionAPI, cutoffMS int64) (bool, error) {
	var doc bson.M
	err := coll.FindOne(ctx, bson.D{
		{Key: "$and", Value: bson.A{
			bson.D{{Key: "metrics.end_time", Value: bson.D{{Key: "$lt", Value: cutoffMS}}}},
			bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"complete", "canceled"}}}}},
			bson.D{{Key: "ancestors", Value: bson.D{{Key: "$size", Value: 1}}}},
		}},
	}).Decode(&doc)
	if err != nil {
		return false, err
	}
	ancestors, ok := doc["ancestors"].(bson.A)
	if !ok || len(ancestors) == 0 {
		return false, nil
	}
	_, isString := ancestors[0].(string)
	return isString, nil
}

// findIDs runs a find query and returns only the _id values of matching
// documents, decoding each into a bson.M so the actual BSON type is
// preserved. Callers receive either primitive.ObjectID or string values
// depending on how the collection stores _id — passing the wrong type to
// a $in filter causes MongoDB to return 0 results without an error.
func findIDs(ctx context.Context, coll CollectionAPI, filter bson.D) ([]interface{}, error) {
	cur, err := coll.Find(ctx, filter,
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}),
	)
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
	}
	return ids, cur.Err()
}

// ----------------------------------------------------------------------------
// Export
// ----------------------------------------------------------------------------

// exportTarget describes one collection to be exported.
type exportTarget struct {
	collection string
	filterKey  string
	ids        []interface{}
}

// exportCollection fetches full documents from coll where filterKey is in ids
// and writes them as JSONL to filePath (one document per line, no schema
// modifications). The file is created lazily — if no documents match, no file
// is created. Returns the number of documents written.
func exportCollection(
	ctx context.Context,
	coll CollectionAPI,
	filterKey string,
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
		cur, err := coll.Find(ctx, bson.D{{Key: filterKey, Value: bson.D{{Key: "$in", Value: batch}}}})
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

// runExport exports all five Itential job/task collections in order:
// tasks → job_data → job_data.files → job_data.chunks → jobs.
//
// fileIDs are the _id values of job_data.files documents (resolved from job IDs
// by findFileIDs). They are needed because job_data.chunks.files_id references
// job_data.files._id, not the job ID.
//
// Returns the total number of documents written across all collections.
func runExport(
	ctx context.Context,
	db DatabaseAPI,
	jobIDs []interface{},
	fileIDs []interface{},
	cfg *Config,
) (int64, error) {
	if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
		return 0, fmt.Errorf("create output directory %s: %w", cfg.OutputDir, err)
	}

	targets := []exportTarget{
		{collTasks, "job._id", jobIDs},
		{collJobData, "job_id", jobIDs},
		{collJobFiles, "metadata.job", jobIDs},
		{collJobChunks, "files_id", fileIDs},
		{collJobs, "_id", jobIDs},
	}

	var totalWritten int64
	for _, target := range targets {
		if ctx.Err() != nil {
			return totalWritten, ctx.Err()
		}

		if len(target.ids) == 0 {
			log.Printf("Exporting collection: %s — 0 filter IDs, skipping", target.collection)
			continue
		}

		log.Printf("Exporting collection: %s (%d filter IDs)", target.collection, len(target.ids))
		filePath := fmt.Sprintf("%s/%s.jsonl", cfg.OutputDir, target.collection)
		n, err := exportCollection(ctx, db.Collection(target.collection), target.filterKey, target.ids, cfg, filePath)
		totalWritten += n
		if err != nil {
			return totalWritten, err
		}
	}
	return totalWritten, nil
}

// ----------------------------------------------------------------------------
// GridFS helpers
// ----------------------------------------------------------------------------

// findFileIDs collects the _id values of all job_data.files documents whose
// metadata.job field is in jobIDs. The returned IDs are ObjectIDs and are
// used as the files_id filter when deleting job_data.chunks.
func findFileIDs(ctx context.Context, db DatabaseAPI, jobIDs []interface{}, batchSize int) ([]interface{}, error) {
	filesCol := db.Collection(collJobFiles)
	idSet := make(map[interface{}]struct{})

	for i := 0; i < len(jobIDs); i += batchSize {
		end := i + batchSize
		if end > len(jobIDs) {
			end = len(jobIDs)
		}
		batch := jobIDs[i:end]

		fileIDs, err := findIDs(ctx, filesCol,
			bson.D{{Key: "metadata.job", Value: bson.D{{Key: "$in", Value: batch}}}},
		)
		if err != nil {
			return nil, err
		}
		for _, id := range fileIDs {
			idSet[id] = struct{}{}
		}
	}

	result := make([]interface{}, 0, len(idSet))
	for id := range idSet {
		result = append(result, id)
	}
	return result, nil
}

// deleteGridFS deletes job_data.chunks (by files_id) and then job_data.files
// (by metadata.job). Because files_id in job_data.chunks references the _id
// of the job_data.files document — not the job ID — this requires a two-phase
// approach: resolve file document IDs first, then delete chunks by those IDs.
func deleteGridFS(
	ctx context.Context,
	db DatabaseAPI,
	jobIDs []interface{},
	cfg *Config,
) (chunksDeleted, filesDeleted int64, err error) {
	log.Println("Resolving GridFS file IDs from job_data.files...")
	fileIDs, err := findFileIDs(ctx, db, jobIDs, cfg.BatchSize)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve file IDs: %w", err)
	}
	log.Printf("  Found %d job_data.files documents", len(fileIDs))

	if len(fileIDs) > 0 {
		if ctx.Err() != nil {
			return 0, 0, ctx.Err()
		}
		log.Printf("Deleting from %s...", collJobChunks)
		t0 := time.Now()
		chunksDeleted, err = batchDelete(ctx, db.Collection(collJobChunks), "files_id", fileIDs, cfg)
		if err != nil {
			return chunksDeleted, 0, fmt.Errorf("delete %s: %w", collJobChunks, err)
		}
		log.Printf("  %s: deleted %d documents  (%s)", collJobChunks, chunksDeleted, time.Since(t0).Round(time.Second))
	}

	if ctx.Err() != nil {
		return chunksDeleted, 0, ctx.Err()
	}
	log.Printf("Deleting from %s...", collJobFiles)
	t0 := time.Now()
	filesDeleted, err = batchDelete(ctx, db.Collection(collJobFiles), "metadata.job", jobIDs, cfg)
	if err != nil {
		return chunksDeleted, filesDeleted, fmt.Errorf("delete %s: %w", collJobFiles, err)
	}
	log.Printf("  %s: deleted %d documents  (%s)", collJobFiles, filesDeleted, time.Since(t0).Round(time.Second))

	return chunksDeleted, filesDeleted, nil
}

// ----------------------------------------------------------------------------
// Cascade delete
// ----------------------------------------------------------------------------

// deleteAllCollections deletes documents from all five Itential job/task
// collections in safe order — tasks and job data are removed before jobs,
// so job IDs remain queryable if the process is interrupted and re-run.
//
// job_data.chunks is deleted before job_data.files, and both happen before
// jobs. Chunks require a two-phase delete via deleteGridFS because files_id
// references the _id of the job_data.files document, not the job ID.
func deleteAllCollections(
	ctx context.Context,
	db DatabaseAPI,
	ids []interface{},
	cfg *Config,
) (map[string]int64, error) {
	totals := make(map[string]int64)

	// tasks and job_data can be filtered directly by job ID.
	// jobs is last so IDs remain discoverable on interrupted re-runs.
	for _, target := range deleteTargets {
		if ctx.Err() != nil {
			return totals, ctx.Err()
		}
		log.Printf("Deleting from %s...", target.collection)
		t0 := time.Now()
		deleted, err := batchDelete(ctx, db.Collection(target.collection), target.filterKey, ids, cfg)
		if err != nil {
			return totals, fmt.Errorf("delete %s: %w", target.collection, err)
		}
		totals[target.collection] = deleted
		log.Printf("  %s: deleted %d documents  (%s)", target.collection, deleted, time.Since(t0).Round(time.Second))

		// Insert GridFS deletion between job_data and jobs.
		if target.collection == collJobData {
			if ctx.Err() != nil {
				return totals, ctx.Err()
			}
			chunksDeleted, filesDeleted, err := deleteGridFS(ctx, db, ids, cfg)
			if err != nil {
				return totals, err
			}
			totals[collJobChunks] = chunksDeleted
			totals[collJobFiles] = filesDeleted
		}
	}

	return totals, nil
}

// batchDelete deletes all documents from coll where filterKey is in successive
// batches of ids, throttled by cfg.BatchDelayMS between batches.
func batchDelete(
	ctx context.Context,
	coll CollectionAPI,
	filterKey string,
	ids []interface{},
	cfg *Config,
) (int64, error) {
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
		filter := bson.D{{Key: filterKey, Value: bson.D{{Key: "$in", Value: batch}}}}
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

// ----------------------------------------------------------------------------
// Document count summary
// ----------------------------------------------------------------------------

// summarizeAffectedDocuments counts the documents that would be touched in
// each of the five collections and logs a summary table. This runs after
// every discovery — export-only or not — so the user always sees the full
// scope before any writes occur.
//
// job_data.chunks requires a two-phase count: first resolve file document IDs
// from job_data.files, then count chunks by those IDs.
func summarizeAffectedDocuments(ctx context.Context, db DatabaseAPI, ids []interface{}, batchSize int) error {
	log.Println("Counting affected documents across all collections...")

	// countByBatches counts documents in coll where filterKey is in filterIDs,
	// processing in batches. Returns -1 on error (after logging).
	countByBatches := func(collection, filterKey string, filterIDs []interface{}) (int64, time.Duration) {
		t0 := time.Now()
		var total int64
		for i := 0; i < len(filterIDs); i += batchSize {
			end := i + batchSize
			if end > len(filterIDs) {
				end = len(filterIDs)
			}
			batch := filterIDs[i:end]
			filter := bson.D{{Key: filterKey, Value: bson.D{{Key: "$in", Value: batch}}}}
			n, err := db.Collection(collection).CountDocuments(ctx, filter)
			if err != nil {
				log.Printf("  %-25s count failed: %v", collection, err)
				return -1, time.Since(t0)
			}
			total += n
		}
		return total, time.Since(t0)
	}

	var grandTotal int64

	logCount := func(collection string, n int64, dur time.Duration) {
		if n >= 0 {
			log.Printf("  %-25s %d documents  (%s)", collection, n, dur.Round(time.Millisecond))
			grandTotal += n
		}
	}

	// tasks, job_data, jobs — direct job ID filter
	tasksCount, dur := countByBatches(collTasks, "job._id", ids)
	logCount(collTasks, tasksCount, dur)

	n, dur := countByBatches(collJobData, "job_id", ids)
	logCount(collJobData, n, dur)

	n, dur = countByBatches(collJobFiles, "metadata.job", ids)
	logCount(collJobFiles, n, dur)

	// job_data.chunks: files_id references job_data.files._id, not the job ID.
	// Resolve file document IDs first, then count chunks by those IDs.
	t0 := time.Now()
	fileIDs, err := findFileIDs(ctx, db, ids, batchSize)
	if err != nil {
		log.Printf("  %-25s count failed (could not resolve file IDs): %v", collJobChunks, err)
	} else {
		resolvedur := time.Since(t0)
		if len(fileIDs) > 0 {
			n, dur = countByBatches(collJobChunks, "files_id", fileIDs)
			logCount(collJobChunks, n, dur+resolvedur)
		} else {
			log.Printf("  %-25s 0 documents  (%s)", collJobChunks, resolvedur.Round(time.Millisecond))
		}
	}

	n, dur = countByBatches(collJobs, "_id", ids)
	logCount(collJobs, n, dur)

	log.Printf("  %-25s %d documents total", "TOTAL", grandTotal)

	// tasks = 0 with jobs > 0 almost certainly means the task filter is wrong
	// or the data model has diverged from expectations. Abort rather than
	// proceed with an export or delete that is likely incorrect.
	if tasksCount == 0 {
		return fmt.Errorf("found %d jobs but 0 tasks — this is unexpected and likely indicates a query logic error; aborting", len(ids))
	}

	return nil
}

// ----------------------------------------------------------------------------
// Main
// ----------------------------------------------------------------------------

// main is the entry point for job-and-task-archiver. It loads configuration,
// connects to MongoDB, runs two-phase job ID discovery, and then optionally
// exports matching documents to per-collection JSONL files and deletes them
// from the database.
func main() {
	start := time.Now()

	cfg, err := initConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if !cfg.Export {
		log.Println("Export disabled (--export=false)")
	}
	if !cfg.Delete {
		log.Println("Delete disabled — pass --delete to enable deletion after export")
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

	// Cutoff is pinned to midnight UTC of the current day, minus N days.
	// This ensures consistent results regardless of when the tool is run —
	// two runs on the same day with the same --cutoff-days always produce
	// the same cutoff timestamp.
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	cutoff := midnight.AddDate(0, 0, -cfg.CutoffDays)
	cutoffMS := cutoff.UnixMilli()

	log.Printf("Connected to %s  |  cutoff: %s (%d ms)  |  read-preference: %s",
		cfg.Database, cutoff.Format(time.RFC3339), cutoffMS, cfg.ReadPreference)

	// --- Discover job IDs -------------------------------------------------
	//
	// Discovery always runs fresh every invocation. The IDs file is written
	// after discovery so the exact set of IDs is available for inspection,
	// but it is never read back — the next run always re-discovers.
	//
	// IDs are kept as []interface{} (primitive.ObjectID or string) matching
	// the actual BSON type stored in MongoDB, so $in filters send the right type.
	log.Println("Discovering job IDs — this may take a few minutes on large databases...")

	ids, err := discoverJobIDs(ctx, db, cutoffMS)
	if err != nil {
		log.Fatalf("discover: %v", err)
	}

	// Sort for deterministic ordering; required for stable export checkpointing.
	sort.Slice(ids, func(i, j int) bool { return idKey(ids[i]) < idKey(ids[j]) })

	// Write the IDs file for post-run inspection. Not used on the next run.
	typ := "objectid"
	if len(ids) > 0 {
		typ = idType(ids[0])
	}
	strIDs := make([]string, len(ids))
	for i, id := range ids {
		strIDs[i] = idKey(id)
	}
	if err := saveIDCache(cfg.IDsFile, &IDCache{
		CutoffMS: cutoffMS,
		IDType:   typ,
		JobIDs:   strIDs,
	}); err != nil {
		log.Printf("WARN: could not save ids file: %v", err)
	} else {
		log.Printf("Saved %d job IDs to %s (id_type=%s)", len(ids), cfg.IDsFile, typ)
	}

	// Discovery always runs fresh, so any checkpoint from a previous run refers
	// to a different ID set and must be reset. Remove it now so the export
	// always starts from the beginning of the newly discovered IDs.
	if len(ids) == 0 {
		log.Println("No eligible jobs found. Check cutoff-days, database name, and that the " +
			"jobs collection contains documents with status 'complete' or 'canceled'.")
		return
	}

	log.Printf("Processing %d jobs", len(ids))

	// Count affected documents across all five collections so the user can
	// see the full scope before any writes occur. Skip with --skip-count for
	// large datasets where the per-collection queries would take too long.
	if cfg.SkipCount {
		log.Println("Skipping document count summary (--skip-count)")
	} else {
		if err := summarizeAffectedDocuments(ctx, db, ids, cfg.BatchSize); err != nil {
			log.Fatalf("count summary: %v", err)
		}
	}

	// --- Export -----------------------------------------------------------
	if !cfg.Export {
		log.Println("Skipping export phase (--export=false)")
	} else {
		// Resolve GridFS file IDs once here. job_data.chunks.files_id references
		// job_data.files._id (not the job ID), so we need these IDs to export chunks.
		log.Println("Resolving GridFS file IDs for export...")
		fileIDs, err := findFileIDs(ctx, db, ids, cfg.BatchSize)
		if err != nil {
			log.Fatalf("resolve file IDs for export: %v", err)
		}
		log.Printf("Found %d job_data.files documents", len(fileIDs))

		exported, err := runExport(ctx, db, ids, fileIDs, cfg)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				log.Printf("Interrupted during export after %d documents written", exported)
				return
			}
			log.Fatalf("export: %v", err)
		}
		log.Printf("Export complete: %d documents written", exported)
	}

	// --- Delete -----------------------------------------------------------
	if !cfg.Delete {
		log.Println("Delete disabled — run with --delete to remove documents")
		log.Printf("Total runtime: %s", time.Since(start).Round(time.Second))
		return
	}

	totals, err := deleteAllCollections(ctx, db, ids, cfg)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Printf("Interrupted during delete — re-run to continue (delete is idempotent)")
			return
		}
		log.Fatalf("delete: %v", err)
	}

	log.Printf("Delete complete: jobs=%d  tasks=%d  job_data=%d  job_data.files=%d  job_data.chunks=%d",
		totals[collJobs], totals[collTasks], totals[collJobData], totals[collJobFiles], totals[collJobChunks])
	log.Printf("Total runtime: %s", time.Since(start).Round(time.Second))
}
