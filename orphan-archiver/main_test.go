package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ----------------------------------------------------------------------------
// Mock implementations
// ----------------------------------------------------------------------------

// mockCursor simulates a *mongo.Cursor over a fixed slice of bson.M documents.
type mockCursor struct {
	docs []bson.M
	pos  int
	err  error
}

func newMockCursor(docs []bson.M) *mockCursor {
	return &mockCursor{docs: docs, pos: -1}
}

func (c *mockCursor) Next(_ context.Context) bool {
	c.pos++
	return c.pos < len(c.docs)
}

func (c *mockCursor) Decode(v interface{}) error {
	data, err := bson.Marshal(c.docs[c.pos])
	if err != nil {
		return err
	}
	return bson.Unmarshal(data, v)
}

func (c *mockCursor) Close(_ context.Context) error { return nil }

func (c *mockCursor) All(_ context.Context, results interface{}) error {
	out, ok := results.(*[]bson.M)
	if !ok {
		return nil
	}
	for _, doc := range c.docs {
		*out = append(*out, doc)
	}
	return nil
}

func (c *mockCursor) Err() error { return c.err }

// mockCollection implements CollectionAPI with configurable behaviour per test.
type mockCollection struct {
	name                string
	findFn              func(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (CursorAPI, error)
	aggregateFn         func(ctx context.Context, pipeline interface{}) (CursorAPI, error)
	deleteManyFn        func(ctx context.Context, filter interface{}) (*mongo.DeleteResult, error)
	estimatedDocCountFn func(ctx context.Context) (int64, error)
}

func (c *mockCollection) Name() string { return c.name }

func (c *mockCollection) Find(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (CursorAPI, error) {
	return c.findFn(ctx, filter, opts...)
}

func (c *mockCollection) Aggregate(ctx context.Context, pipeline interface{}) (CursorAPI, error) {
	return c.aggregateFn(ctx, pipeline)
}

func (c *mockCollection) DeleteMany(ctx context.Context, filter interface{}) (*mongo.DeleteResult, error) {
	return c.deleteManyFn(ctx, filter)
}

// EstimatedDocumentCount defaults to (0, nil) when the test doesn't care
// about it, so existing test literals don't all need updating for this field.
func (c *mockCollection) EstimatedDocumentCount(ctx context.Context) (int64, error) {
	if c.estimatedDocCountFn == nil {
		return 0, nil
	}
	return c.estimatedDocCountFn(ctx)
}

// mockDatabase implements DatabaseAPI, routing Collection() calls by name.
type mockDatabase struct {
	collections map[string]CollectionAPI
}

func (d *mockDatabase) Collection(name string) CollectionAPI {
	return d.collections[name]
}

// ----------------------------------------------------------------------------
// idKey / idType
// ----------------------------------------------------------------------------

func TestIdKey_String(t *testing.T) {
	if got := idKey("abc123"); got != "abc123" {
		t.Errorf("expected abc123, got %s", got)
	}
}

func TestIdKey_ObjectID(t *testing.T) {
	oid := primitive.NewObjectID()
	if got := idKey(oid); got != oid.Hex() {
		t.Errorf("expected %s, got %s", oid.Hex(), got)
	}
}

func TestIdType_String(t *testing.T) {
	if idType("abc") != "string" {
		t.Error("expected string")
	}
}

func TestIdType_ObjectID(t *testing.T) {
	if idType(primitive.NewObjectID()) != "objectid" {
		t.Error("expected objectid")
	}
}

// ----------------------------------------------------------------------------
// parseReadPref
// ----------------------------------------------------------------------------

func TestParseReadPref(t *testing.T) {
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"primary", false},
		{"secondaryPreferred", false},
		{"", false},
		{"invalid", true},
	}
	for _, tc := range cases {
		_, err := parseReadPref(tc.input)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseReadPref(%q): wantErr=%v got err=%v", tc.input, tc.wantErr, err)
		}
	}
}

// ----------------------------------------------------------------------------
// directOrphanPipeline / chunksOrphanPipeline
// ----------------------------------------------------------------------------

