# Vector Bucket Phase 1 实现计划

> **给 Agent 执行者:** 必须使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务执行本计划。步骤使用 `- [ ]` 语法追踪进度。

**目标:** 构建 Vector Bucket Gateway 服务，提供 REST API 实现 bucket/collection/vector 的 CRUD 和查询操作，底层对接 Milvus Standalone（IVF_SQ8+mmap），包含 LRU/TTL Load/Release 控制器、配额管控和监控指标。

**架构:** 一个独立的 Go HTTP 服务（`internal/vectorbucket/`），使用 Gin 框架（go.mod 中已有），分层设计：Gateway（HTTP handler + 鉴权 + 限流）-> Metadata Service（SQLite 持久化 bucket/collection 状态）-> Namespace Router（逻辑名 -> 物理名映射）-> Load/Release Controller（LRU+TTL 内存预算管理）-> Milvus Adapter（封装 `client/milvusclient`）。服务与 Milvus Standalone 同 VM 部署，通过 gRPC localhost 连接。

**技术栈:** Go、Gin HTTP 框架、Milvus Go Client（`client/milvusclient`）、SQLite（`modernc.org/sqlite`，纯 Go 无 CGo）、Prometheus client_golang 指标。

---

## 文件结构

```
internal/vectorbucket/
  cmd/                          # 入口
    main.go                     # 服务启动引导
  config/
    config.go                   # 配置结构体 + 环境变量加载
  metadata/
    store.go                    # MetadataStore 接口定义
    sqlite_store.go             # SQLite 实现
    models.go                   # Bucket、LogicalCollection 结构体
  router/
    namespace_router.go         # 逻辑名 -> 物理 Milvus collection 名解析
  adapter/
    milvus_adapter.go           # 封装 milvusclient，提供 collection/vector 操作
  controller/
    load_controller.go          # LRU + TTL load/release 逻辑
  gateway/
    server.go                   # Gin 引擎配置、路由注册
    handlers_bucket.go          # Bucket CRUD handler
    handlers_collection.go      # Collection CRUD handler
    handlers_vector.go          # Vector put/upsert/delete handler
    handlers_query.go           # 查询 handler
    middleware.go               # 鉴权、限流、配额中间件
    errors.go                   # 统一错误响应
  quota/
    quota.go                    # 配额检查逻辑
  metrics/
    metrics.go                  # Prometheus 指标定义
```

```
internal/vectorbucket/
  cmd/
    main_test.go
  metadata/
    sqlite_store_test.go
  router/
    namespace_router_test.go
  adapter/
    milvus_adapter_test.go
  controller/
    load_controller_test.go
  gateway/
    handlers_bucket_test.go
    handlers_collection_test.go
    handlers_vector_test.go
    handlers_query_test.go
  quota/
    quota_test.go
  integration/
    api_test.go                 # 端到端集成测试
```

---

## 任务 1：项目脚手架 + 配置模块

**文件:**
- 新建: `internal/vectorbucket/config/config.go`
- 新建: `internal/vectorbucket/cmd/main.go`

- [ ] **步骤 1: 编写配置测试**

```go
// internal/vectorbucket/config/config_test.go
package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, ":9200", cfg.ListenAddr)
	assert.Equal(t, "localhost:19530", cfg.MilvusAddr)
	assert.Equal(t, 4096, cfg.LoadBudgetMB)
	assert.Equal(t, 30*60, cfg.TTLSeconds)
	assert.Equal(t, 10000, cfg.IndexBuildThreshold)
	assert.NotEmpty(t, cfg.SQLitePath)
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("VB_LISTEN_ADDR", ":8080")
	t.Setenv("VB_MILVUS_ADDR", "milvus:19530")
	t.Setenv("VB_LOAD_BUDGET_MB", "2048")
	cfg := LoadConfig()
	assert.Equal(t, ":8080", cfg.ListenAddr)
	assert.Equal(t, "milvus:19530", cfg.MilvusAddr)
	assert.Equal(t, 2048, cfg.LoadBudgetMB)
}
```

- [ ] **步骤 2: 运行测试确认失败**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 ./internal/vectorbucket/config/...`
预期: FAIL — package 不存在

- [ ] **步骤 3: 实现配置模块**

```go
// internal/vectorbucket/config/config.go
package config

import (
	"os"
	"strconv"
)

type Config struct {
	ListenAddr          string // 服务监听地址
	MilvusAddr          string // Milvus gRPC 地址
	SQLitePath          string // SQLite 数据库路径
	LoadBudgetMB        int    // 活跃 load 总内存预算（MB）
	TTLSeconds          int    // 空闲 collection 自动 release 的 TTL（秒）
	IndexBuildThreshold int    // 触发建索引的向量数阈值
	MaxBucketsPerTenant int    // 单租户 bucket 上限
	MaxBucketsGlobal    int    // 全局 bucket 上限
	MaxCollPerBucket    int    // 单 bucket 下 collection 上限
	MaxVectorsPerColl   int    // 单 collection 向量数上限
	MaxDim              int    // 最大维度
	WriteQPSPerColl     int    // 单 collection 写入 QPS 上限
	QueryQPSPerColl     int    // 单 collection 查询 QPS 上限
	MaxLoadedColls      int    // 同时 loaded 的 collection 硬上限
}

func DefaultConfig() Config {
	return Config{
		ListenAddr:          ":9200",
		MilvusAddr:          "localhost:19530",
		SQLitePath:          "/var/lib/vectorbucket/metadata.db",
		LoadBudgetMB:        4096,
		TTLSeconds:          30 * 60,
		IndexBuildThreshold: 10000,
		MaxBucketsPerTenant: 100,
		MaxBucketsGlobal:    1000,
		MaxCollPerBucket:    50,
		MaxVectorsPerColl:   1000000,
		MaxDim:              1536,
		WriteQPSPerColl:     500,
		QueryQPSPerColl:     50,
		MaxLoadedColls:      50,
	}
}

func LoadConfig() Config {
	cfg := DefaultConfig()
	if v := os.Getenv("VB_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("VB_MILVUS_ADDR"); v != "" {
		cfg.MilvusAddr = v
	}
	if v := os.Getenv("VB_SQLITE_PATH"); v != "" {
		cfg.SQLitePath = v
	}
	if v := os.Getenv("VB_LOAD_BUDGET_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.LoadBudgetMB = n
		}
	}
	if v := os.Getenv("VB_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.TTLSeconds = n
		}
	}
	return cfg
}
```

- [ ] **步骤 4: 运行测试确认通过**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 ./internal/vectorbucket/config/...`
预期: PASS

- [ ] **步骤 5: 编写 main.go 占位文件**

```go
// internal/vectorbucket/cmd/main.go
package main

import (
	"fmt"

	"github.com/milvus-io/milvus/internal/vectorbucket/config"
)

func main() {
	cfg := config.LoadConfig()
	fmt.Printf("Vector Bucket Gateway starting on %s, Milvus at %s\n", cfg.ListenAddr, cfg.MilvusAddr)
}
```

- [ ] **步骤 6: 验证编译通过**

运行: `cd /root/xty/milvus && go build ./internal/vectorbucket/cmd/`
预期: 成功，无错误

- [ ] **步骤 7: 提交**

```bash
git add internal/vectorbucket/config/ internal/vectorbucket/cmd/
git commit -s -m "feat(vectorbucket): add project scaffold and configuration

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 2：Metadata 模型 + Store 接口

**文件:**
- 新建: `internal/vectorbucket/metadata/models.go`
- 新建: `internal/vectorbucket/metadata/store.go`

- [ ] **步骤 1: 编写模型结构体和 Store 接口**

```go
// internal/vectorbucket/metadata/models.go
package metadata

import "time"

type BucketStatus string

const (
	BucketStatusReady    BucketStatus = "READY"
	BucketStatusDeleting BucketStatus = "DELETING"
	BucketStatusDeleted  BucketStatus = "DELETED"
)

type CollectionStatus string

const (
	CollStatusInit     CollectionStatus = "INIT"
	CollStatusReady    CollectionStatus = "READY"
	CollStatusDeleting CollectionStatus = "DELETING"
	CollStatusDeleted  CollectionStatus = "DELETED"
)

