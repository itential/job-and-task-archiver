package main

import (
	"context"
	"encoding/json"
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

// mockSingleResult simulates a *mongo.SingleResult.
type mockSingleResult struct {
	doc bson.M
	err error
}

func (r *mockSingleResult) Decode(v interface{}) error {
	if r.err != nil {
		return r.err
	}
	data, err := bson.Marshal(r.doc)
	if err != nil {
		return err
	}
	return bson.Unmarshal(data, v)
}

// mockCollection implements CollectionAPI with configurable behaviour per test.
type mockCollection struct {
	name         string
	findFn       func(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (CursorAPI, error)
	findOneFn    func(ctx context.Context, filter interface{}) SingleResultAPI
	deleteManyFn func(ctx context.Context, filter interface{}) (*mongo.DeleteResult, error)
	countFn      func(ctx context.Context, filter interface{}) (int64, error)
}

func (c *mockCollection) Name() string { return c.name }

func (c *mockCollection) Find(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (CursorAPI, error) {
	return c.findFn(ctx, filter, opts...)
}

func (c *mockCollection) FindOne(ctx context.Context, filter interface{}) SingleResultAPI {
	return c.findOneFn(ctx, filter)
}

func (c *mockCollection) DeleteMany(ctx context.Context, filter interface{}) (*mongo.DeleteResult, error) {
	return c.deleteManyFn(ctx, filter)
}

func (c *mockCollection) CountDocuments(ctx context.Context, filter interface{}) (int64, error) {
	return c.countFn(ctx, filter)
}

// mockDatabase implements DatabaseAPI, routing Collection() calls by name.
type mockDatabase struct {
	collections map[string]CollectionAPI
}

func (d *mockDatabase) Collection(name string) CollectionAPI {
	return d.collections[name]
}

// ----------------------------------------------------------------------------
// idKey
// ----------------------------------------------------------------------------

func TestIdKey_String(t *testing.T) {
	got := idKey("abc123")
	if got != "abc123" {
		t.Errorf("expected abc123, got %s", got)
	}
}

func TestIdKey_ObjectID(t *testing.T) {
	oid := primitive.NewObjectID()
	got := idKey(oid)
	if got != oid.Hex() {
		t.Errorf("expected %s, got %s", oid.Hex(), got)
	}
}

func TestIdKey_Other(t *testing.T) {
	got := idKey(42)
	if got != "42" {
		t.Errorf("expected 42, got %s", got)
	}
}

// ----------------------------------------------------------------------------
// idType
// ----------------------------------------------------------------------------

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
		{"Primary", false},
		{"primaryPreferred", false},
		{"secondary", false},
		{"secondaryPreferred", false},
		{"nearest", false},
		{"", false},
		{"invalid", true},
		{"SECONDARY", false},
	}
	for _, tc := range cases {
		_, err := parseReadPref(tc.input)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseReadPref(%q): wantErr=%v got err=%v", tc.input, tc.wantErr, err)
		}
	}
}

// ----------------------------------------------------------------------------
// saveIDCache
// ----------------------------------------------------------------------------

func TestSaveIDCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ids.json")

	cache := &IDCache{
		CutoffMS: 1234567890000,
		IDType:   "string",
		JobIDs:   []string{"aaa", "bbb", "ccc"},
	}

	if err := saveIDCache(path, cache); err != nil {
		t.Fatalf("saveIDCache: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	var loaded IDCache
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if loaded.CutoffMS != cache.CutoffMS {
		t.Errorf("CutoffMS: expected %d got %d", cache.CutoffMS, loaded.CutoffMS)
	}
	if loaded.IDType != cache.IDType {
		t.Errorf("IDType: expected %s got %s", cache.IDType, loaded.IDType)
	}
	if len(loaded.JobIDs) != len(cache.JobIDs) {
		t.Errorf("JobIDs length: expected %d got %d", len(cache.JobIDs), len(loaded.JobIDs))
	}
	if loaded.CreatedAt == "" {
		t.Error("CreatedAt should be set")
	}
}

// ----------------------------------------------------------------------------
// cutoff calculation
// ----------------------------------------------------------------------------

func TestCutoffCalculation(t *testing.T) {
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	cutoff := midnight.AddDate(0, 0, -30)

	if cutoff.Hour() != 0 || cutoff.Minute() != 0 || cutoff.Second() != 0 {
		t.Errorf("cutoff should be exactly midnight, got %s", cutoff.Format(time.RFC3339))
	}
	expected := midnight.AddDate(0, 0, -30)
	if !cutoff.Equal(expected) {
		t.Errorf("expected %s got %s", expected, cutoff)
	}
}

// ----------------------------------------------------------------------------
// findIDs
// ----------------------------------------------------------------------------

func TestFindIDs_StringIDs(t *testing.T) {
	docs := []bson.M{
		{"_id": "job1"},
		{"_id": "job2"},
		{"_id": "job3"},
	}

	coll := &mockCollection{
		name: collJobs,
		findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
			return newMockCursor(docs), nil
		},
	}

	ids, err := findIDs(context.Background(), coll, bson.D{})
	if err != nil {
		t.Fatalf("findIDs: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 IDs, got %d", len(ids))
	}
	if ids[0] != "job1" {
		t.Errorf("expected job1, got %v", ids[0])
	}
}