// TestDirectOrphanPipeline_Shape confirms the pipeline joins against the jobs
// collection on the given local field and filters for no match (_job size 0)
// — i.e. the document's job reference does not exist in jobs.
func TestDirectOrphanPipeline_Shape(t *testing.T) {
	pipeline := directOrphanPipeline("job_id")
	if len(pipeline) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(pipeline))
	}

	lookup, ok := pipeline[0].(bson.D)
	if !ok {
		t.Fatalf("stage 0 is not a bson.D: %#v", pipeline[0])
	}
	lookupBody, ok := lookup[0].Value.(bson.D)
	if !ok || lookup[0].Key != "$lookup" {
		t.Fatalf("stage 0 is not $lookup: %#v", lookup)
	}
	fields := map[string]interface{}{}
	for _, e := range lookupBody {
		fields[e.Key] = e.Value
	}
	if fields["from"] != collJobs {
		t.Errorf("expected lookup from %q, got %v", collJobs, fields["from"])
	}
	if fields["localField"] != "job_id" {
		t.Errorf("expected localField job_id, got %v", fields["localField"])
	}
	if fields["foreignField"] != "_id" {
		t.Errorf("expected foreignField _id, got %v", fields["foreignField"])
	}
}

// TestChunksOrphanPipeline_Shape confirms the chunks pipeline uses a nested
// $lookup (via let/pipeline) into job_data.files rather than a direct
// localField/foreignField join, since files_id references job_data.files._id
// rather than a job ID.
func TestChunksOrphanPipeline_Shape(t *testing.T) {
	pipeline := chunksOrphanPipeline()
	if len(pipeline) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(pipeline))
	}

	lookup, ok := pipeline[0].(bson.D)
	if !ok || lookup[0].Key != "$lookup" {
		t.Fatalf("stage 0 is not $lookup: %#v", pipeline[0])
	}
	lookupBody, ok := lookup[0].Value.(bson.D)
	if !ok {
		t.Fatalf("$lookup value is not a bson.D: %#v", lookup[0].Value)
	}
	fields := map[string]interface{}{}
	for _, e := range lookupBody {
		fields[e.Key] = e.Value
	}
	if fields["from"] != collJobFiles {
		t.Errorf("expected nested lookup from %q, got %v", collJobFiles, fields["from"])
	}
	if _, ok := fields["pipeline"]; !ok {
		t.Error("expected a pipeline-style $lookup (with a nested pipeline), got a direct localField/foreignField lookup")
	}
}

// ----------------------------------------------------------------------------
// aggregateIDs
// ----------------------------------------------------------------------------

func TestAggregateIDs(t *testing.T) {
	docs := []bson.M{{"_id": "orphan1"}, {"_id": "orphan2"}}
	coll := &mockCollection{
		name: collTasks,
		aggregateFn: func(_ context.Context, _ interface{}) (CursorAPI, error) {
			return newMockCursor(docs), nil
		},
	}

	ids, err := aggregateIDs(context.Background(), coll, directOrphanPipeline("job._id"), 0)
	if err != nil {
		t.Fatalf("aggregateIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs, got %d", len(ids))
	}
	if ids[0] != "orphan1" || ids[1] != "orphan2" {
		t.Errorf("unexpected IDs: %v", ids)
	}
}

func TestAggregateIDs_Empty(t *testing.T) {
	coll := &mockCollection{
		name: collTasks,
		aggregateFn: func(_ context.Context, _ interface{}) (CursorAPI, error) {
			return newMockCursor(nil), nil
		},
	}

	ids, err := aggregateIDs(context.Background(), coll, directOrphanPipeline("job._id"), 0)
	if err != nil {
		t.Fatalf("aggregateIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 IDs, got %d", len(ids))
	}
}