type Bucket struct {
	ID        string
	Name      string
	Owner     string
	Status    BucketStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

type LogicalCollection struct {
	ID           string
	BucketID     string
	Name         string
	Dim          int
	Metric       string // "COSINE" 或 "L2"
	Status       CollectionStatus
	PhysicalName string // "vb_{bucket_id}_{lc_id}"
	IndexBuilt   bool
	VectorCount  int64
	EstMemMB     float64
	LastAccessAt time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
```

```go
// internal/vectorbucket/metadata/store.go
package metadata

import "context"

type Store interface {
	// Bucket 操作
	CreateBucket(ctx context.Context, b *Bucket) error
	GetBucket(ctx context.Context, id string) (*Bucket, error)
	GetBucketByName(ctx context.Context, name string) (*Bucket, error)
	ListBuckets(ctx context.Context) ([]*Bucket, error)
	UpdateBucketStatus(ctx context.Context, id string, status BucketStatus) error
	DeleteBucket(ctx context.Context, id string) error
	CountBuckets(ctx context.Context) (int, error)
	CountBucketsByOwner(ctx context.Context, owner string) (int, error)

	// Collection 操作
	CreateCollection(ctx context.Context, c *LogicalCollection) error
	GetCollection(ctx context.Context, bucketID, name string) (*LogicalCollection, error)
	GetCollectionByID(ctx context.Context, id string) (*LogicalCollection, error)
	ListCollections(ctx context.Context, bucketID string) ([]*LogicalCollection, error)
	UpdateCollectionStatus(ctx context.Context, id string, status CollectionStatus) error
	UpdateCollectionIndexBuilt(ctx context.Context, id string, built bool) error
	UpdateCollectionVectorCount(ctx context.Context, id string, delta int64) error
	UpdateCollectionLastAccess(ctx context.Context, id string) error
	DeleteCollection(ctx context.Context, id string) error
	CountCollections(ctx context.Context, bucketID string) (int, error)

	// 初始化和关闭
	Init(ctx context.Context) error
	Close() error
}
```

- [ ] **步骤 2: 验证编译通过**

运行: `cd /root/xty/milvus && go build ./internal/vectorbucket/metadata/`
预期: 成功

- [ ] **步骤 3: 提交**

```bash
git add internal/vectorbucket/metadata/models.go internal/vectorbucket/metadata/store.go
git commit -s -m "feat(vectorbucket): add metadata models and store interface

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 3：SQLite Metadata Store 实现

**文件:**
- 新建: `internal/vectorbucket/metadata/sqlite_store.go`
- 新建: `internal/vectorbucket/metadata/sqlite_store_test.go`

**说明:** 使用 `modernc.org/sqlite`（纯 Go，无 CGo 依赖）。需先执行 `go get modernc.org/sqlite`。

- [ ] **步骤 1: 添加 SQLite 依赖**

运行: `cd /root/xty/milvus && go get modernc.org/sqlite`

- [ ] **步骤 2: 编写 Bucket CRUD 失败测试**

```go
// internal/vectorbucket/metadata/sqlite_store_test.go
package metadata

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s := NewSQLiteStore(dbPath)
	require.NoError(t, s.Init(context.Background()))
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndGetBucket(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	b := &Bucket{ID: "b1", Name: "my-bucket", Owner: "user1", Status: BucketStatusReady}
	require.NoError(t, s.CreateBucket(ctx, b))

	got, err := s.GetBucket(ctx, "b1")
	require.NoError(t, err)
	assert.Equal(t, "my-bucket", got.Name)
	assert.Equal(t, "user1", got.Owner)
	assert.Equal(t, BucketStatusReady, got.Status)
}

func TestGetBucketByName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	b := &Bucket{ID: "b1", Name: "my-bucket", Owner: "user1", Status: BucketStatusReady}
	require.NoError(t, s.CreateBucket(ctx, b))

	got, err := s.GetBucketByName(ctx, "my-bucket")
	require.NoError(t, err)
	assert.Equal(t, "b1", got.ID)
}

func TestCreateDuplicateBucketNameFails(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	b1 := &Bucket{ID: "b1", Name: "same-name", Owner: "user1", Status: BucketStatusReady}
	require.NoError(t, s.CreateBucket(ctx, b1))

	b2 := &Bucket{ID: "b2", Name: "same-name", Owner: "user1", Status: BucketStatusReady}
	err := s.CreateBucket(ctx, b2)
	assert.Error(t, err)
}

func TestListBuckets(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateBucket(ctx, &Bucket{ID: "b1", Name: "a", Owner: "u1", Status: BucketStatusReady}))
	require.NoError(t, s.CreateBucket(ctx, &Bucket{ID: "b2", Name: "b", Owner: "u1", Status: BucketStatusReady}))

	list, err := s.ListBuckets(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestCountBuckets(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateBucket(ctx, &Bucket{ID: "b1", Name: "a", Owner: "u1", Status: BucketStatusReady}))
	require.NoError(t, s.CreateBucket(ctx, &Bucket{ID: "b2", Name: "b", Owner: "u1", Status: BucketStatusReady}))

	cnt, err := s.CountBuckets(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, cnt)

	cnt, err = s.CountBucketsByOwner(ctx, "u1")
	require.NoError(t, err)
	assert.Equal(t, 2, cnt)
}

func TestDeleteBucket(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateBucket(ctx, &Bucket{ID: "b1", Name: "a", Owner: "u1", Status: BucketStatusReady}))
	require.NoError(t, s.DeleteBucket(ctx, "b1"))

	_, err := s.GetBucket(ctx, "b1")
	assert.Error(t, err)
}
```

- [ ] **步骤 3: 运行测试确认失败**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 ./internal/vectorbucket/metadata/...`
预期: FAIL — `NewSQLiteStore` 未定义

- [ ] **步骤 4: 实现 SQLiteStore（Bucket + Collection 全部操作）**

```go
// internal/vectorbucket/metadata/sqlite_store.go
package metadata

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	dbPath string
	db     *sql.DB
}

func NewSQLiteStore(dbPath string) *SQLiteStore {
	return &SQLiteStore{dbPath: dbPath}
}

func (s *SQLiteStore) Init(ctx context.Context) error {
	db, err := sql.Open("sqlite", s.dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	s.db = db

	schema := `
	CREATE TABLE IF NOT EXISTS buckets (
		id         TEXT PRIMARY KEY,
		name       TEXT NOT NULL UNIQUE,
		owner      TEXT NOT NULL,
		status     TEXT NOT NULL DEFAULT 'READY',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS collections (
		id              TEXT PRIMARY KEY,
		bucket_id       TEXT NOT NULL REFERENCES buckets(id),
		name            TEXT NOT NULL,
		dim             INTEGER NOT NULL,
		metric          TEXT NOT NULL,
		status          TEXT NOT NULL DEFAULT 'INIT',
		physical_name   TEXT NOT NULL,
		index_built     BOOLEAN NOT NULL DEFAULT FALSE,
		vector_count    INTEGER NOT NULL DEFAULT 0,
		est_mem_mb      REAL NOT NULL DEFAULT 0,
		last_access_at  DATETIME,
		created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(bucket_id, name)
	);
	`
	_, err = s.db.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("create tables: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// --- Bucket 操作 ---

func (s *SQLiteStore) CreateBucket(ctx context.Context, b *Bucket) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO buckets (id, name, owner, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		b.ID, b.Name, b.Owner, b.Status, now, now)
	return err
}

func (s *SQLiteStore) GetBucket(ctx context.Context, id string) (*Bucket, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT id, name, owner, status, created_at, updated_at FROM buckets WHERE id = ?", id)
	return scanBucket(row)
}

func (s *SQLiteStore) GetBucketByName(ctx context.Context, name string) (*Bucket, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT id, name, owner, status, created_at, updated_at FROM buckets WHERE name = ?", name)
	return scanBucket(row)
}

func (s *SQLiteStore) ListBuckets(ctx context.Context) ([]*Bucket, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, name, owner, status, created_at, updated_at FROM buckets WHERE status != 'DELETED' ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []*Bucket
	for rows.Next() {
		b := &Bucket{}
		if err := rows.Scan(&b.ID, &b.Name, &b.Owner, &b.Status, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) UpdateBucketStatus(ctx context.Context, id string, status BucketStatus) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE buckets SET status = ?, updated_at = ? WHERE id = ?", status, time.Now(), id)
	return err
}

func (s *SQLiteStore) DeleteBucket(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM buckets WHERE id = ?", id)
	return err
}

func (s *SQLiteStore) CountBuckets(ctx context.Context) (int, error) {
	var cnt int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM buckets WHERE status != 'DELETED'").Scan(&cnt)
	return cnt, err
}

func (s *SQLiteStore) CountBucketsByOwner(ctx context.Context, owner string) (int, error) {
	var cnt int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM buckets WHERE owner = ? AND status != 'DELETED'", owner).Scan(&cnt)
	return cnt, err
}

// --- Collection 操作 ---

func (s *SQLiteStore) CreateCollection(ctx context.Context, c *LogicalCollection) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO collections (id, bucket_id, name, dim, metric, status, physical_name, index_built, vector_count, est_mem_mb, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.BucketID, c.Name, c.Dim, c.Metric, c.Status, c.PhysicalName, c.IndexBuilt, c.VectorCount, c.EstMemMB, now, now)
	return err
}

func (s *SQLiteStore) GetCollection(ctx context.Context, bucketID, name string) (*LogicalCollection, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, bucket_id, name, dim, metric, status, physical_name, index_built, vector_count, est_mem_mb, last_access_at, created_at, updated_at
		 FROM collections WHERE bucket_id = ? AND name = ? AND status != 'DELETED'`, bucketID, name)
	return scanCollection(row)
}

func (s *SQLiteStore) GetCollectionByID(ctx context.Context, id string) (*LogicalCollection, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, bucket_id, name, dim, metric, status, physical_name, index_built, vector_count, est_mem_mb, last_access_at, created_at, updated_at
		 FROM collections WHERE id = ?`, id)
	return scanCollection(row)
}

func (s *SQLiteStore) ListCollections(ctx context.Context, bucketID string) ([]*LogicalCollection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, bucket_id, name, dim, metric, status, physical_name, index_built, vector_count, est_mem_mb, last_access_at, created_at, updated_at
		 FROM collections WHERE bucket_id = ? AND status != 'DELETED' ORDER BY created_at`, bucketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []*LogicalCollection
	for rows.Next() {
		c, err := scanCollectionRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) UpdateCollectionStatus(ctx context.Context, id string, status CollectionStatus) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE collections SET status = ?, updated_at = ? WHERE id = ?", status, time.Now(), id)
	return err
}

func (s *SQLiteStore) UpdateCollectionIndexBuilt(ctx context.Context, id string, built bool) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE collections SET index_built = ?, updated_at = ? WHERE id = ?", built, time.Now(), id)
	return err
}

func (s *SQLiteStore) UpdateCollectionVectorCount(ctx context.Context, id string, delta int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE collections SET vector_count = vector_count + ?, updated_at = ? WHERE id = ?", delta, time.Now(), id)
	return err
}

func (s *SQLiteStore) UpdateCollectionLastAccess(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE collections SET last_access_at = ?, updated_at = ? WHERE id = ?", time.Now(), time.Now(), id)
	return err
}

func (s *SQLiteStore) DeleteCollection(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM collections WHERE id = ?", id)
	return err
}

func (s *SQLiteStore) CountCollections(ctx context.Context, bucketID string) (int, error) {
	var cnt int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM collections WHERE bucket_id = ? AND status != 'DELETED'", bucketID).Scan(&cnt)
	return cnt, err
}

// --- Scan 辅助函数 ---

func scanBucket(row *sql.Row) (*Bucket, error) {
	b := &Bucket{}
	err := row.Scan(&b.ID, &b.Name, &b.Owner, &b.Status, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func scanCollection(row *sql.Row) (*LogicalCollection, error) {
	c := &LogicalCollection{}
	var lastAccess sql.NullTime
	err := row.Scan(&c.ID, &c.BucketID, &c.Name, &c.Dim, &c.Metric, &c.Status, &c.PhysicalName,
		&c.IndexBuilt, &c.VectorCount, &c.EstMemMB, &lastAccess, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if lastAccess.Valid {
		c.LastAccessAt = lastAccess.Time
	}
	return c, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCollectionRow(row rowScanner) (*LogicalCollection, error) {
	c := &LogicalCollection{}
	var lastAccess sql.NullTime
	err := row.Scan(&c.ID, &c.BucketID, &c.Name, &c.Dim, &c.Metric, &c.Status, &c.PhysicalName,
		&c.IndexBuilt, &c.VectorCount, &c.EstMemMB, &lastAccess, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if lastAccess.Valid {
		c.LastAccessAt = lastAccess.Time
	}
	return c, nil
}
```

- [ ] **步骤 5: 运行测试确认通过**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 ./internal/vectorbucket/metadata/...`
预期: PASS

- [ ] **步骤 6: 编写 Collection CRUD 测试**

```go
// 追加到 internal/vectorbucket/metadata/sqlite_store_test.go

func TestCreateAndGetCollection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateBucket(ctx, &Bucket{ID: "b1", Name: "bkt", Owner: "u1", Status: BucketStatusReady}))

	c := &LogicalCollection{
		ID: "c1", BucketID: "b1", Name: "my-coll", Dim: 768, Metric: "COSINE",
		Status: CollStatusReady, PhysicalName: "vb_b1_c1",
	}
	require.NoError(t, s.CreateCollection(ctx, c))

	got, err := s.GetCollection(ctx, "b1", "my-coll")
	require.NoError(t, err)
	assert.Equal(t, "c1", got.ID)
	assert.Equal(t, 768, got.Dim)
	assert.Equal(t, "COSINE", got.Metric)
	assert.Equal(t, "vb_b1_c1", got.PhysicalName)
}

func TestDuplicateCollectionNameInBucketFails(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateBucket(ctx, &Bucket{ID: "b1", Name: "bkt", Owner: "u1", Status: BucketStatusReady}))

	c1 := &LogicalCollection{ID: "c1", BucketID: "b1", Name: "coll", Dim: 768, Metric: "COSINE", Status: CollStatusReady, PhysicalName: "vb_b1_c1"}
	require.NoError(t, s.CreateCollection(ctx, c1))

	c2 := &LogicalCollection{ID: "c2", BucketID: "b1", Name: "coll", Dim: 768, Metric: "COSINE", Status: CollStatusReady, PhysicalName: "vb_b1_c2"}
	assert.Error(t, s.CreateCollection(ctx, c2))
}

func TestUpdateCollectionVectorCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateBucket(ctx, &Bucket{ID: "b1", Name: "bkt", Owner: "u1", Status: BucketStatusReady}))
	c := &LogicalCollection{ID: "c1", BucketID: "b1", Name: "coll", Dim: 768, Metric: "COSINE", Status: CollStatusReady, PhysicalName: "vb_b1_c1"}
	require.NoError(t, s.CreateCollection(ctx, c))

	require.NoError(t, s.UpdateCollectionVectorCount(ctx, "c1", 100))
	got, _ := s.GetCollectionByID(ctx, "c1")
	assert.Equal(t, int64(100), got.VectorCount)

	require.NoError(t, s.UpdateCollectionVectorCount(ctx, "c1", -30))
	got, _ = s.GetCollectionByID(ctx, "c1")
	assert.Equal(t, int64(70), got.VectorCount)
}

func TestCountCollections(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateBucket(ctx, &Bucket{ID: "b1", Name: "bkt", Owner: "u1", Status: BucketStatusReady}))
	require.NoError(t, s.CreateCollection(ctx, &LogicalCollection{ID: "c1", BucketID: "b1", Name: "a", Dim: 768, Metric: "COSINE", Status: CollStatusReady, PhysicalName: "vb_b1_c1"}))
	require.NoError(t, s.CreateCollection(ctx, &LogicalCollection{ID: "c2", BucketID: "b1", Name: "b", Dim: 768, Metric: "COSINE", Status: CollStatusReady, PhysicalName: "vb_b1_c2"}))

	cnt, err := s.CountCollections(ctx, "b1")
	require.NoError(t, err)
	assert.Equal(t, 2, cnt)
}
```

- [ ] **步骤 7: 运行全部 metadata 测试**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 -v ./internal/vectorbucket/metadata/...`
预期: 全部 PASS

- [ ] **步骤 8: 提交**

```bash
git add internal/vectorbucket/metadata/
git commit -s -m "feat(vectorbucket): implement SQLite metadata store with bucket and collection CRUD

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 4：Namespace Router（命名空间路由）

**文件:**
- 新建: `internal/vectorbucket/router/namespace_router.go`
- 新建: `internal/vectorbucket/router/namespace_router_test.go`

- [ ] **步骤 1: 编写失败测试**

```go
// internal/vectorbucket/router/namespace_router_test.go
package router

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
)

func newTestRouter(t *testing.T) *NamespaceRouter {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store := metadata.NewSQLiteStore(dbPath)
	require.NoError(t, store.Init(context.Background()))
	t.Cleanup(func() { store.Close() })
	return NewNamespaceRouter(store)
}

func TestPhysicalName(t *testing.T) {
	assert.Equal(t, "vb_b1_c1", PhysicalCollectionName("b1", "c1"))
}

func TestResolveCollection(t *testing.T) {
	r := newTestRouter(t)
	ctx := context.Background()

	// 准备：在 metadata 中创建 bucket + collection
	require.NoError(t, r.store.CreateBucket(ctx, &metadata.Bucket{
		ID: "b1", Name: "my-bucket", Owner: "u1", Status: metadata.BucketStatusReady,
	}))
	require.NoError(t, r.store.CreateCollection(ctx, &metadata.LogicalCollection{
		ID: "c1", BucketID: "b1", Name: "my-coll", Dim: 768, Metric: "COSINE",
		Status: metadata.CollStatusReady, PhysicalName: "vb_b1_c1",
	}))

	lc, err := r.Resolve(ctx, "my-bucket", "my-coll")
	require.NoError(t, err)
	assert.Equal(t, "vb_b1_c1", lc.PhysicalName)
	assert.Equal(t, 768, lc.Dim)
}

func TestResolveNotFound(t *testing.T) {
	r := newTestRouter(t)
	ctx := context.Background()

	_, err := r.Resolve(ctx, "no-bucket", "no-coll")
	assert.Error(t, err)
}
```

- [ ] **步骤 2: 运行测试确认失败**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 ./internal/vectorbucket/router/...`
预期: FAIL

- [ ] **步骤 3: 实现 NamespaceRouter**

```go
// internal/vectorbucket/router/namespace_router.go
package router

import (
	"context"
	"fmt"

	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
)

// PhysicalCollectionName 生成物理 Milvus collection 名称
func PhysicalCollectionName(bucketID, collectionID string) string {
	return fmt.Sprintf("vb_%s_%s", bucketID, collectionID)
}

type NamespaceRouter struct {
	store metadata.Store
}

func NewNamespaceRouter(store metadata.Store) *NamespaceRouter {
	return &NamespaceRouter{store: store}
}

// Resolve 通过 bucket 名 + collection 名查找逻辑 collection，
// 返回包含物理 Milvus collection 名称的完整 LogicalCollection。
func (r *NamespaceRouter) Resolve(ctx context.Context, bucketName, collectionName string) (*metadata.LogicalCollection, error) {
	bucket, err := r.store.GetBucketByName(ctx, bucketName)
	if err != nil {
		return nil, fmt.Errorf("bucket %q not found: %w", bucketName, err)
	}
	if bucket.Status != metadata.BucketStatusReady {
		return nil, fmt.Errorf("bucket %q is not ready (status=%s)", bucketName, bucket.Status)
	}

	coll, err := r.store.GetCollection(ctx, bucket.ID, collectionName)
	if err != nil {
		return nil, fmt.Errorf("collection %q not found in bucket %q: %w", collectionName, bucketName, err)
	}
	if coll.Status != metadata.CollStatusReady {
		return nil, fmt.Errorf("collection %q is not ready (status=%s)", collectionName, coll.Status)
	}

	return coll, nil
}
```

- [ ] **步骤 4: 运行测试确认通过**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 ./internal/vectorbucket/router/...`
预期: PASS

- [ ] **步骤 5: 提交**

```bash
git add internal/vectorbucket/router/
git commit -s -m "feat(vectorbucket): add namespace router for logical-to-physical collection resolution

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 5：Milvus Adapter（Milvus 适配层）

**文件:**
- 新建: `internal/vectorbucket/adapter/milvus_adapter.go`
- 新建: `internal/vectorbucket/adapter/milvus_adapter_test.go`

- [ ] **步骤 1: 定义 Adapter 接口和实现**

```go
// internal/vectorbucket/adapter/milvus_adapter.go
package adapter

import (
	"context"
	"fmt"
	"math"

	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/index"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// Adapter 定义 Vector Bucket 对 Milvus 的操作接口
type Adapter interface {
	CreateCollection(ctx context.Context, name string, dim int, metric string) error
	DropCollection(ctx context.Context, name string) error
	HasCollection(ctx context.Context, name string) (bool, error)
	CreateIndex(ctx context.Context, name string, vectorCount int64, metric string) error
	LoadCollection(ctx context.Context, name string) error
	ReleaseCollection(ctx context.Context, name string) error
	Insert(ctx context.Context, name string, ids []string, vectors [][]float32, metadataJSON [][]byte, timestamps []int64) error
	Upsert(ctx context.Context, name string, ids []string, vectors [][]float32, metadataJSON [][]byte, timestamps []int64) error
	Delete(ctx context.Context, name string, ids []string) error
	Search(ctx context.Context, name string, vector []float32, topK int, nprobe int, filter string, metric string) ([]SearchResult, error)
	Close() error
}

type SearchResult struct {
	ID       string
	Score    float32
	Metadata []byte
}

type MilvusAdapter struct {
	client *milvusclient.Client
}

func NewMilvusAdapter(addr string) (*MilvusAdapter, error) {
	client, err := milvusclient.New(context.Background(), &milvusclient.ClientConfig{
		Address: addr,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to milvus at %s: %w", addr, err)
	}
	return &MilvusAdapter{client: client}, nil
}

func (a *MilvusAdapter) Close() error {
	return a.client.Close(context.Background())
}

func (a *MilvusAdapter) CreateCollection(ctx context.Context, name string, dim int, metric string) error {
	schema := entity.NewSchema().
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeVarChar).WithMaxLength(64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("vector").WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dim))).
		WithField(entity.NewField().WithName("metadata").WithDataType(entity.FieldTypeJSON)).
		WithField(entity.NewField().WithName("created_at").WithDataType(entity.FieldTypeInt64))

	return a.client.CreateCollection(ctx, milvusclient.NewCreateCollectionOption(name, schema).
		WithShardNum(1))
}

func (a *MilvusAdapter) DropCollection(ctx context.Context, name string) error {
	return a.client.DropCollection(ctx, milvusclient.NewDropCollectionOption(name))
}

func (a *MilvusAdapter) HasCollection(ctx context.Context, name string) (bool, error) {
	return a.client.HasCollection(ctx, milvusclient.NewHasCollectionOption(name))
}

// computeNlist 根据设计文档公式计算 IVF nlist 参数: clamp(sqrt(N)*4, 1024, 65536)
func computeNlist(vectorCount int64) int {
	nlist := int(math.Sqrt(float64(vectorCount)) * 4)
	if nlist < 1024 {
		nlist = 1024
	}
	if nlist > 65536 {
		nlist = 65536
	}
	return nlist
}

func metricTypeFromString(metric string) entity.MetricType {
	switch metric {
	case "L2":
		return entity.L2
	case "COSINE":
		return entity.COSINE
	default:
		return entity.COSINE
	}
}

func (a *MilvusAdapter) CreateIndex(ctx context.Context, name string, vectorCount int64, metric string) error {
	nlist := computeNlist(vectorCount)
	idx := index.NewIvfSQ8Index(metricTypeFromString(metric), nlist)
	return a.client.CreateIndex(ctx, milvusclient.NewCreateIndexOption(name, "vector", idx).
		WithExtraParam("mmap.enabled", "true"))
}

func (a *MilvusAdapter) LoadCollection(ctx context.Context, name string) error {
	loadTask, err := a.client.LoadCollection(ctx, milvusclient.NewLoadCollectionOption(name))
	if err != nil {
		return err
	}
	return loadTask.Await(ctx)
}

func (a *MilvusAdapter) ReleaseCollection(ctx context.Context, name string) error {
	return a.client.ReleaseCollection(ctx, milvusclient.NewReleaseCollectionOption(name))
}

func (a *MilvusAdapter) Insert(ctx context.Context, name string, ids []string, vectors [][]float32, metadataJSON [][]byte, timestamps []int64) error {
	_, err := a.client.Insert(ctx, milvusclient.NewColumnBasedInsertOption(name).
		WithVarcharColumn("id", ids).
		WithFloatVectorColumn("vector", len(vectors[0]), vectors).
		WithColumn(entity.NewColumnJSONBytes("metadata", metadataJSON)).
		WithInt64Column("created_at", timestamps))
	return err
}

func (a *MilvusAdapter) Upsert(ctx context.Context, name string, ids []string, vectors [][]float32, metadataJSON [][]byte, timestamps []int64) error {
	_, err := a.client.Upsert(ctx, milvusclient.NewColumnBasedInsertOption(name).
		WithVarcharColumn("id", ids).
		WithFloatVectorColumn("vector", len(vectors[0]), vectors).
		WithColumn(entity.NewColumnJSONBytes("metadata", metadataJSON)).
		WithInt64Column("created_at", timestamps))
	return err
}

func (a *MilvusAdapter) Delete(ctx context.Context, name string, ids []string) error {
	return a.client.Delete(ctx, milvusclient.NewDeleteOption(name).WithExpr(
		buildIDFilter(ids)))
}

func buildIDFilter(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	expr := "id in ["
	for i, id := range ids {
		if i > 0 {
			expr += ","
		}
		expr += fmt.Sprintf("%q", id)
	}
	expr += "]"
	return expr
}

func (a *MilvusAdapter) Search(ctx context.Context, name string, vector []float32, topK int, nprobe int, filter string, metric string) ([]SearchResult, error) {
	searchOpt := milvusclient.NewSearchOption(name, topK, []entity.Vector{entity.FloatVector(vector)}).
		WithSearchParam("nprobe", nprobe).
		WithOutputFields("id", "metadata")

	if filter != "" {
		searchOpt = searchOpt.WithFilter(filter)
	}

	results, err := a.client.Search(ctx, searchOpt)
	if err != nil {
		return nil, err
	}

	var out []SearchResult
	for _, rs := range results {
		for i := 0; i < rs.ResultCount; i++ {
			id, _ := rs.IDs.GetAsString(i)
			var meta []byte
			if col := rs.Fields.GetColumn("metadata"); col != nil {
				metaVal, _ := col.GetAsString(i)
				meta = []byte(metaVal)
			}
			out = append(out, SearchResult{
				ID:       id,
				Score:    rs.Scores[i],
				Metadata: meta,
			})
		}
	}
	return out, nil
}
```