func TestFindIDs_ObjectIDs(t *testing.T) {
	oid1 := primitive.NewObjectID()
	oid2 := primitive.NewObjectID()
	docs := []bson.M{
		{"_id": oid1},
		{"_id": oid2},
	}

	coll := &mockCollection{
		name: collJobs,
		findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
			return newMockCursor(docs), nil
		},
	}

	ids, err := findIDs(context.Background(), coll, bson.D{})
	if err != nil {
		t.Fatalf("findIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 IDs, got %d", len(ids))
	}
}

func TestFindIDs_Empty(t *testing.T) {
	coll := &mockCollection{
		name: collJobs,
		findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
			return newMockCursor(nil), nil
		},
	}

	ids, err := findIDs(context.Background(), coll, bson.D{})
	if err != nil {
		t.Fatalf("findIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 IDs, got %d", len(ids))
	}
}

// ----------------------------------------------------------------------------
// ancestorsStoredAsStrings
// ----------------------------------------------------------------------------

func TestAncestorsStoredAsStrings_Strings(t *testing.T) {
	doc := bson.M{"ancestors": bson.A{"parentid123"}}
	coll := &mockCollection{
		findOneFn: func(_ context.Context, _ interface{}) SingleResultAPI {
			return &mockSingleResult{doc: doc}
		},
	}

	got, err := ancestorsStoredAsStrings(context.Background(), coll, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("expected true for string ancestors")
	}
}

func TestAncestorsStoredAsStrings_ObjectIDs(t *testing.T) {
	doc := bson.M{"ancestors": bson.A{primitive.NewObjectID()}}
	coll := &mockCollection{
		findOneFn: func(_ context.Context, _ interface{}) SingleResultAPI {
			return &mockSingleResult{doc: doc}
		},
	}

	got, err := ancestorsStoredAsStrings(context.Background(), coll, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("expected false for ObjectID ancestors")
	}
}

// ----------------------------------------------------------------------------
// batchDelete
// ----------------------------------------------------------------------------