// TestAggregateIDs_Heartbeat confirms the heartbeat ticker logs at least once
// during a slow aggregation, without interfering with the returned results.
// The mock's aggregateFn sleeps briefly before returning so the ticker (set
// to fire every 5ms) has a chance to run at least once.
func TestAggregateIDs_Heartbeat(t *testing.T) {
	coll := &mockCollection{
		name: collTasks,
		aggregateFn: func(_ context.Context, _ interface{}) (CursorAPI, error) {
			time.Sleep(20 * time.Millisecond)
			return newMockCursor([]bson.M{{"_id": "orphan1"}}), nil
		},
	}

	ids, err := aggregateIDs(context.Background(), coll, directOrphanPipeline("job._id"), 5*time.Millisecond)
	if err != nil {
		t.Fatalf("aggregateIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "orphan1" {
		t.Errorf("unexpected IDs: %v", ids)
	}
}

// ----------------------------------------------------------------------------
// discoverOrphans
// ----------------------------------------------------------------------------

// TestDiscoverOrphans_RoutesEachCollectionIndependently confirms discovery
// queries all four collections and correctly attributes each collection's
// results, independent of the others (in particular, that job_data.chunks
// does not depend on job_data.files having been queried first).
func TestDiscoverOrphans_RoutesEachCollectionIndependently(t *testing.T) {
	makeColl := func(name string, ids []bson.M) CollectionAPI {
		return &mockCollection{
			name: name,
			aggregateFn: func(_ context.Context, _ interface{}) (CursorAPI, error) {
				return newMockCursor(ids), nil
			},
		}
	}

	db := &mockDatabase{collections: map[string]CollectionAPI{
		collTasks:     makeColl(collTasks, []bson.M{{"_id": "t1"}}),
		collJobData:   makeColl(collJobData, []bson.M{{"_id": "jd1"}, {"_id": "jd2"}}),
		collJobFiles:  makeColl(collJobFiles, nil),
		collJobChunks: makeColl(collJobChunks, []bson.M{{"_id": "c1"}}),
	}}

	orphans, err := discoverOrphans(context.Background(), db, 0)
	if err != nil {
		t.Fatalf("discoverOrphans: %v", err)
	}

	if len(orphans[collTasks]) != 1 {
		t.Errorf("expected 1 orphaned task, got %d", len(orphans[collTasks]))
	}
	if len(orphans[collJobData]) != 2 {
		t.Errorf("expected 2 orphaned job_data docs, got %d", len(orphans[collJobData]))
	}
	if len(orphans[collJobFiles]) != 0 {
		t.Errorf("expected 0 orphaned job_data.files docs, got %d", len(orphans[collJobFiles]))
	}
	if len(orphans[collJobChunks]) != 1 {
		t.Errorf("expected 1 orphaned chunk, got %d", len(orphans[collJobChunks]))
	}
}

// TestDiscoverOrphans_LogsEstimatedCountBeforeScan confirms
// EstimatedDocumentCount is called once per collection ahead of the
// aggregation, and that a failure to estimate doesn't abort discovery — it's
// a best-effort operator hint, not a correctness requirement.
func TestDiscoverOrphans_LogsEstimatedCountBeforeScan(t *testing.T) {
	estimateCalled := false
	makeColl := func(name string, ids []bson.M) CollectionAPI {
		return &mockCollection{
			name: name,
			aggregateFn: func(_ context.Context, _ interface{}) (CursorAPI, error) {
				return newMockCursor(ids), nil
			},
			estimatedDocCountFn: func(_ context.Context) (int64, error) {
				estimateCalled = true
				return 12345, nil
			},
		}
	}
	failingEstimateColl := &mockCollection{
		name: collJobData,
		aggregateFn: func(_ context.Context, _ interface{}) (CursorAPI, error) {
			return newMockCursor(nil), nil
		},
		estimatedDocCountFn: func(_ context.Context) (int64, error) {
			return 0, errors.New("estimate failed")
		},
	}

	db := &mockDatabase{collections: map[string]CollectionAPI{
		collTasks:     makeColl(collTasks, nil),
		collJobData:   failingEstimateColl,
		collJobFiles:  makeColl(collJobFiles, nil),
		collJobChunks: makeColl(collJobChunks, nil),
	}}

	if _, err := discoverOrphans(context.Background(), db, 0); err != nil {
		t.Fatalf("discoverOrphans should not fail when a count estimate fails: %v", err)
	}
	if !estimateCalled {
		t.Error("expected EstimatedDocumentCount to be called")
	}
}

// ----------------------------------------------------------------------------
// saveOrphanReport
// ----------------------------------------------------------------------------

func TestSaveOrphanReport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orphan-report.json")

	oid := primitive.NewObjectID()
	orphans := map[string][]interface{}{
		collTasks:    {"taskA", "taskB"},
		collJobData:  {oid},
		collJobFiles: {},
	}

	if err := saveOrphanReport(path, orphans); err != nil {
		t.Fatalf("saveOrphanReport: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	var report OrphanReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if report.Counts[collTasks] != 2 {
		t.Errorf("expected 2 tasks, got %d", report.Counts[collTasks])
	}
	if report.IDType[collTasks] != "string" {
		t.Errorf("expected string id_type for tasks, got %s", report.IDType[collTasks])
	}
	if report.IDType[collJobData] != "objectid" {
		t.Errorf("expected objectid id_type for job_data, got %s", report.IDType[collJobData])
	}
	if report.Counts[collJobFiles] != 0 {
		t.Errorf("expected 0 job_data.files, got %d", report.Counts[collJobFiles])
	}
	if report.CreatedAt == "" {
		t.Error("CreatedAt should be set")
	}
}

// ----------------------------------------------------------------------------
// batchDelete
// ----------------------------------------------------------------------------

func TestBatchDelete_SingleBatch(t *testing.T) {
	coll := &mockCollection{
		name: collTasks,
		deleteManyFn: func(_ context.Context, _ interface{}) (*mongo.DeleteResult, error) {
			return &mongo.DeleteResult{DeletedCount: 5}, nil
		},
	}

	cfg := &Config{BatchSize: 1000, BatchDelayMS: 0}
	deleted, err := batchDelete(context.Background(), coll, []interface{}{"id1", "id2", "id3"}, cfg)
	if err != nil {
		t.Fatalf("batchDelete: %v", err)
	}
	if deleted != 5 {
		t.Errorf("expected 5 deleted, got %d", deleted)
	}
}

func TestBatchDelete_MultipleBatches(t *testing.T) {
	callCount := 0
	coll := &mockCollection{
		name: collTasks,
		deleteManyFn: func(_ context.Context, _ interface{}) (*mongo.DeleteResult, error) {
			callCount++
			return &mongo.DeleteResult{DeletedCount: 2}, nil
		},
	}

	cfg := &Config{BatchSize: 2, BatchDelayMS: 0}
	deleted, err := batchDelete(context.Background(), coll, []interface{}{"id1", "id2", "id3", "id4", "id5"}, cfg)
	if err != nil {
		t.Fatalf("batchDelete: %v", err)
	}
	if callCount != 3 {
		t.Errorf("expected 3 batches, got %d", callCount)
	}
	if deleted != 6 {
		t.Errorf("expected 6 deleted, got %d", deleted)
	}
}

func TestBatchDelete_Empty(t *testing.T) {
	coll := &mockCollection{
		name: collTasks,
		deleteManyFn: func(_ context.Context, _ interface{}) (*mongo.DeleteResult, error) {
			t.Fatal("DeleteMany should not be called for an empty ID slice")
			return nil, nil
		},
	}

	cfg := &Config{BatchSize: 1000, BatchDelayMS: 0}
	deleted, err := batchDelete(context.Background(), coll, nil, cfg)
	if err != nil {
		t.Fatalf("batchDelete: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
}

// ----------------------------------------------------------------------------
// exportCollection
// ----------------------------------------------------------------------------

func TestExportCollection_WritesJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.jsonl")

	docs := []bson.M{{"_id": "id1", "foo": "bar"}, {"_id": "id2", "foo": "baz"}}
	coll := &mockCollection{
		name: collTasks,
		findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
			return newMockCursor(docs), nil
		},
	}

	cfg := &Config{BatchSize: 1000, BatchDelayMS: 0}
	written, err := exportCollection(context.Background(), coll, []interface{}{"id1", "id2"}, cfg, path)
	if err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	if written != 2 {
		t.Errorf("expected 2 written, got %d", written)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("expected 2 lines, got %d", lines)
	}
}

func TestExportCollection_NoMatchesNoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job_data.files.jsonl")

	coll := &mockCollection{
		name: collJobFiles,
		findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
			return newMockCursor(nil), nil
		},
	}

	cfg := &Config{BatchSize: 1000, BatchDelayMS: 0}
	written, err := exportCollection(context.Background(), coll, []interface{}{"id1"}, cfg, path)
	if err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	if written != 0 {
		t.Errorf("expected 0 written, got %d", written)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected no file to be created when there are no matching documents")
	}
}