- [ ] **步骤 2: 编写纯函数的单元测试**

```go
// internal/vectorbucket/adapter/milvus_adapter_test.go
package adapter

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComputeNlist(t *testing.T) {
	// 小数量 → 钳制到 1024
	assert.Equal(t, 1024, computeNlist(100))

	// sqrt(100000)*4 = 1264
	assert.Equal(t, 1264, computeNlist(100000))

	// 超大数量 → 钳制到 65536
	assert.Equal(t, 65536, computeNlist(1000000000))
}

func TestBuildIDFilter(t *testing.T) {
	assert.Equal(t, "", buildIDFilter(nil))
	assert.Equal(t, `id in ["a"]`, buildIDFilter([]string{"a"}))
	assert.Equal(t, `id in ["a","b","c"]`, buildIDFilter([]string{"a", "b", "c"}))
}

func TestMetricTypeFromString(t *testing.T) {
	assert.NotNil(t, metricTypeFromString("L2"))
	assert.NotNil(t, metricTypeFromString("COSINE"))
	assert.NotNil(t, metricTypeFromString("unknown"))
}
```

- [ ] **步骤 3: 运行测试**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 ./internal/vectorbucket/adapter/...`
预期: PASS（仅纯函数测试；Milvus 集成测试在任务 16）

- [ ] **步骤 4: 提交**

```bash
git add internal/vectorbucket/adapter/
git commit -s -m "feat(vectorbucket): add Milvus adapter wrapping client for collection and vector operations

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 6：Load/Release 控制器

**文件:**
- 新建: `internal/vectorbucket/controller/load_controller.go`
- 新建: `internal/vectorbucket/controller/load_controller_test.go`

- [ ] **步骤 1: 编写失败测试**

```go
// internal/vectorbucket/controller/load_controller_test.go
package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockAdapter 记录 load/release 调用，不依赖真实 Milvus
type mockAdapter struct {
	mu        sync.Mutex
	loaded    map[string]bool
	loadDelay time.Duration
}

func newMockAdapter() *mockAdapter {
	return &mockAdapter{loaded: make(map[string]bool)}
}

func (m *mockAdapter) LoadCollection(ctx context.Context, name string) error {
	if m.loadDelay > 0 {
		time.Sleep(m.loadDelay)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loaded[name] = true
	return nil
}

func (m *mockAdapter) ReleaseCollection(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.loaded, name)
	return nil
}

func (m *mockAdapter) isLoaded(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loaded[name]
}

func TestEnsureLoaded(t *testing.T) {
	ma := newMockAdapter()
	ctrl := NewLoadController(ma, 4096, 30*time.Minute, 50)

	err := ctrl.EnsureLoaded(context.Background(), "coll1", 100.0)
	require.NoError(t, err)
	assert.True(t, ma.isLoaded("coll1"))
	assert.True(t, ctrl.IsLoaded("coll1"))
}

func TestEnsureLoadedIdempotent(t *testing.T) {
	ma := newMockAdapter()
	ctrl := NewLoadController(ma, 4096, 30*time.Minute, 50)

	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "coll1", 100.0))
	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "coll1", 100.0))
	assert.True(t, ctrl.IsLoaded("coll1"))
}

func TestLRUEviction(t *testing.T) {
	ma := newMockAdapter()
	// 预算: 200 MB
	ctrl := NewLoadController(ma, 200, 30*time.Minute, 50)

	// 加载两个 100MB 的 collection → 预算满
	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "coll1", 100.0))
	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "coll2", 100.0))

	// 加载第三个 → 应淘汰 coll1（LRU 最旧）
	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "coll3", 100.0))
	assert.False(t, ctrl.IsLoaded("coll1"), "coll1 应被淘汰")
	assert.True(t, ctrl.IsLoaded("coll2"))
	assert.True(t, ctrl.IsLoaded("coll3"))
}

func TestTTLRelease(t *testing.T) {
	ma := newMockAdapter()
	// TTL 100ms，加速测试
	ctrl := NewLoadController(ma, 4096, 100*time.Millisecond, 50)

	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "coll1", 100.0))
	assert.True(t, ctrl.IsLoaded("coll1"))

	// 等待 TTL 过期
	time.Sleep(200 * time.Millisecond)
	ctrl.RunTTLSweep()

	assert.False(t, ctrl.IsLoaded("coll1"))
}

func TestInFlightPreventsRelease(t *testing.T) {
	ma := newMockAdapter()
	ctrl := NewLoadController(ma, 4096, 100*time.Millisecond, 50)

	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "coll1", 100.0))
	ctrl.InFlightInc("coll1")

	// 等待 TTL
	time.Sleep(200 * time.Millisecond)
	ctrl.RunTTLSweep()

	// 有 in-flight 查询，不应被 release
	assert.True(t, ctrl.IsLoaded("coll1"))

	ctrl.InFlightDec("coll1")
	ctrl.RunTTLSweep()
	// 现在应该被 release
	assert.False(t, ctrl.IsLoaded("coll1"))
}

func TestMaxLoadedCollections(t *testing.T) {
	ma := newMockAdapter()
	ctrl := NewLoadController(ma, 999999, 30*time.Minute, 2) // 最多 2 个

	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "c1", 1.0))
	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "c2", 1.0))

	// 第三个应淘汰 LRU
	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "c3", 1.0))
	assert.False(t, ctrl.IsLoaded("c1"))
}

func TestTouchUpdatesLRU(t *testing.T) {
	ma := newMockAdapter()
	ctrl := NewLoadController(ma, 200, 30*time.Minute, 50)

	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "coll1", 100.0))
	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "coll2", 100.0))

	// Touch coll1，使其变为最近使用
	ctrl.Touch("coll1")

	// 加载 coll3 → 应淘汰 coll2（此时 LRU 最旧），而非 coll1
	require.NoError(t, ctrl.EnsureLoaded(context.Background(), "coll3", 100.0))
	assert.True(t, ctrl.IsLoaded("coll1"), "coll1 已被 touch，不应被淘汰")
	assert.False(t, ctrl.IsLoaded("coll2"), "coll2 应作为 LRU 被淘汰")
}
```

- [ ] **步骤 2: 运行测试确认失败**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 ./internal/vectorbucket/controller/...`
预期: FAIL

- [ ] **步骤 3: 实现 LoadController**

```go
// internal/vectorbucket/controller/load_controller.go
package controller

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// LoadReleaser 是控制器所需的 Adapter 子集接口
type LoadReleaser interface {
	LoadCollection(ctx context.Context, name string) error
	ReleaseCollection(ctx context.Context, name string) error
}

type LoadEntry struct {
	Name          string
	LoadedAt      time.Time
	LastAccessAt  time.Time
	EstMemMB      float64
	InFlightCount int64
}

type LoadController struct {
	mu          sync.Mutex
	adapter     LoadReleaser
	budgetMB    float64
	ttl         time.Duration
	maxLoaded   int
	entries     map[string]*LoadEntry
	lruOrder    []string // 前端 = 最旧，尾部 = 最新
	loadLocks   map[string]*sync.Mutex
	loadLocksMu sync.Mutex
}

func NewLoadController(adapter LoadReleaser, budgetMB int, ttl time.Duration, maxLoaded int) *LoadController {
	return &LoadController{
		adapter:   adapter,
		budgetMB:  float64(budgetMB),
		ttl:       ttl,
		maxLoaded: maxLoaded,
		entries:   make(map[string]*LoadEntry),
		loadLocks: make(map[string]*sync.Mutex),
	}
}

func (c *LoadController) getLoadLock(name string) *sync.Mutex {
	c.loadLocksMu.Lock()
	defer c.loadLocksMu.Unlock()
	if _, ok := c.loadLocks[name]; !ok {
		c.loadLocks[name] = &sync.Mutex{}
	}
	return c.loadLocks[name]
}

func (c *LoadController) IsLoaded(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[name]
	return ok
}

func (c *LoadController) Touch(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[name]; ok {
		e.LastAccessAt = time.Now()
		c.moveLRUToBack(name)
	}
}

func (c *LoadController) InFlightInc(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[name]; ok {
		e.InFlightCount++
	}
}

func (c *LoadController) InFlightDec(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[name]; ok {
		if e.InFlightCount > 0 {
			e.InFlightCount--
		}
	}
}

func (c *LoadController) EnsureLoaded(ctx context.Context, name string, estMemMB float64) error {
	// 快路径：已加载
	c.mu.Lock()
	if _, ok := c.entries[name]; ok {
		c.entries[name].LastAccessAt = time.Now()
		c.moveLRUToBack(name)
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	// 慢路径：获取 per-collection 互斥锁
	lock := c.getLoadLock(name)
	lock.Lock()
	defer lock.Unlock()

	// 双重检查
	c.mu.Lock()
	if _, ok := c.entries[name]; ok {
		c.entries[name].LastAccessAt = time.Now()
		c.moveLRUToBack(name)
		c.mu.Unlock()
		return nil
	}

	// 需要腾空间时淘汰
	if err := c.evictIfNeeded(estMemMB); err != nil {
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	// 调用 Milvus load（在锁外执行，避免阻塞）
	if err := c.adapter.LoadCollection(ctx, name); err != nil {
		return fmt.Errorf("load collection %s: %w", name, err)
	}

	// 注册
	c.mu.Lock()
	now := time.Now()
	c.entries[name] = &LoadEntry{
		Name:         name,
		LoadedAt:     now,
		LastAccessAt: now,
		EstMemMB:     estMemMB,
	}
	c.lruOrder = append(c.lruOrder, name)
	c.mu.Unlock()

	return nil
}

// evictIfNeeded 按 LRU 顺序淘汰 collection 直到预算足够。调用时必须持有 c.mu。
func (c *LoadController) evictIfNeeded(neededMB float64) error {
	for c.usedMB()+neededMB > c.budgetMB || len(c.entries) >= c.maxLoaded {
		if len(c.lruOrder) == 0 {
			return fmt.Errorf("no collections to evict, budget exhausted")
		}

		evicted := false
		for _, candidate := range c.lruOrder {
			entry := c.entries[candidate]
			if entry == nil {
				continue
			}
			if entry.InFlightCount > 0 {
				continue
			}
			if err := c.adapter.ReleaseCollection(context.Background(), candidate); err != nil {
				return fmt.Errorf("release collection %s: %w", candidate, err)
			}
			delete(c.entries, candidate)
			c.removeLRU(candidate)
			evicted = true
			break
		}
		if !evicted {
			return fmt.Errorf("all loaded collections have in-flight queries, cannot evict")
		}
	}
	return nil
}

func (c *LoadController) usedMB() float64 {
	var total float64
	for _, e := range c.entries {
		total += e.EstMemMB
	}
	return total
}

// RunTTLSweep 扫描并释放超过 TTL 且无 in-flight 查询的 collection
func (c *LoadController) RunTTLSweep() {
	c.mu.Lock()
	now := time.Now()
	var toRelease []string
	for name, entry := range c.entries {
		if now.Sub(entry.LastAccessAt) > c.ttl && entry.InFlightCount == 0 {
			toRelease = append(toRelease, name)
		}
	}
	c.mu.Unlock()

	for _, name := range toRelease {
		_ = c.adapter.ReleaseCollection(context.Background(), name)
		c.mu.Lock()
		delete(c.entries, name)
		c.removeLRU(name)
		c.mu.Unlock()
	}
}

// StartTTLSweepLoop 启动后台 goroutine 定时扫描 TTL
func (c *LoadController) StartTTLSweepLoop(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.RunTTLSweep()
			}
		}
	}()
}