func TestBatchDelete_SingleBatch(t *testing.T) {
	var capturedFilter interface{}
	coll := &mockCollection{
		name: collTasks,
		deleteManyFn: func(_ context.Context, filter interface{}) (*mongo.DeleteResult, error) {
			capturedFilter = filter
			return &mongo.DeleteResult{DeletedCount: 5}, nil
		},
	}

	cfg := &Config{BatchSize: 1000, BatchDelayMS: 0}
	ids := []interface{}{"id1", "id2", "id3"}

	deleted, err := batchDelete(context.Background(), coll, "job._id", ids, cfg)
	if err != nil {
		t.Fatalf("batchDelete: %v", err)
	}
	if deleted != 5 {
		t.Errorf("expected 5 deleted, got %d", deleted)
	}
	if capturedFilter == nil {
		t.Error("expected filter to be set")
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
	ids := []interface{}{"id1", "id2", "id3", "id4", "id5"}

	deleted, err := batchDelete(context.Background(), coll, "job._id", ids, cfg)
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
		name:         collTasks,
		deleteManyFn: func(_ context.Context, _ interface{}) (*mongo.DeleteResult, error) {
			return &mongo.DeleteResult{DeletedCount: 0}, nil
		},
	}

	cfg := &Config{BatchSize: 1000, BatchDelayMS: 0}
	deleted, err := batchDelete(context.Background(), coll, "job._id", nil, cfg)
	if err != nil {
		t.Fatalf("batchDelete: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
}

// ----------------------------------------------------------------------------
// summarizeAffectedDocuments
// ----------------------------------------------------------------------------

func TestSummarizeAffectedDocuments(t *testing.T) {
	makeCountColl := func(name string, count int64) CollectionAPI {
		return &mockCollection{
			name: name,
			countFn: func(_ context.Context, _ interface{}) (int64, error) {
				return count, nil
			},
			findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
				return newMockCursor(nil), nil
			},
		}
	}

	db := &mockDatabase{
		collections: map[string]CollectionAPI{
			collTasks:     makeCountColl(collTasks, 100),
			collJobData:   makeCountColl(collJobData, 200),
			collJobFiles:  makeCountColl(collJobFiles, 0),
			collJobChunks: makeCountColl(collJobChunks, 0),
			collJobs:      makeCountColl(collJobs, 10),
		},
	}

	ids := []interface{}{"id1", "id2"}
	err := summarizeAffectedDocuments(context.Background(), db, ids, 1000)
	if err != nil {
		t.Fatalf("summarizeAffectedDocuments: %v", err)
	}
}

func TestSummarizeAffectedDocuments_ZeroTasksErrors(t *testing.T) {
	makeCountColl := func(name string, count int64) CollectionAPI {
		return &mockCollection{
			name: name,
			countFn: func(_ context.Context, _ interface{}) (int64, error) {
				return count, nil
			},
			findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
				return newMockCursor(nil), nil
			},
		}
	}

	db := &mockDatabase{
		collections: map[string]CollectionAPI{
			collTasks:     makeCountColl(collTasks, 0),
			collJobData:   makeCountColl(collJobData, 50),
			collJobFiles:  makeCountColl(collJobFiles, 0),
			collJobChunks: makeCountColl(collJobChunks, 0),
			collJobs:      makeCountColl(collJobs, 5),
		},
	}

	ids := []interface{}{"id1"}
	err := summarizeAffectedDocuments(context.Background(), db, ids, 1000)
	if err == nil {
		t.Error("expected error when tasks count is 0")
	}
}

// ----------------------------------------------------------------------------
// exportCollection
// ----------------------------------------------------------------------------

func TestExportCollection_WritesJSONL(t *testing.T) {
	docs := []bson.M{
		{"_id": "job1", "status": "complete"},
		{"_id": "job2", "status": "canceled"},
	}

	coll := &mockCollection{
		name: collJobs,
		findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
			return newMockCursor(docs), nil
		},
	}

	dir := t.TempDir()
	filePath := filepath.Join(dir, "jobs.jsonl")
	cfg := &Config{BatchSize: 1000, BatchDelayMS: 0}
	ids := []interface{}{"job1", "job2"}

	written, err := exportCollection(context.Background(), coll, "_id", ids, cfg, filePath)
	if err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	if written != 2 {
		t.Errorf("expected 2 written, got %d", written)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	lines := splitLines(data)
	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(lines))
	}
}

func TestExportCollection_NoMatchesNoFile(t *testing.T) {
	coll := &mockCollection{
		name: collJobFiles,
		findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
			return newMockCursor(nil), nil
		},
	}

	dir := t.TempDir()
	filePath := filepath.Join(dir, "job_data.files.jsonl")
	cfg := &Config{BatchSize: 1000, BatchDelayMS: 0}
	ids := []interface{}{"job1", "job2"}

	written, err := exportCollection(context.Background(), coll, "metadata.job", ids, cfg, filePath)
	if err != nil {
		t.Fatalf("exportCollection: %v", err)
	}
	if written != 0 {
		t.Errorf("expected 0 written, got %d", written)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Error("expected no file to be created when no documents match")
	}
}

func TestExportCollection_DocumentsUnmodified(t *testing.T) {
	docs := []bson.M{
		{"_id": "job1", "status": "complete", "metrics": bson.M{"end_time": int64(1700000000000)}},
	}

	coll := &mockCollection{
		name: collJobs,
		findFn: func(_ context.Context, _ interface{}, _ ...*options.FindOptions) (CursorAPI, error) {
			return newMockCursor(docs), nil
		},
	}

	dir := t.TempDir()
	filePath := filepath.Join(dir, "jobs.jsonl")
	cfg := &Config{BatchSize: 1000, BatchDelayMS: 0}

	_, err := exportCollection(context.Background(), coll, "_id", []interface{}{"job1"}, cfg, filePath)
	if err != nil {
		t.Fatalf("exportCollection: %v", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(splitLines(data)[0], &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := result["_collection"]; ok {
		t.Error("_collection field should not be present in exported documents")
	}
	if result["status"] != "complete" {
		t.Errorf("status field modified: got %v", result["status"])
	}
}

// splitLines returns non-empty lines from a byte slice.
func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}