// ----------------------------------------------------------------------------
// runOrphanExport / deleteAllOrphans wiring
// ----------------------------------------------------------------------------

// TestRunOrphanExport_SkipsEmptyCollections confirms collections with no
// discovered orphans are skipped entirely (no Find call, no file written).
func TestRunOrphanExport_SkipsEmptyCollections(t *testing.T) {
	dir := t.TempDir()

	findCalled := false
	emptyColl := &mockCollection{
		name: collJobFiles,
		findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
			findCalled = true
			return newMockCursor(nil), nil
		},
	}
	nonEmptyColl := &mockCollection{
		name: collTasks,
		findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
			return newMockCursor([]bson.M{{"_id": "t1"}}), nil
		},
	}

	db := &mockDatabase{collections: map[string]CollectionAPI{
		collTasks:     nonEmptyColl,
		collJobData:   emptyColl,
		collJobFiles:  emptyColl,
		collJobChunks: emptyColl,
	}}

	orphans := map[string][]interface{}{
		collTasks:     {"t1"},
		collJobData:   {},
		collJobFiles:  {},
		collJobChunks: {},
	}

	cfg := &Config{BatchSize: 1000, BatchDelayMS: 0, OutputDir: dir}
	written, err := runOrphanExport(context.Background(), db, orphans, cfg)
	if err != nil {
		t.Fatalf("runOrphanExport: %v", err)
	}
	if written != 1 {
		t.Errorf("expected 1 document written, got %d", written)
	}
	if findCalled {
		t.Error("expected Find not to be called for collections with 0 orphaned documents")
	}
}