// EstimateMemMB 计算 IVF_SQ8 模式下的预估内存占用
// IVF_SQ8: 每个向量 = dim * 1 byte (SQ8) * overhead_ratio
func EstimateMemMB(vectorCount int64, dim int) float64 {
	const overheadRatio = 1.2
	bytes := float64(vectorCount) * float64(dim) * 1.0 * overheadRatio
	return bytes / (1024 * 1024)
}

// --- LRU 辅助方法（调用时必须持有 c.mu）---

func (c *LoadController) moveLRUToBack(name string) {
	c.removeLRU(name)
	c.lruOrder = append(c.lruOrder, name)
}

func (c *LoadController) removeLRU(name string) {
	for i, n := range c.lruOrder {
		if n == name {
			c.lruOrder = append(c.lruOrder[:i], c.lruOrder[i+1:]...)
			return
		}
	}
}
```

- [ ] **步骤 4: 运行测试确认通过**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 -v ./internal/vectorbucket/controller/...`
预期: 全部 PASS

- [ ] **步骤 5: 提交**

```bash
git add internal/vectorbucket/controller/
git commit -s -m "feat(vectorbucket): implement LRU+TTL Load/Release Controller with budget management

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 7：配额管控

**文件:**
- 新建: `internal/vectorbucket/quota/quota.go`
- 新建: `internal/vectorbucket/quota/quota_test.go`

- [ ] **步骤 1: 编写失败测试**

```go
// internal/vectorbucket/quota/quota_test.go
package quota

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/milvus-io/milvus/internal/vectorbucket/config"
	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
)

func newTestChecker(t *testing.T) (*Checker, *metadata.SQLiteStore) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store := metadata.NewSQLiteStore(dbPath)
	require.NoError(t, store.Init(context.Background()))
	t.Cleanup(func() { store.Close() })
	cfg := config.DefaultConfig()
	return NewChecker(store, &cfg), store
}

func TestCanCreateBucket(t *testing.T) {
	checker, _ := newTestChecker(t)
	ctx := context.Background()

	err := checker.CanCreateBucket(ctx, "owner1")
	assert.NoError(t, err)
}