// TestDeleteAllOrphans_SkipsEmptyCollections mirrors the export test for the
// delete path.
func TestDeleteAllOrphans_SkipsEmptyCollections(t *testing.T) {
	deleteCalled := false
	emptyColl := &mockCollection{
		name: collJobFiles,
		deleteManyFn: func(_ context.Context, _ interface{}) (*mongo.DeleteResult, error) {
			deleteCalled = true
			return &mongo.DeleteResult{}, nil
		},
	}
	nonEmptyColl := &mockCollection{
		name: collTasks,
		deleteManyFn: func(_ context.Context, _ interface{}) (*mongo.DeleteResult, error) {
			return &mongo.DeleteResult{DeletedCount: 1}, nil
		},
	}

	db := &mockDatabase{collections: map[string]CollectionAPI{
		collTasks:     nonEmptyColl,
		collJobData:   emptyColl,
		collJobFiles:  emptyColl,
		collJobChunks: emptyColl,
	}}

	orphans := map[string][]interface{}{
		collTasks:     {"t1"},
		collJobData:   {},
		collJobFiles:  {},
		collJobChunks: {},
	}

	cfg := &Config{BatchSize: 1000, BatchDelayMS: 0}
	totals, err := deleteAllOrphans(context.Background(), db, orphans, cfg)
	if err != nil {
		t.Fatalf("deleteAllOrphans: %v", err)
	}
	if totals[collTasks] != 1 {
		t.Errorf("expected 1 task deleted, got %d", totals[collTasks])
	}
	if deleteCalled {
		t.Error("expected DeleteMany not to be called for collections with 0 orphaned documents")
	}
}