func TestCanCreateBucketExceedsPerTenantLimit(t *testing.T) {
	checker, store := newTestChecker(t)
	ctx := context.Background()

	// 填满单租户上限 (100)
	for i := 0; i < 100; i++ {
		require.NoError(t, store.CreateBucket(ctx, &metadata.Bucket{
			ID: fmt.Sprintf("b%d", i), Name: fmt.Sprintf("bkt-%d", i),
			Owner: "owner1", Status: metadata.BucketStatusReady,
		}))
	}

	err := checker.CanCreateBucket(ctx, "owner1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "per-tenant bucket limit")
}

func TestCanCreateCollection(t *testing.T) {
	checker, store := newTestChecker(t)
	ctx := context.Background()

	require.NoError(t, store.CreateBucket(ctx, &metadata.Bucket{
		ID: "b1", Name: "bkt", Owner: "u1", Status: metadata.BucketStatusReady,
	}))

	err := checker.CanCreateCollection(ctx, "b1")
	assert.NoError(t, err)
}

func TestCanCreateCollectionExceedsLimit(t *testing.T) {
	checker, store := newTestChecker(t)
	ctx := context.Background()

	require.NoError(t, store.CreateBucket(ctx, &metadata.Bucket{
		ID: "b1", Name: "bkt", Owner: "u1", Status: metadata.BucketStatusReady,
	}))

	for i := 0; i < 50; i++ {
		require.NoError(t, store.CreateCollection(ctx, &metadata.LogicalCollection{
			ID: fmt.Sprintf("c%d", i), BucketID: "b1", Name: fmt.Sprintf("coll-%d", i),
			Dim: 768, Metric: "COSINE", Status: metadata.CollStatusReady,
			PhysicalName: fmt.Sprintf("vb_b1_c%d", i),
		}))
	}

	err := checker.CanCreateCollection(ctx, "b1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "collection limit")
}

func TestCheckDimension(t *testing.T) {
	checker, _ := newTestChecker(t)
	assert.NoError(t, checker.CheckDimension(768))
	assert.NoError(t, checker.CheckDimension(1536))
	assert.Error(t, checker.CheckDimension(2048))
	assert.Error(t, checker.CheckDimension(0))
}

func TestCheckVectorCount(t *testing.T) {
	checker, _ := newTestChecker(t)
	assert.NoError(t, checker.CheckVectorCount(999999, 1))
	assert.Error(t, checker.CheckVectorCount(999999, 2)) // 999999 + 2 > 1000000
}
```

- [ ] **步骤 2: 运行测试确认失败**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 ./internal/vectorbucket/quota/...`
预期: FAIL

- [ ] **步骤 3: 实现 Checker**

```go
// internal/vectorbucket/quota/quota.go
package quota

import (
	"context"
	"fmt"

	"github.com/milvus-io/milvus/internal/vectorbucket/config"
	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
)

type Checker struct {
	store metadata.Store
	cfg   *config.Config
}

func NewChecker(store metadata.Store, cfg *config.Config) *Checker {
	return &Checker{store: store, cfg: cfg}
}

func (c *Checker) CanCreateBucket(ctx context.Context, owner string) error {
	globalCount, err := c.store.CountBuckets(ctx)
	if err != nil {
		return fmt.Errorf("count buckets: %w", err)
	}
	if globalCount >= c.cfg.MaxBucketsGlobal {
		return fmt.Errorf("global bucket limit reached (%d)", c.cfg.MaxBucketsGlobal)
	}

	ownerCount, err := c.store.CountBucketsByOwner(ctx, owner)
	if err != nil {
		return fmt.Errorf("count buckets by owner: %w", err)
	}
	if ownerCount >= c.cfg.MaxBucketsPerTenant {
		return fmt.Errorf("per-tenant bucket limit reached (%d)", c.cfg.MaxBucketsPerTenant)
	}

	return nil
}

func (c *Checker) CanCreateCollection(ctx context.Context, bucketID string) error {
	cnt, err := c.store.CountCollections(ctx, bucketID)
	if err != nil {
		return fmt.Errorf("count collections: %w", err)
	}
	if cnt >= c.cfg.MaxCollPerBucket {
		return fmt.Errorf("collection limit per bucket reached (%d)", c.cfg.MaxCollPerBucket)
	}
	return nil
}

func (c *Checker) CheckDimension(dim int) error {
	if dim <= 0 {
		return fmt.Errorf("dimension must be positive, got %d", dim)
	}
	if dim > c.cfg.MaxDim {
		return fmt.Errorf("dimension %d exceeds maximum %d", dim, c.cfg.MaxDim)
	}
	return nil
}

func (c *Checker) CheckVectorCount(currentCount int64, insertCount int) error {
	if currentCount+int64(insertCount) > int64(c.cfg.MaxVectorsPerColl) {
		return fmt.Errorf("insert would exceed max vectors per collection (%d)", c.cfg.MaxVectorsPerColl)
	}
	return nil
}

func (c *Checker) CheckMetric(metric string) error {
	switch metric {
	case "COSINE", "L2":
		return nil
	default:
		return fmt.Errorf("unsupported metric type %q, must be COSINE or L2", metric)
	}
}
```

- [ ] **步骤 4: 运行测试确认通过**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 -v ./internal/vectorbucket/quota/...`
预期: PASS

- [ ] **步骤 5: 提交**

```bash
git add internal/vectorbucket/quota/
git commit -s -m "feat(vectorbucket): add quota enforcement for buckets, collections, dimensions, and vector counts

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 8：Prometheus 监控指标

**文件:**
- 新建: `internal/vectorbucket/metrics/metrics.go`

- [ ] **步骤 1: 定义指标**

```go
// internal/vectorbucket/metrics/metrics.go
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	BucketCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "vb_bucket_count",
		Help: "活跃 bucket 数量",
	})

	LogicalCollectionCount = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vb_logical_collection_count",
		Help: "按状态统计的 logical collection 数量",
	}, []string{"status"})

	LoadedCollectionCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "vb_loaded_collection_count",
		Help: "当前已 load 的 collection 数量",
	})

	LoadDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "vb_load_duration_seconds",
		Help:    "LoadCollection 调用耗时",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 10), // 0.1s 到 ~51s
	})

	QueryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vb_query_duration_seconds",
		Help:    "查询各阶段耗时",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms 到 ~16s
	}, []string{"phase"})

	ReleaseEvictions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vb_release_evictions_total",
		Help: "按原因统计的 collection release 次数",
	}, []string{"reason"})

	CollectionMemEstimate = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vb_collection_mem_estimate_mb",
		Help: "单 collection 预估内存占用（MB）",
	}, []string{"collection"})

	InsertTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vb_insert_total",
		Help: "插入向量总数",
	}, []string{"bucket", "collection"})

	QueryTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vb_query_total",
		Help: "查询请求总数",
	}, []string{"bucket", "collection"})

	ErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vb_error_total",
		Help: "按类型统计的错误总数",
	}, []string{"type"})
)
```

- [ ] **步骤 2: 验证编译通过**

运行: `cd /root/xty/milvus && go build ./internal/vectorbucket/metrics/`
预期: 成功

- [ ] **步骤 3: 提交**

```bash
git add internal/vectorbucket/metrics/
git commit -s -m "feat(vectorbucket): add Prometheus metric definitions

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 9：统一错误响应

**文件:**
- 新建: `internal/vectorbucket/gateway/errors.go`

- [ ] **步骤 1: 编写错误响应辅助函数**

```go
// internal/vectorbucket/gateway/errors.go
package gateway

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

type ErrorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func respondError(c *gin.Context, code int, msg string) {
	c.JSON(code, ErrorResponse{Code: code, Message: msg})
}

func respondNotFound(c *gin.Context, msg string) {
	respondError(c, http.StatusNotFound, msg)
}

func respondBadRequest(c *gin.Context, msg string) {
	respondError(c, http.StatusBadRequest, msg)
}

func respondQuotaExceeded(c *gin.Context, msg string) {
	respondError(c, http.StatusTooManyRequests, msg)
}

func respondServiceUnavailable(c *gin.Context, msg string, retryAfterSec int) {
	c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
	respondError(c, http.StatusServiceUnavailable, msg)
}
```

- [ ] **步骤 2: 验证编译通过**

运行: `cd /root/xty/milvus && go build ./internal/vectorbucket/gateway/`
预期: 成功

- [ ] **步骤 3: 提交**

```bash
git add internal/vectorbucket/gateway/errors.go
git commit -s -m "feat(vectorbucket): add HTTP error response helpers

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 10：Bucket HTTP Handler

**文件:**
- 新建: `internal/vectorbucket/gateway/server.go`
- 新建: `internal/vectorbucket/gateway/handlers_bucket.go`
- 新建: `internal/vectorbucket/gateway/handlers_bucket_test.go`

- [ ] **步骤 1: 编写 Gateway Server 结构体**

```go
// internal/vectorbucket/gateway/server.go
package gateway

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/milvus-io/milvus/internal/vectorbucket/adapter"
	"github.com/milvus-io/milvus/internal/vectorbucket/config"
	"github.com/milvus-io/milvus/internal/vectorbucket/controller"
	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
	"github.com/milvus-io/milvus/internal/vectorbucket/quota"
	"github.com/milvus-io/milvus/internal/vectorbucket/router"
)

type Server struct {
	cfg        *config.Config
	engine     *gin.Engine
	httpServer *http.Server
	store      metadata.Store
	adapter    adapter.Adapter
	router     *router.NamespaceRouter
	controller *controller.LoadController
	quota      *quota.Checker
}

func NewServer(cfg *config.Config, store metadata.Store, milvusAdapter adapter.Adapter, ctrl *controller.LoadController) *Server {
	s := &Server{
		cfg:        cfg,
		store:      store,
		adapter:    milvusAdapter,
		router:     router.NewNamespaceRouter(store),
		controller: ctrl,
		quota:      quota.NewChecker(store, cfg),
	}

	gin.SetMode(gin.ReleaseMode)
	s.engine = gin.New()
	s.engine.Use(gin.Recovery())

	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	v1 := s.engine.Group("/v1")

	// Bucket
	v1.POST("/buckets", s.CreateBucket)
	v1.GET("/buckets/:bucket", s.GetBucket)
	v1.DELETE("/buckets/:bucket", s.DeleteBucket)

	// Collection
	v1.POST("/buckets/:bucket/collections", s.CreateCollection)
	v1.DELETE("/buckets/:bucket/collections/:collection", s.DeleteCollection)

	// Vector
	v1.POST("/buckets/:bucket/collections/:collection/vectors", s.PutVectors)
	v1.POST("/buckets/:bucket/collections/:collection/vectors:upsert", s.UpsertVectors)
	v1.POST("/buckets/:bucket/collections/:collection/vectors:delete", s.DeleteVectors)

	// 查询
	v1.POST("/buckets/:bucket/collections/:collection/query", s.QueryVectors)

	// 指标
	s.engine.GET("/metrics", gin.WrapH(promhttp.Handler()))
}

func (s *Server) Start() error {
	s.httpServer = &http.Server{
		Addr:    s.cfg.ListenAddr,
		Handler: s.engine,
	}
	return s.httpServer.ListenAndServe()
}

func (s *Server) Stop(ctx context.Context) error {
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

func (s *Server) Engine() *gin.Engine {
	return s.engine
}
```

- [ ] **步骤 2: 编写 Bucket Handler**

```go
// internal/vectorbucket/gateway/handlers_bucket.go
package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
)

type CreateBucketRequest struct {
	Name string `json:"name" binding:"required"`
}

type BucketResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func bucketToResponse(b *metadata.Bucket) BucketResponse {
	return BucketResponse{
		ID:        b.ID,
		Name:      b.Name,
		Status:    string(b.Status),
		CreatedAt: b.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

func (s *Server) CreateBucket(c *gin.Context) {
	var req CreateBucketRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "invalid request: "+err.Error())
		return
	}

	// TODO: 从鉴权上下文获取 owner；当前使用占位符
	owner := "default"

	if err := s.quota.CanCreateBucket(c.Request.Context(), owner); err != nil {
		respondQuotaExceeded(c, err.Error())
		return
	}

	bucket := &metadata.Bucket{
		ID:     uuid.New().String(),
		Name:   req.Name,
		Owner:  owner,
		Status: metadata.BucketStatusReady,
	}

	if err := s.store.CreateBucket(c.Request.Context(), bucket); err != nil {
		respondError(c, http.StatusConflict, "bucket already exists or internal error: "+err.Error())
		return
	}

	c.JSON(http.StatusCreated, bucketToResponse(bucket))
}

func (s *Server) GetBucket(c *gin.Context) {
	bucketName := c.Param("bucket")

	bucket, err := s.store.GetBucketByName(c.Request.Context(), bucketName)
	if err != nil {
		respondNotFound(c, "bucket not found")
		return
	}

	c.JSON(http.StatusOK, bucketToResponse(bucket))
}

func (s *Server) DeleteBucket(c *gin.Context) {
	bucketName := c.Param("bucket")

	bucket, err := s.store.GetBucketByName(c.Request.Context(), bucketName)
	if err != nil {
		respondNotFound(c, "bucket not found")
		return
	}

	// 检查 bucket 是否为空
	cnt, err := s.store.CountCollections(c.Request.Context(), bucket.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to check collections")
		return
	}
	if cnt > 0 {
		respondBadRequest(c, "cannot delete non-empty bucket; delete all collections first")
		return
	}

	if err := s.store.DeleteBucket(c.Request.Context(), bucket.ID); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete bucket")
		return
	}

	c.Status(http.StatusNoContent)
}
```

- [ ] **步骤 3: 编写 Bucket Handler 测试**

```go
// internal/vectorbucket/gateway/handlers_bucket_test.go
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/milvus-io/milvus/internal/vectorbucket/config"
	"github.com/milvus-io/milvus/internal/vectorbucket/controller"
	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
)

// testServer 创建一个使用 SQLite 存储、无真实 Milvus adapter 的 Server
func testServer(t *testing.T) *Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store := metadata.NewSQLiteStore(dbPath)
	require.NoError(t, store.Init(context.Background()))
	t.Cleanup(func() { store.Close() })

	cfg := config.DefaultConfig()
	ctrl := controller.NewLoadController(nil, cfg.LoadBudgetMB, 30*60, cfg.MaxLoadedColls)
	return NewServer(&cfg, store, nil, ctrl)
}

func TestCreateBucket(t *testing.T) {
	s := testServer(t)
	body, _ := json.Marshal(CreateBucketRequest{Name: "test-bucket"})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp BucketResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "test-bucket", resp.Name)
	assert.Equal(t, "READY", resp.Status)
	assert.NotEmpty(t, resp.ID)
}

func TestGetBucket(t *testing.T) {
	s := testServer(t)

	// 先创建
	body, _ := json.Marshal(CreateBucketRequest{Name: "test-bucket"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// 查询
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/buckets/test-bucket", nil)
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp BucketResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "test-bucket", resp.Name)
}

func TestGetBucketNotFound(t *testing.T) {
	s := testServer(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/buckets/nonexistent", nil)
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteEmptyBucket(t *testing.T) {
	s := testServer(t)

	// 创建
	body, _ := json.Marshal(CreateBucketRequest{Name: "del-bucket"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// 删除
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/buckets/del-bucket", nil)
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestDeleteBucketNotFound(t *testing.T) {
	s := testServer(t)

	w := httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/buckets/does-not-exist", nil)
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}
```

- [ ] **步骤 4: 运行测试**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 -v ./internal/vectorbucket/gateway/...`
预期: PASS

- [ ] **步骤 5: 提交**

```bash
git add internal/vectorbucket/gateway/server.go internal/vectorbucket/gateway/handlers_bucket.go internal/vectorbucket/gateway/handlers_bucket_test.go
git commit -s -m "feat(vectorbucket): add Gin server scaffold and bucket CRUD HTTP handlers

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 11：Collection HTTP Handler

**文件:**
- 新建: `internal/vectorbucket/gateway/handlers_collection.go`
- 新建: `internal/vectorbucket/gateway/handlers_collection_test.go`

- [ ] **步骤 1: 编写 Collection Handler**

```go
// internal/vectorbucket/gateway/handlers_collection.go
package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
	"github.com/milvus-io/milvus/internal/vectorbucket/router"
)

type CreateCollectionRequest struct {
	Name   string `json:"name" binding:"required"`
	Dim    int    `json:"dim" binding:"required"`
	Metric string `json:"metric" binding:"required"`
}

type CollectionResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Dim       int    `json:"dim"`
	Metric    string `json:"metric"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func collToResponse(c *metadata.LogicalCollection) CollectionResponse {
	return CollectionResponse{
		ID:        c.ID,
		Name:      c.Name,
		Dim:       c.Dim,
		Metric:    c.Metric,
		Status:    string(c.Status),
		CreatedAt: c.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

func (s *Server) CreateCollection(c *gin.Context) {
	bucketName := c.Param("bucket")
	ctx := c.Request.Context()

	var req CreateCollectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "invalid request: "+err.Error())
		return
	}

	if err := s.quota.CheckDimension(req.Dim); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := s.quota.CheckMetric(req.Metric); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	bucket, err := s.store.GetBucketByName(ctx, bucketName)
	if err != nil {
		respondNotFound(c, "bucket not found")
		return
	}

	if err := s.quota.CanCreateCollection(ctx, bucket.ID); err != nil {
		respondQuotaExceeded(c, err.Error())
		return
	}

	collID := uuid.New().String()
	physName := router.PhysicalCollectionName(bucket.ID, collID)

	lc := &metadata.LogicalCollection{
		ID:           collID,
		BucketID:     bucket.ID,
		Name:         req.Name,
		Dim:          req.Dim,
		Metric:       req.Metric,
		Status:       metadata.CollStatusInit,
		PhysicalName: physName,
	}

	if err := s.store.CreateCollection(ctx, lc); err != nil {
		respondError(c, http.StatusConflict, "collection already exists: "+err.Error())
		return
	}

	// 创建物理 Milvus collection（不建索引）
	if s.adapter != nil {
		if err := s.adapter.CreateCollection(ctx, physName, req.Dim, req.Metric); err != nil {
			// 回滚 metadata
			s.store.DeleteCollection(ctx, collID)
			respondError(c, http.StatusInternalServerError, "failed to create Milvus collection: "+err.Error())
			return
		}
	}

	if err := s.store.UpdateCollectionStatus(ctx, collID, metadata.CollStatusReady); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update status")
		return
	}
	lc.Status = metadata.CollStatusReady

	c.JSON(http.StatusCreated, collToResponse(lc))
}

func (s *Server) DeleteCollection(c *gin.Context) {
	bucketName := c.Param("bucket")
	collName := c.Param("collection")
	ctx := c.Request.Context()

	bucket, err := s.store.GetBucketByName(ctx, bucketName)
	if err != nil {
		respondNotFound(c, "bucket not found")
		return
	}

	coll, err := s.store.GetCollection(ctx, bucket.ID, collName)
	if err != nil {
		respondNotFound(c, "collection not found")
		return
	}

	// 标记为删除中
	s.store.UpdateCollectionStatus(ctx, coll.ID, metadata.CollStatusDeleting)

	// 在 Milvus 中 release + drop
	if s.adapter != nil {
		_ = s.adapter.ReleaseCollection(ctx, coll.PhysicalName)
		_ = s.adapter.DropCollection(ctx, coll.PhysicalName)
	}

	// 标记已删除
	s.store.UpdateCollectionStatus(ctx, coll.ID, metadata.CollStatusDeleted)

	c.Status(http.StatusNoContent)
}
```

- [ ] **步骤 2: 编写 Collection Handler 测试**

```go
// internal/vectorbucket/gateway/handlers_collection_test.go
package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestBucket(t *testing.T, s *Server, name string) {
	t.Helper()
	body, _ := json.Marshal(CreateBucketRequest{Name: name})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
}

func TestCreateCollection(t *testing.T) {
	s := testServer(t)
	createTestBucket(t, s, "bkt")

	body, _ := json.Marshal(CreateCollectionRequest{Name: "my-coll", Dim: 768, Metric: "COSINE"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp CollectionResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "my-coll", resp.Name)
	assert.Equal(t, 768, resp.Dim)
	assert.Equal(t, "COSINE", resp.Metric)
	assert.Equal(t, "READY", resp.Status)
}

func TestCreateCollectionBadDim(t *testing.T) {
	s := testServer(t)
	createTestBucket(t, s, "bkt")

	body, _ := json.Marshal(CreateCollectionRequest{Name: "c", Dim: 9999, Metric: "COSINE"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateCollectionBadMetric(t *testing.T) {
	s := testServer(t)
	createTestBucket(t, s, "bkt")

	body, _ := json.Marshal(CreateCollectionRequest{Name: "c", Dim: 768, Metric: "INVALID"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateCollectionBucketNotFound(t *testing.T) {
	s := testServer(t)

	body, _ := json.Marshal(CreateCollectionRequest{Name: "c", Dim: 768, Metric: "COSINE"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/nonexistent/collections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteCollection(t *testing.T) {
	s := testServer(t)
	createTestBucket(t, s, "bkt")

	// 创建 collection
	body, _ := json.Marshal(CreateCollectionRequest{Name: "del-coll", Dim: 768, Metric: "COSINE"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// 删除 collection
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/buckets/bkt/collections/del-coll", nil)
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestDeleteNonEmptyBucketFails(t *testing.T) {
	s := testServer(t)
	createTestBucket(t, s, "bkt")

	// 创建 collection
	body, _ := json.Marshal(CreateCollectionRequest{Name: "coll", Dim: 768, Metric: "COSINE"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// 尝试删除非空 bucket
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/buckets/bkt", nil)
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
```

- [ ] **步骤 3: 运行测试**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 -v ./internal/vectorbucket/gateway/...`
预期: PASS

- [ ] **步骤 4: 提交**

```bash
git add internal/vectorbucket/gateway/handlers_collection.go internal/vectorbucket/gateway/handlers_collection_test.go
git commit -s -m "feat(vectorbucket): add collection create/delete HTTP handlers with quota checks

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 12：Vector 写入 Handler（Put / Upsert / Delete）

**文件:**
- 新建: `internal/vectorbucket/gateway/handlers_vector.go`
- 新建: `internal/vectorbucket/gateway/handlers_vector_test.go`

- [ ] **步骤 1: 编写 Vector Handler**

```go
// internal/vectorbucket/gateway/handlers_vector.go
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
	"github.com/milvus-io/milvus/internal/vectorbucket/metrics"
)

type VectorEntry struct {
	ID       string          `json:"id" binding:"required"`
	Vector   []float32       `json:"vector" binding:"required"`
	Metadata json.RawMessage `json:"metadata"`
}

type PutVectorsRequest struct {
	Vectors []VectorEntry `json:"vectors" binding:"required,dive"`
}

type DeleteVectorsRequest struct {
	IDs    []string `json:"ids"`
	Filter string   `json:"filter"`
}

func (s *Server) PutVectors(c *gin.Context) {
	s.writeVectors(c, false)
}

func (s *Server) UpsertVectors(c *gin.Context) {
	s.writeVectors(c, true)
}

func (s *Server) writeVectors(c *gin.Context, upsert bool) {
	bucketName := c.Param("bucket")
	collName := c.Param("collection")
	ctx := c.Request.Context()

	var req PutVectorsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "invalid request: "+err.Error())
		return
	}

	if len(req.Vectors) == 0 {
		respondBadRequest(c, "vectors array cannot be empty")
		return
	}

	lc, err := s.router.Resolve(ctx, bucketName, collName)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	// 验证维度
	for _, v := range req.Vectors {
		if len(v.Vector) != lc.Dim {
			respondBadRequest(c, fmt.Sprintf("vector dimension mismatch: expected %d, got %d", lc.Dim, len(v.Vector)))
			return
		}
	}

	// 配额检查
	if !upsert {
		if err := s.quota.CheckVectorCount(lc.VectorCount, len(req.Vectors)); err != nil {
			respondQuotaExceeded(c, err.Error())
			return
		}
	}

	// 准备批量数据
	ids := make([]string, len(req.Vectors))
	vectors := make([][]float32, len(req.Vectors))
	metadataBytes := make([][]byte, len(req.Vectors))
	timestamps := make([]int64, len(req.Vectors))
	now := time.Now().UnixMilli()

	for i, v := range req.Vectors {
		ids[i] = v.ID
		vectors[i] = v.Vector
		if v.Metadata != nil {
			metadataBytes[i] = []byte(v.Metadata)
		} else {
			metadataBytes[i] = []byte("{}")
		}
		timestamps[i] = now
	}

	if s.adapter != nil {
		if upsert {
			err = s.adapter.Upsert(ctx, lc.PhysicalName, ids, vectors, metadataBytes, timestamps)
		} else {
			err = s.adapter.Insert(ctx, lc.PhysicalName, ids, vectors, metadataBytes, timestamps)
		}
		if err != nil {
			respondError(c, http.StatusInternalServerError, "write failed: "+err.Error())
			return
		}
	}

	// 更新向量计数
	if !upsert {
		s.store.UpdateCollectionVectorCount(ctx, lc.ID, int64(len(req.Vectors)))
	}

	// 累积达到阈值后触发异步建索引
	if !lc.IndexBuilt {
		newCount := lc.VectorCount + int64(len(req.Vectors))
		if newCount >= int64(s.cfg.IndexBuildThreshold) {
			go s.buildIndex(lc)
		}
	}

	metrics.InsertTotal.WithLabelValues(bucketName, collName).Add(float64(len(req.Vectors)))
	c.JSON(http.StatusOK, gin.H{"inserted": len(req.Vectors)})
}

func (s *Server) buildIndex(lc *metadata.LogicalCollection) {
	if s.adapter == nil {
		return
	}
	ctx := context.Background()
	if err := s.adapter.CreateIndex(ctx, lc.PhysicalName, lc.VectorCount, lc.Metric); err != nil {
		return
	}
	s.store.UpdateCollectionIndexBuilt(ctx, lc.ID, true)
}

func (s *Server) DeleteVectors(c *gin.Context) {
	bucketName := c.Param("bucket")
	collName := c.Param("collection")
	ctx := c.Request.Context()

	var req DeleteVectorsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "invalid request: "+err.Error())
		return
	}

	if len(req.IDs) == 0 && req.Filter == "" {
		respondBadRequest(c, "must provide ids or filter")
		return
	}

	lc, err := s.router.Resolve(ctx, bucketName, collName)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	if s.adapter != nil && len(req.IDs) > 0 {
		if err := s.adapter.Delete(ctx, lc.PhysicalName, req.IDs); err != nil {
			respondError(c, http.StatusInternalServerError, "delete failed: "+err.Error())
			return
		}
		s.store.UpdateCollectionVectorCount(ctx, lc.ID, -int64(len(req.IDs)))
	}

	c.JSON(http.StatusOK, gin.H{"deleted": len(req.IDs)})
}
```

- [ ] **步骤 2: 编写测试（无真实 Milvus — adapter=nil 路径）**

```go
// internal/vectorbucket/gateway/handlers_vector_test.go
package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupBucketAndColl(t *testing.T, s *Server) {
	t.Helper()
	createTestBucket(t, s, "bkt")

	body, _ := json.Marshal(CreateCollectionRequest{Name: "coll", Dim: 3, Metric: "COSINE"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
}

func TestPutVectors(t *testing.T) {
	s := testServer(t)
	setupBucketAndColl(t, s)

	body, _ := json.Marshal(PutVectorsRequest{
		Vectors: []VectorEntry{
			{ID: "v1", Vector: []float32{0.1, 0.2, 0.3}, Metadata: json.RawMessage(`{"key":"val"}`)},
			{ID: "v2", Vector: []float32{0.4, 0.5, 0.6}},
		},
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections/coll/vectors", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestPutVectorsDimMismatch(t *testing.T) {
	s := testServer(t)
	setupBucketAndColl(t, s)

	body, _ := json.Marshal(PutVectorsRequest{
		Vectors: []VectorEntry{
			{ID: "v1", Vector: []float32{0.1, 0.2}}, // dim=2，期望 3
		},
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections/coll/vectors", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPutVectorsCollectionNotFound(t *testing.T) {
	s := testServer(t)
	createTestBucket(t, s, "bkt")

	body, _ := json.Marshal(PutVectorsRequest{
		Vectors: []VectorEntry{
			{ID: "v1", Vector: []float32{0.1, 0.2, 0.3}},
		},
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections/no-coll/vectors", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteVectors(t *testing.T) {
	s := testServer(t)
	setupBucketAndColl(t, s)

	body, _ := json.Marshal(DeleteVectorsRequest{IDs: []string{"v1", "v2"}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections/coll/vectors:delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteVectorsNoIDsOrFilter(t *testing.T) {
	s := testServer(t)
	setupBucketAndColl(t, s)

	body, _ := json.Marshal(DeleteVectorsRequest{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections/coll/vectors:delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
```

- [ ] **步骤 3: 运行测试**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 -v ./internal/vectorbucket/gateway/...`
预期: PASS

- [ ] **步骤 4: 提交**

```bash
git add internal/vectorbucket/gateway/handlers_vector.go internal/vectorbucket/gateway/handlers_vector_test.go
git commit -s -m "feat(vectorbucket): add vector put/upsert/delete HTTP handlers with dim validation

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 13：Query Handler（含 Load/Release 集成）

**文件:**
- 新建: `internal/vectorbucket/gateway/handlers_query.go`
- 新建: `internal/vectorbucket/gateway/handlers_query_test.go`

- [ ] **步骤 1: 编写查询 Handler**

```go
// internal/vectorbucket/gateway/handlers_query.go
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/milvus-io/milvus/internal/vectorbucket/controller"
	"github.com/milvus-io/milvus/internal/vectorbucket/metrics"
)

type QueryRequest struct {
	Vector []float32 `json:"vector" binding:"required"`
	TopK   int       `json:"topK" binding:"required"`
	Filter string    `json:"filter"`
	Nprobe int       `json:"nprobe"`
}

type QueryResultItem struct {
	ID       string          `json:"id"`
	Score    float32         `json:"score"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

func (s *Server) QueryVectors(c *gin.Context) {
	bucketName := c.Param("bucket")
	collName := c.Param("collection")
	ctx := c.Request.Context()

	var req QueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "invalid request: "+err.Error())
		return
	}

	if req.TopK < 1 || req.TopK > 30 {
		respondBadRequest(c, "topK must be between 1 and 30")
		return
	}
	if req.Nprobe <= 0 {
		req.Nprobe = 16 // 默认值
	}

	lc, err := s.router.Resolve(ctx, bucketName, collName)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	if len(req.Vector) != lc.Dim {
		respondBadRequest(c, fmt.Sprintf("query vector dimension mismatch: expected %d, got %d", lc.Dim, len(req.Vector)))
		return
	}

	// 确保 collection 已 load
	estMem := controller.EstimateMemMB(lc.VectorCount, lc.Dim)
	loadStart := time.Now()
	if err := s.controller.EnsureLoaded(ctx, lc.PhysicalName, estMem); err != nil {
		respondServiceUnavailable(c, "load failed: "+err.Error(), 5)
		return
	}
	loadDur := time.Since(loadStart)
	if loadDur > 100*time.Millisecond {
		metrics.LoadDuration.Observe(loadDur.Seconds())
	}

	// 追踪 in-flight 查询
	s.controller.InFlightInc(lc.PhysicalName)
	defer s.controller.InFlightDec(lc.PhysicalName)
	s.controller.Touch(lc.PhysicalName)

	// 更新 metadata 中的最近访问时间
	s.store.UpdateCollectionLastAccess(ctx, lc.ID)

	if s.adapter == nil {
		// 无 Milvus adapter — 返回空结果（测试模式）
		c.JSON(http.StatusOK, []QueryResultItem{})
		return
	}

	searchStart := time.Now()
	results, err := s.adapter.Search(ctx, lc.PhysicalName, req.Vector, req.TopK, req.Nprobe, req.Filter, lc.Metric)
	searchDur := time.Since(searchStart)
	metrics.QueryDuration.WithLabelValues("search").Observe(searchDur.Seconds())

	if err != nil {
		respondError(c, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}

	out := make([]QueryResultItem, len(results))
	for i, r := range results {
		out[i] = QueryResultItem{
			ID:       r.ID,
			Score:    r.Score,
			Metadata: json.RawMessage(r.Metadata),
		}
	}

	metrics.QueryTotal.WithLabelValues(bucketName, collName).Inc()
	c.JSON(http.StatusOK, out)
}
```

- [ ] **步骤 2: 编写查询 Handler 测试**

```go
// internal/vectorbucket/gateway/handlers_query_test.go
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/milvus-io/milvus/internal/vectorbucket/config"
	"github.com/milvus-io/milvus/internal/vectorbucket/controller"
	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
)

// mockLoadReleaser 用于控制器测试
type mockLoadReleaser struct{}

func (m *mockLoadReleaser) LoadCollection(ctx context.Context, name string) error    { return nil }
func (m *mockLoadReleaser) ReleaseCollection(ctx context.Context, name string) error { return nil }

func testServerWithController(t *testing.T) *Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store := metadata.NewSQLiteStore(dbPath)
	require.NoError(t, store.Init(context.Background()))
	t.Cleanup(func() { store.Close() })

	cfg := config.DefaultConfig()
	ctrl := controller.NewLoadController(&mockLoadReleaser{}, cfg.LoadBudgetMB, 30*time.Minute, cfg.MaxLoadedColls)
	return NewServer(&cfg, store, nil, ctrl)
}

func TestQueryVectors(t *testing.T) {
	s := testServerWithController(t)
	createTestBucket(t, s, "bkt")

	// 创建 dim=3 的 collection
	body, _ := json.Marshal(CreateCollectionRequest{Name: "coll", Dim: 3, Metric: "COSINE"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// 查询
	body, _ = json.Marshal(QueryRequest{Vector: []float32{0.1, 0.2, 0.3}, TopK: 5})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/v1/buckets/bkt/collections/coll/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestQueryVectorsTopKTooLarge(t *testing.T) {
	s := testServerWithController(t)
	createTestBucket(t, s, "bkt")

	body, _ := json.Marshal(CreateCollectionRequest{Name: "coll", Dim: 3, Metric: "COSINE"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	body, _ = json.Marshal(QueryRequest{Vector: []float32{0.1, 0.2, 0.3}, TopK: 100})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/v1/buckets/bkt/collections/coll/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestQueryVectorsDimMismatch(t *testing.T) {
	s := testServerWithController(t)
	createTestBucket(t, s, "bkt")

	body, _ := json.Marshal(CreateCollectionRequest{Name: "coll", Dim: 3, Metric: "COSINE"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets/bkt/collections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	body, _ = json.Marshal(QueryRequest{Vector: []float32{0.1, 0.2}, TopK: 5}) // dim=2 != 3
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/v1/buckets/bkt/collections/coll/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Engine().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
```

- [ ] **步骤 3: 运行测试**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 -v ./internal/vectorbucket/gateway/...`
预期: PASS

- [ ] **步骤 4: 提交**

```bash
git add internal/vectorbucket/gateway/handlers_query.go internal/vectorbucket/gateway/handlers_query_test.go
git commit -s -m "feat(vectorbucket): add query handler with Load/Release Controller integration

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 14：Main 入口（串联所有组件）

**文件:**
- 修改: `internal/vectorbucket/cmd/main.go`

- [ ] **步骤 1: 更新 main.go 串联所有组件**

```go
// internal/vectorbucket/cmd/main.go
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/vectorbucket/adapter"
	"github.com/milvus-io/milvus/internal/vectorbucket/config"
	"github.com/milvus-io/milvus/internal/vectorbucket/controller"
	"github.com/milvus-io/milvus/internal/vectorbucket/gateway"
	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
	"github.com/milvus-io/milvus/pkg/v2/log"
)

func main() {
	cfg := config.LoadConfig()

	// 初始化 metadata 存储
	store := metadata.NewSQLiteStore(cfg.SQLitePath)
	if err := store.Init(context.Background()); err != nil {
		log.Fatal("failed to init metadata store", zap.Error(err))
	}
	defer store.Close()

	// 初始化 Milvus 适配层
	milvusAdapter, err := adapter.NewMilvusAdapter(cfg.MilvusAddr)
	if err != nil {
		log.Fatal("failed to connect to Milvus", zap.Error(err))
	}
	defer milvusAdapter.Close()

	// 初始化 Load/Release 控制器
	ttl := time.Duration(cfg.TTLSeconds) * time.Second
	ctrl := controller.NewLoadController(milvusAdapter, cfg.LoadBudgetMB, ttl, cfg.MaxLoadedColls)

	// 启动 TTL 扫描循环
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.StartTTLSweepLoop(ctx, 60*time.Second)

	// 初始化 Gateway 服务
	srv := gateway.NewServer(&cfg, store, milvusAdapter, ctrl)

	// 优雅关闭
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Info("shutting down Vector Bucket Gateway...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		srv.Stop(shutdownCtx)
		cancel()
	}()

	log.Info("Vector Bucket Gateway starting", zap.String("addr", cfg.ListenAddr))
	if err := srv.Start(); err != nil && err.Error() != "http: Server closed" {
		log.Fatal("server error", zap.Error(err))
	}
}
```

- [ ] **步骤 2: 验证编译通过**

运行: `cd /root/xty/milvus && go build ./internal/vectorbucket/cmd/`
预期: 成功

- [ ] **步骤 3: 提交**

```bash
git add internal/vectorbucket/cmd/main.go
git commit -s -m "feat(vectorbucket): wire all components in main entry point with graceful shutdown

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 15：全量测试 + 修复问题

- [ ] **步骤 1: 运行所有 vectorbucket 测试**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 -v ./internal/vectorbucket/...`
预期: 全部 PASS

- [ ] **步骤 2: 运行 go vet**

运行: `cd /root/xty/milvus && go vet ./internal/vectorbucket/...`
预期: 无问题

- [ ] **步骤 3: 修复步骤 1-2 发现的编译或测试错误**

常见问题：
- 缺少 import（补充 `"fmt"`、`"encoding/json"`、`"context"` 等）
- import 循环（adapter 接口如有需要应独立文件）
- controller 构造函数中 `time.Duration` 和 `int` 类型不匹配

- [ ] **步骤 4: 如有修复则提交**

```bash
git add internal/vectorbucket/
git commit -s -m "fix(vectorbucket): fix compilation and test issues from full test suite run

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 任务 16：验证完整 API 契约

- [ ] **步骤 1: 编写端到端 API 契约测试**

```go
// internal/vectorbucket/integration/api_test.go
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/milvus-io/milvus/internal/vectorbucket/config"
	"github.com/milvus-io/milvus/internal/vectorbucket/controller"
	"github.com/milvus-io/milvus/internal/vectorbucket/gateway"
	"github.com/milvus-io/milvus/internal/vectorbucket/metadata"
)

type mockLR struct{}
func (m *mockLR) LoadCollection(ctx context.Context, name string) error    { return nil }
func (m *mockLR) ReleaseCollection(ctx context.Context, name string) error { return nil }

func TestFullAPIFlow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store := metadata.NewSQLiteStore(dbPath)
	require.NoError(t, store.Init(context.Background()))
	defer store.Close()

	cfg := config.DefaultConfig()
	ctrl := controller.NewLoadController(&mockLR{}, cfg.LoadBudgetMB, 30*time.Minute, cfg.MaxLoadedColls)
	srv := gateway.NewServer(&cfg, store, nil, ctrl)
	engine := srv.Engine()

	// 1. 创建 bucket
	body, _ := json.Marshal(map[string]string{"name": "test-bucket"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/buckets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)

	// 2. 查询 bucket
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/buckets/test-bucket", nil)
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 3. 创建 collection
	body, _ = json.Marshal(map[string]any{"name": "vectors", "dim": 4, "metric": "COSINE"})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/v1/buckets/test-bucket/collections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)

	// 4. 写入向量
	body, _ = json.Marshal(map[string]any{
		"vectors": []map[string]any{
			{"id": "v1", "vector": []float32{0.1, 0.2, 0.3, 0.4}, "metadata": map[string]string{"tag": "a"}},
			{"id": "v2", "vector": []float32{0.5, 0.6, 0.7, 0.8}},
		},
	})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/v1/buckets/test-bucket/collections/vectors/vectors", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 5. 查询向量
	body, _ = json.Marshal(map[string]any{"vector": []float32{0.1, 0.2, 0.3, 0.4}, "topK": 5})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/v1/buckets/test-bucket/collections/vectors/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 6. 删除向量
	body, _ = json.Marshal(map[string]any{"ids": []string{"v1"}})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/v1/buckets/test-bucket/collections/vectors/vectors:delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 7. 删除 collection
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/buckets/test-bucket/collections/vectors", nil)
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)

	// 8. 删除 bucket（已空）
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/buckets/test-bucket", nil)
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)

	// 9. 验证 bucket 已不存在
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/buckets/test-bucket", nil)
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}
```

- [ ] **步骤 2: 运行集成测试**

运行: `cd /root/xty/milvus && go test -tags dynamic,test -gcflags="all=-N -l" -count=1 -v ./internal/vectorbucket/integration/...`
预期: PASS

- [ ] **步骤 3: 提交**

```bash
git add internal/vectorbucket/integration/
git commit -s -m "test(vectorbucket): add full API flow integration test covering all Phase 1 endpoints

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## 总览

| 任务 | 组件 | 核心交付物 |
|------|------|-----------|
| 1 | 配置 + 脚手架 | `config.go`、`main.go` 占位 |
| 2 | Metadata 模型 | `models.go`、`store.go` 接口 |
| 3 | SQLite Store | Bucket + Collection 完整 CRUD |
| 4 | Namespace Router | 逻辑名 -> 物理名解析 |
| 5 | Milvus Adapter | 封装 client 的 collection/vector/search 操作 |
| 6 | Load/Release 控制器 | LRU + TTL + 预算 + in-flight 追踪 |
| 7 | 配额管控 | Bucket/Collection/维度/向量数限制 |
| 8 | Prometheus 指标 | Phase 1 全部指标定义 |
| 9 | 错误响应 | 统一 HTTP 错误响应格式 |
| 10 | Bucket Handler | 创建/查询/删除 Bucket HTTP API |
| 11 | Collection Handler | 创建/删除 Collection HTTP API |
| 12 | Vector Handler | Put/Upsert/Delete 向量 HTTP API |
| 13 | Query Handler | 查询 + Load/Release 控制器集成 |
| 14 | Main 入口 | 串联所有组件 + 优雅关闭 |
| 15 | 全量测试 | 修复剩余编译和测试问题 |
| 16 | API 契约测试 | 端到端完整流程验证 |
