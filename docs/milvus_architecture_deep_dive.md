# Milvus 向量数据库架构深度解析

## 目录

- [一、系统架构总览](#一系统架构总览)
  - [1.1 整体架构](#11-整体架构)
  - [1.2 核心组件](#12-核心组件)
  - [1.3 组件通信](#13-组件通信)
  - [1.4 服务发现与会话管理](#14-服务发现与会话管理)
  - [1.5 组件生命周期与状态机](#15-组件生命周期与状态机)
- [二、存储层架构](#二存储层架构)
  - [2.1 元数据存储 (Meta Storage)](#21-元数据存储-meta-storage)
  - [2.2 对象存储 (Object Storage)](#22-对象存储-object-storage)
  - [2.3 消息队列 / WAL](#23-消息队列--wal)
  - [2.4 Binlog 存储格式](#24-binlog-存储格式)
  - [2.5 Segment 文件布局](#25-segment-文件布局)
  - [2.6 索引存储](#26-索引存储)
- [三、缓存与内存管理](#三缓存与内存管理)
  - [3.1 QueryNode 段管理](#31-querynode-段管理)
  - [3.2 Mmap 内存映射](#32-mmap-内存映射)
  - [3.3 DiskCache 磁盘缓存](#33-diskcache-磁盘缓存)
  - [3.4 内存估算与资源管理](#34-内存估算与资源管理)
- [四、插入向量端到端流程](#四插入向量端到端流程)
  - [4.1 流程概览](#41-流程概览)
  - [4.2 Step 1: Proxy 接收请求](#42-step-1-proxy-接收请求)
  - [4.3 Step 2: 主键分配与分片路由](#43-step-2-主键分配与分片路由)
  - [4.4 Step 3: 写入 WAL / 消息队列](#44-step-3-写入-wal--消息队列)
  - [4.5 Step 4: StreamingNode 消费与缓冲](#45-step-4-streamingnode-消费与缓冲)
  - [4.6 Step 5: Segment 封存 (Seal)](#46-step-5-segment-封存-seal)
  - [4.7 Step 6: Flush 到对象存储](#47-step-6-flush-到对象存储)
  - [4.8 Step 7: 索引构建](#48-step-7-索引构建)
  - [4.9 Step 8: 段可查询](#49-step-8-段可查询)
- [五、查询向量端到端流程](#五查询向量端到端流程)
  - [5.1 流程概览](#51-流程概览)
  - [5.2 Step 1: Proxy 接收搜索请求](#52-step-1-proxy-接收搜索请求)
  - [5.3 Step 2: 表达式解析与查询计划生成](#53-step-2-表达式解析与查询计划生成)
  - [5.4 Step 3: 分片路由与负载均衡](#54-step-3-分片路由与负载均衡)
  - [5.5 Step 4: QueryNode 执行搜索](#55-step-4-querynode-执行搜索)
  - [5.6 Step 5: 段数据加载（缓存未命中）](#56-step-5-段数据加载缓存未命中)
  - [5.7 Step 6: C++ Core 向量检索](#57-step-6-c-core-向量检索)
  - [5.8 Step 7: 结果归并 (Reduce)](#58-step-7-结果归并-reduce)
  - [5.9 Query（非向量查询）流程差异](#59-query非向量查询流程差异)
  - [5.10 Hybrid Search（混合搜索）流程](#510-hybrid-search混合搜索流程)
- [六、Delete 处理与 L0 Segment](#六delete-处理与-l0-segment)
- [七、Compaction 压缩机制](#七compaction-压缩机制)
- [八、关键代码路径速查表](#八关键代码路径速查表)

---

## 一、系统架构总览

### 1.1 整体架构

Milvus 采用**存算分离**的分布式架构，由三类外部依赖和四层内部组件组成：

```
┌─────────────────────────────────────────────────────────────────┐
│                        Client SDK                               │
│                  (Python / Java / Go / REST)                    │
└───────────────────────────┬─────────────────────────────────────┘
                            │ gRPC / HTTP
┌───────────────────────────▼─────────────────────────────────────┐
│                      Proxy (接入层)                              │
│          请求路由 · 认证鉴权 · 限流 · 结果归并                     │
└──────┬────────────────┬───────────────────┬─────────────────────┘
       │                │                   │
┌──────▼──────┐  ┌──────▼──────┐  ┌────────▼────────┐
│  MixCoord   │  │  QueryNode  │  │  DataNode       │
│ (协调层)     │  │  (查询节点)  │  │  (数据节点)      │
│             │  │             │  │                  │
│ · RootCoord │  │ · 向量检索   │  │ · 索引构建       │
│ · DataCoord │  │ · 标量过滤   │  │                  │
│ · QueryCoord│  │ · 缓存管理   │  │                  │
│ · Streaming │  │             │  │                  │
│   Coord     │  │             │  │                  │
└──────┬──────┘  └──────┬──────┘  └────────┬────────┘
       │                │                   │
       │         ┌──────▼──────┐            │
       │         │StreamingNode│            │
       │         │ (流式节点)   │            │
       │         │ · WAL 管理   │            │
       │         │ · 消息分发   │            │
       │         └──────┬──────┘            │
       │                │                   │
┌──────▼────────────────▼───────────────────▼─────────────────────┐
│                     外部依赖层                                    │
│  ┌──────────┐  ┌───────────────────┐  ┌──────────────────────┐  │
│  │   etcd   │  │  MinIO / S3       │  │  Pulsar / Kafka /    │  │
│  │ 元数据    │  │  对象存储          │  │  RocksMQ 消息队列     │  │
│  └──────────┘  └───────────────────┘  └──────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
```

### 1.2 核心组件

#### MixCoord（混合协调器）

MixCoord 是 Milvus 的统一协调器，将所有协调角色整合在一个进程中：

| 子协调器 | 职责 | 实现文件 |
|---------|------|---------|
| **RootCoord** | DDL 操作（建库/建表/建索引）、时间戳分配、ID 分配 | `internal/rootcoord/root_coord.go` |
| **DataCoord** | Segment 分配与生命周期管理、Compaction 调度、GC | `internal/datacoord/server.go` |
| **QueryCoord** | Collection 加载/释放、Segment 分配到 QueryNode、负载均衡 | `internal/querycoordv2/server.go` |
| **StreamingCoord** | 流式 WAL 的协调管理 | `internal/streamingcoord/server.go` |

**关键实现**: `internal/coordinator/mix_coord.go`

```go
type mixCoordImpl struct {
    rootcoordServer   *rootcoord.Core
    queryCoordServer  *querycoordv2.Server
    datacoordServer   *datacoord.Server
    streamingCoord    *streamingcoord.Server
    // ...
}
```

MixCoord 同时注册为以下 gRPC 服务：
- `rootcoordpb.RootCoordServer`
- `querypb.QueryCoordServer`
- `datapb.DataCoordServer`
- `indexpb.IndexCoordServer`
- `streamingpb.StreamingCoordServer`

#### Proxy（代理节点）

用户请求的唯一入口，实现 `milvuspb.MilvusServiceServer`。

**职责**：
- 请求验证与参数解析
- 主键分配（Auto-ID）
- 分片路由（Hash 到 VChannel）
- 将请求分发到 QueryNode / DataNode
- 结果归并与返回

**关键实现**: `internal/proxy/impl.go`, `internal/distributed/proxy/service.go`

#### QueryNode（查询节点）

执行向量相似度搜索和标量查询。

**职责**：
- 加载 Sealed Segment（从对象存储）和 Growing Segment（从流式数据）
- 通过 CGO 调用 C++ Knowhere 引擎执行向量检索
- 标量过滤、结果排序
- 段级缓存管理（Mmap / DiskCache）

**关键实现**: `internal/querynodev2/`

#### DataNode（数据节点）

负责索引构建。

**关键实现**: `internal/datanode/`

#### StreamingNode（流式节点）

管理 WAL（Write-Ahead Log），负责消息的持久化和分发。

**职责**：
- 接收 Proxy 写入的消息并持久化到 WAL
- 将消息分发给消费者
- 管理 Growing Segment 的写缓冲和 Flush

**关键实现**: `internal/streamingnode/server/`

### 1.3 组件通信

```
Client ──gRPC/HTTP──► Proxy ──gRPC──► MixCoord (元数据操作)
                        │
                        ├──gRPC──► QueryNode (Search/Query)
                        │
                        └──WAL───► StreamingNode (Insert/Delete)
                                      │
                                      └──Flush──► Object Storage (MinIO/S3)
```

**gRPC 服务定义**:
- Proxy: `internal/distributed/proxy/service.go` — 使用 cmux 在同一端口复用 gRPC + HTTP
- MixCoord: `internal/distributed/mixcoord/service.go`
- 所有 gRPC 服务支持 TLS、otelgrpc 追踪、自定义认证拦截器

### 1.4 服务发现与会话管理

基于 etcd 的服务发现机制。

**Session 结构** (`internal/util/sessionutil/session_util.go`):

```go
type SessionRaw struct {
    ServerID    int64
    ServerName  string
    Address     string
    Exclusive   bool
    Stopping    bool
    Version     semver.Version
    LeaseID     clientv3.LeaseID  // etcd 租约，带 TTL
    HostName    string
}
```

**注册路径**: `session/<server_role>/<server_id>`

**服务发现流程**:
1. 组件启动后调用 `Register()` 在 etcd 注册 Session，附带租约 TTL
2. 其他组件通过 `WatchServices(prefix, revision, rewatch)` 监听服务变更
3. 事件类型：`SessionAddEvent` / `SessionDelEvent` / `SessionUpdateEvent`
4. 协调器支持 Active/Standby 选举：`ProcessActiveStandBy()` 通过 etcd 选主

### 1.5 组件生命周期与状态机

**状态定义** (`commonpb.StateCode`):

```
Initializing ──► Healthy ──► Stopping
     │                          │
     └──► StandBy ──► Healthy   └──► Abnormal
```

**启动流程** (`cmd/milvus/run.go`):

```
解析角色 → printBanner() → injectVariablesToEnv() → printHardwareInfo()
→ createPidFile() → roles.Run()
```

**组件启动**:
```
NewComponent() → Prepare() → Init() → UpdateStateCode(Initializing)
→ Register(etcd) → ProcessActiveStandBy() → activateFunc()
→ Start() → UpdateStateCode(Healthy)
```

**优雅关闭**:
```
SIGTERM/SIGINT → cancel context → drain in-flight requests
→ deregister from etcd → wg.Wait() → cleanup
```

超时由 `paramtable.Get().<Component>Cfg.GracefulStopTimeout` 控制。

---

## 二、存储层架构

### 2.1 元数据存储 (Meta Storage)

元数据通过 **etcd** 或 **TiKV** 存储，接口定义在 `internal/metastore/catalog.go`。

**RootCoordCatalog 接口涵盖**:
- Database CRUD: `CreateDatabase`, `DropDatabase`, `ListDatabases`
- Collection CRUD: `CreateCollection`, `GetCollectionByID`, `DropCollection`
- Partition 管理: `CreatePartition`, `DropPartition`
- 别名管理: `CreateAlias`, `DropAlias`
- RBAC: `CreateRole`, `DropRole`, `AlterGrant`, `ListGrant`
- 索引元数据、字段元数据

**存储后端实现**: `internal/metastore/kv/` 目录下有 rootcoord、datacoord、querycoord 各自的 catalog 实现。

### 2.2 对象存储 (Object Storage)

通过 `ChunkManager` 接口抽象，支持多种后端。

**接口定义** (`internal/storage/types.go`):

```go
type ChunkManager interface {
    RootPath() string
    Write(ctx, filePath string, content []byte) error
    Read(ctx, filePath string) ([]byte, error)
    MultiWrite(ctx, contents map[string][]byte) error
    MultiRead(ctx, filePaths []string) ([][]byte, error)
    Mmap(ctx, filePath string) (*mmap.ReaderAt, error)
    ReadAt(ctx, filePath string, off, length int64) ([]byte, error)
    WalkWithPrefix(ctx, prefix string, recursive bool, walkFunc) error
    Remove(ctx, filePath string) error
    Copy(ctx, srcPath, dstPath string) error
}
```

**两种实现**:
- `LocalChunkManager` (`local_chunk_manager.go`) — 本地文件系统
- `RemoteChunkManager` (`remote_chunk_manager.go`) — MinIO / S3 / Azure Blob / GCP

### 2.3 消息队列 / WAL

**MsgStream 工厂** (`pkg/mq/msgstream/mq_factory.go`):

支持三种后端：
| 后端 | 适用场景 | 实现路径 |
|------|---------|---------|
| **RocksMQ** | 单机/嵌入模式（默认） | `pkg/mq/mqimpl/rocksmq/` |
| **Pulsar** | 分布式生产环境 | 通过 MQ 工厂适配 |
| **Kafka** | 分布式生产环境 | 通过 MQ 工厂适配 |

**消息类型** (`pkg/mq/msgstream/msg.go`):
- `InsertMessage` — 行数据
- `DeleteMessage` — 删除标记
- `TimestampMessage` — 时间同步
- `FlushMessage` — Flush 检查点

**WAL 层** (`internal/streamingnode/server/wal/`):
- `WALManager` 创建和管理 WAL 实例
- 支持拦截器链：`shard_interceptor`（分片分配）、`replicate_interceptor`（副本）、`lock_interceptor`（并发控制）、`redo_interceptor`（恢复日志）

### 2.4 Binlog 存储格式

**文件结构** (`internal/storage/binlog_writer.go`):

```
┌──────────────────────────────────────┐
│ Magic Number (4 bytes): 0xfffabc     │
├──────────────────────────────────────┤
│ Descriptor Event                     │
│  ├─ Timestamp (int64)                │
│  ├─ TypeCode (int8)                  │
│  ├─ EventLength (int32)              │
│  └─ NextPosition (int32)            │
├──────────────────────────────────────┤
│ Event 1 (Insert/Delete/...)          │
├──────────────────────────────────────┤
│ Event 2                             │
├──────────────────────────────────────┤
│ ...                                  │
└──────────────────────────────────────┘
```

**Event 类型**:
| TypeCode | 名称 | 用途 |
|----------|------|------|
| 0 | DescriptorEvent | 文件描述符 |
| 1 | InsertEvent | 插入数据 |
| 2 | DeleteEvent | 删除数据 |
| 7 | IndexFileEvent | 索引文件 |

数据序列化支持 Arrow / Parquet 格式，通过 `PayloadWriter/Reader` 进行列式编码。

### 2.5 Segment 文件布局

**对象存储路径规则** (`pkg/util/metautil/binlog.go`):

```
{rootPath}/
├── insert_log/{collectionID}/{partitionID}/{segmentID}/{fieldID}/{logID}
├── delta_log/{collectionID}/{partitionID}/{segmentID}/{logID}
├── stats_log/{collectionID}/{partitionID}/{segmentID}/{fieldID}/{logID}
└── index/{buildID}/{indexVersion}/{partitionID}/{segmentID}/{fileKey}
```

**SegmentInfo 结构** (Proto 定义):

```protobuf
message SegmentInfo {
    int64 ID = 1;
    int64 collectionID = 2;
    int64 partitionID = 3;
    string insert_channel = 4;
    int64 num_of_rows = 5;
    SegmentState state = 6;              // Growing / Sealed / Flushing / Flushed / Dropped
    repeated FieldBinlog binlogs = 11;   // 插入 Binlog（按字段分）
    repeated FieldBinlog statslogs = 12; // 统计信息（min/max 等）
    repeated FieldBinlog deltalogs = 13; // 删除日志
    SegmentLevel level = 20;             // L0 / L1 / L2
    int64 storage_version = 21;
    bool is_sorted = 25;                 // 主键是否已排序
}
```

每个 Segment 由以下文件组成：
- **Insert Binlog**: 每个字段一组 binlog 文件，列式存储
- **Stats Binlog**: 统计信息（主键范围、行数等）
- **Delta Binlog**: 删除记录（主键 + 时间戳）
- **Index Files**: 构建后的向量索引文件

### 2.6 索引存储

**索引接口** (`internal/util/indexcgowrapper/index.go`):

```go
type CodecIndex interface {
    Build(*Dataset) error
    Serialize() ([]*Blob, error)
    Load([]*Blob) error
    UpLoad() (*cgopb.IndexStats, error)
    CleanLocalData() error
}
```

**索引类型与加载模式**:

| 索引类型 | 加载方式 | 内存开销 |
|---------|---------|---------|
| HNSW | 全内存 | indexSize |
| IVF_FLAT / IVF_SQ8 | 全内存 | indexSize |
| DiskANN | 磁盘 + 部分内存 | indexSize / UsedDiskMemoryRatio |
| INVERTED | Mmap | 0（mmap binlog） |

索引文件存储路径：`{rootPath}/index/{buildID}/{indexVersion}/{partitionID}/{segmentID}/{fileKey}`

---

## 三、缓存与内存管理

### 3.1 QueryNode 段管理

QueryNode 管理两类段：

| 类型 | 数据来源 | 存储位置 | 说明 |
|------|---------|---------|------|
| **Sealed Segment** | 对象存储加载 | 内存 / Mmap / DiskCache | 历史数据，已完成 Flush |
| **Growing Segment** | 流式数据实时写入 | 内存 | 最新数据，尚未 Flush |
| **L0 Segment** | 对象存储加载 | 始终在内存 | 仅存储删除记录 |

**段生命周期管理**:
- 段通过 `ptrLock.PinIf()` 机制确保搜索期间不被释放
- 段释放由 QueryCoord 的负载均衡策略触发

### 3.2 Mmap 内存映射

```go
// ChunkManager 接口支持 Mmap
Mmap(ctx context.Context, filePath string) (*mmap.ReaderAt, error)
```

- 按字段粒度决定是否使用 Mmap
- 支持同步/异步预热策略（warmup）
- 倒排索引（INVERTED）默认使用 Mmap，无额外内存开销

### 3.3 DiskCache 磁盘缓存

QueryNode 使用本地磁盘作为远程对象存储的缓存层。

**监控指标** (`internal/querynodev2/segments/metricsutil/observer.go`):

| 指标 | 含义 |
|------|------|
| `DiskCacheLoadTotal` | 磁盘缓存加载总次数 |
| `DiskCacheLoadBytes` | 加载到缓存的字节数 |
| `DiskCacheLoadDuration` | 加载耗时（按资源组） |
| `DiskCacheEvictTotal` | 缓存淘汰总次数 |
| `DiskCacheEvictBytes` | 淘汰的字节数 |

**缓存流程**:
```
QueryNode 需要段数据
  → 检查本地 DiskCache 是否存在
    → 命中: 直接从本地磁盘加载（Mmap 或读取）
    → 未命中: 从对象存储（MinIO/S3）下载到本地磁盘 → 加载
  → 缓存空间不足时 LRU 淘汰
```

### 3.4 内存估算与资源管理

**ResourceUsage 结构**:

```go
type ResourceUsage struct {
    MemorySize         uint64
    DiskSize           uint64
    MmapFieldCount     int
    FieldGpuMemorySize []uint64
}
```

内存估算逻辑根据索引类型不同：
- **DiskANN**: `neededMemSize = indexSize / UsedDiskMemoryRatio`
- **INVERTED**: `neededMemSize = 0`, `neededDiskSize = indexSize + binlogDataSize`
- **内存索引 (HNSW/IVF)**: `neededMemSize = indexSize`

---

## 四、插入向量端到端流程

### 4.1 流程概览

```
Client
  │
  │ gRPC Insert Request
  ▼
┌─────────┐    ┌──────────────┐    ┌───────────────┐    ┌──────────────┐
│  Proxy  │───►│ StreamingNode│───►│ Write Buffer  │───►│ Object Store │
│         │    │   (WAL)      │    │  (内存缓冲)    │    │ (MinIO/S3)   │
└─────────┘    └──────────────┘    └───────────────┘    └──────────────┘
  │                                       │                     │
  │ 1.验证+分片路由                        │ 5.Seal+Flush         │ 7.索引构建
  │ 2.主键分配                             │                     │
  │ 3.写入WAL                              ▼                     ▼
  │                                 ┌─────────────┐    ┌──────────────┐
  │                                 │  DataCoord  │    │  DataNode    │
  │                                 │ (段生命周期)  │    │ (构建索引)    │
  │                                 └─────────────┘    └──────────────┘
  ▼
 返回 MutationResult (IDs + Timestamp)
```

### 4.2 Step 1: Proxy 接收请求

**入口**: `internal/proxy/impl.go:2750`

```go
func (node *Proxy) Insert(ctx context.Context, request *milvuspb.InsertRequest) (*milvuspb.MutationResult, error)
```

**处理步骤**:
1. 健康检查: `merr.CheckHealthy(node.GetStateCode())`
2. 检查是否为外部集合（外部集合禁止写入）
3. 创建 `insertTask` 并入队执行

**insertTask 结构** (`internal/proxy/task_insert.go:24`):

```go
type insertTask struct {
    insertMsg   *BaseInsertTask    // = msgstream.InsertMsg
    idAllocator *allocator.IDAllocator
    chMgr       channelsMgr        // 通道管理器
    schema      *schemaInfo
    result      *milvuspb.MutationResult
    vChannels   []string
    pChannels   []string
}
```

**PreExecute 验证** (`task_insert.go:102`):
- 校验 Collection 名称
- 检查请求大小（不超过 `maxInsertSize`）
- 获取 Collection ID 和 Schema
- 处理 Auto-ID（自动主键分配）：`checkPrimaryFieldData()`
- 校验字段数据类型、动态字段

### 4.3 Step 2: 主键分配与分片路由

**Hash 分片** (`internal/proxy/util.go:2404`):

```go
func assignChannelsByPK(result IDs, channelNames []string, insertMsg *InsertMsg)
```

流程：
1. 对每条数据的主键计算 Hash 值: `typeutil.HashPK2Channels(pks, channelNames)`
2. Hash 值决定数据路由到哪个 Virtual Channel (VChannel)
3. 按 VChannel 将数据拆分成多个 `InsertMsg`

**消息打包** (`internal/proxy/msg_pack.go:30`):

```go
func genInsertMsgsByPartition(segmentID, partitionID, ...) ([]*msgstream.InsertMsg, error)
```

- 数据按列式 (Column-Based) 组织: `msgpb.InsertRequest{Version: Version_ColumnBased}`
- 超过消息大小阈值 (`PulsarCfg.MaxMessageSize`) 时自动分包

### 4.4 Step 3: 写入 WAL / 消息队列

**入口**: `internal/proxy/task_insert_streaming.go:28`

```go
func (it *insertTask) Execute(ctx context.Context) error
```

**消息构建**:
```go
msg := message.NewInsertMessageBuilderV1().
    WithVChannel(channel).          // 目标 VChannel
    WithHeader(header).             // Partition ID 等元信息
    WithBody(insertRequest).        // 实际数据
    BuildMutable()
```

**写入 WAL**:
```go
resp := streaming.WAL().AppendMessages(ctx, msgs...)
```

消息被追加到 StreamingNode 的 WAL 中，返回 `MaxTimeTick` 用于会话一致性保证。

### 4.5 Step 4: StreamingNode 消费与缓冲

**WAL Flusher** (`internal/streamingnode/server/flusher/flusherimpl/wal_flusher.go:59`):

```go
type WALFlusherImpl struct { ... }

func (f *WALFlusherImpl) Execute(recoverSnapshot) // 从 WAL Scanner 消费消息
```

**消息处理器** (`internal/streamingnode/server/flusher/flusherimpl/msg_handler_impl.go:40`):

```go
type msgHandlerImpl struct { ... }
```

- 将 Insert 消息路由到 Write Buffer Manager
- 处理 Flush、Seal Segment、Schema 变更等控制消息

**Write Buffer Manager** (`internal/flushcommon/writebuffer/manager.go:26`):

```go
type BufferManager interface {
    CreateNewGrowingSegment(ctx, channel, partitionID, segmentID)
    SealSegments(ctx, channel, segmentIDs)
    FlushChannel(ctx, channel, flushTs)
}
```

数据在内存中按 Segment 粒度缓冲：
- `write_buffer.go` — 每个 Collection 的写缓冲
- `insert_buffer.go` — 插入数据缓冲
- `segment_buffer.go` — 段级别缓冲

### 4.6 Step 5: Segment 封存 (Seal)

**封存策略** (`internal/datacoord/segment_allocation_policy.go`):

| 策略 | 触发条件 | 函数 |
|------|---------|------|
| 容量封存 | Segment 行数达到上限 | `sealL1SegmentByCapacity()` |
| 时间封存 | Segment 存活时间超过阈值 | `sealL1SegmentByLifetime()` |
| Binlog 文件数 | Binlog 文件数量过多 | `sealL1SegmentByBinlogFileNumber()` |
| 空闲封存 | Segment 长时间无新写入 | `sealL1SegmentByIdleTime()` |

**DataCoord 段管理** (`internal/datacoord/segment_manager.go`):

```go
func (s *SegmentManager) tryToSealSegment(ctx, ts, channel) // 评估封存策略
func (s *SegmentManager) SealAllSegments(ctx, channel, segIDs) // 手动全部封存
```

### 4.7 Step 6: Flush 到对象存储

**Sync Task** (`internal/flushcommon/syncmgr/task.go:49`):

```go
type SyncTask struct { ... }

func (t *SyncTask) Run(ctx context.Context) error
```

Flush 过程生成以下文件并写入对象存储：

| 文件类型 | 说明 |
|---------|------|
| `insertBinlogs` | 每个字段的插入数据（`map[fieldID]*FieldBinlog`） |
| `statsBinlogs` | 统计信息（min/max/行数等） |
| `deltaBinlog` | 删除日志 |
| `bm25Binlogs` | BM25 搜索日志（如适用） |
| `manifestPath` | 段元数据清单 |

支持 StorageV2 和 StorageV3 两种写入格式：`NewBulkPackWriterV2()` / `NewBulkPackWriterV3()`

**Segment 状态转换**:
```
Growing → Sealed (SealSegments)
       → Flushing (postFlush)
       → Flushed (完成写入对象存储)
```

### 4.8 Step 7: 索引构建

Flush 完成后触发索引构建：

1. DataCoord 通过 `notifyIndexChan` 通知索引检查器
2. `indexInspector` 监控已 Flush 的段
3. 通过 `globalScheduler` 调度索引构建任务到 DataNode
4. DataNode 的 `index/TaskScheduler` 执行索引构建

**索引构建位置**: `internal/datanode/index/`

### 4.9 Step 8: 段可查询

索引构建完成后：
1. DataCoord 更新段的元数据（添加索引信息）
2. QueryCoord 检测到新的可用段
3. QueryCoord 将段分配到 QueryNode 进行加载
4. QueryNode 从对象存储加载段数据和索引 → 段变为可查询状态

---

## 五、查询向量端到端流程

### 5.1 流程概览

```
Client
  │
  │ gRPC Search Request (vectors + filter + topK)
  ▼
┌──────────┐
│  Proxy   │
│          │ 1. 解析参数、生成查询计划
│          │ 2. 获取分片路由信息
│          │ 3. 按分片并行分发到 QueryNode
└────┬─────┘
     │ gRPC (按 VChannel 路由)
     ▼
┌──────────────┐
│  QueryNode   │
│              │ 4. 查找本地段（Sealed + Growing）
│              │ 5. 缓存命中? → 直接搜索
│              │    缓存未命中? → 从对象存储加载
│              │ 6. CGO 调用 C++ Knowhere 引擎
│              │ 7. 段级结果归并
└────┬─────────┘
     │ 返回 SearchResults
     ▼
┌──────────┐
│  Proxy   │ 8. 跨分片结果归并 (TopK Reduce)
│          │ 9. 返回最终结果
└──────────┘
```

### 5.2 Step 1: Proxy 接收搜索请求

**入口**: `internal/proxy/task_search.go:63`

```go
type searchTask struct {
    *internalpb.SearchRequest
    ctx            context.Context
    result         *milvuspb.SearchResults
    request        *milvuspb.SearchRequest
    lb             LBPolicy           // 负载均衡策略
    queryChannelsTs map[string]uint64
    // ...
}
```

**PreExecute** (`task_search.go:146`):
1. 解析 Collection 元数据和 Schema
2. 验证输出字段 (`translateOutputFields()`)
3. 解析搜索参数: metric_type, topK, nq, search_params
4. 生成查询计划: `tryGeneratePlan()`

### 5.3 Step 2: 表达式解析与查询计划生成

**解析器**: `internal/parser/planparserv2/plan_parser_v2.go:465`

```go
func CreateSearchPlan(schema, exprStr, vectorField, queryInfo, ...) (*planpb.PlanNode, error)
```

**PlanNode 包含**:
- 表达式 AST（过滤条件树）
- 向量字段 ID 和度量类型
- 输出字段列表
- 命名空间信息

**示例**: `search(vectors, filter="age > 20 AND city == 'Beijing'", topK=10)`
```
PlanNode {
    Node: VectorANNS {
        FieldID: 101,
        MetricType: L2,
        TopK: 10,
        Predicates: BinaryExpr {
            Op: AND,
            Left: CompareExpr { Field: "age", Op: GT, Value: 20 },
            Right: CompareExpr { Field: "city", Op: EQ, Value: "Beijing" }
        }
    }
}
```

### 5.4 Step 3: 分片路由与负载均衡

**负载均衡器** (`internal/proxy/shardclient/lb_policy.go:301`):

```go
func (lb *LBPolicyImpl) Execute(ctx, workload ChannelWorkload) error
```

**路由流程**:
1. `GetShardLeaderList()` — 从 Meta Cache 获取 VChannel → QueryNode 的映射
2. 按 VChannel 将 nq (查询向量数) 分配到不同 QueryNode
3. 使用 `errgroup` 并行发送搜索请求
4. 每个 VChannel 通过 `executeWithRetry()` 发送到对应的 Shard Leader

```
Collection 有 N 个 VChannel
  → VChannel-0 → QueryNode-A (Shard Leader)
  → VChannel-1 → QueryNode-B (Shard Leader)
  → VChannel-2 → QueryNode-A (Shard Leader)  // 一个 QN 可负责多个分片
```

### 5.5 Step 4: QueryNode 执行搜索

**RPC 处理器**: `internal/querynodev2/handlers.go`

**搜索分为两阶段** (`internal/querynodev2/segments/search.go`):

```go
func SearchHistorical(ctx, mgr, searchReq, ...) ([]*SearchResult, error)  // line 118
func SearchStreaming(ctx, mgr, searchReq, ...)  ([]*SearchResult, error)  // line 133
```

| 阶段 | 数据源 | 说明 |
|------|--------|------|
| **SearchHistorical** | Sealed Segments | 已 Flush 到对象存储的历史数据 |
| **SearchStreaming** | Growing Segments | 实时流式写入的最新数据 |

**段级并行搜索** (`search.go:35`):
```go
func searchSegments(ctx, mgr, segments, segType, searchReq) ([]*SearchResult, error)
// 使用 errgroup 对每个 segment 并行调用 s.Search(ctx, searchReq)
```

### 5.6 Step 5: 段数据加载（缓存未命中）

当 QueryNode 需要搜索一个段但数据不在本地时：

```
QueryCoord 分配段加载任务
  │
  ▼
QueryNode Segment Loader
  │
  ├─ 检查 DiskCache（本地磁盘）
  │   ├─ 命中 → 从本地磁盘加载到内存（或 Mmap）
  │   └─ 未命中 → 从对象存储 (MinIO/S3) 下载
  │              ├─ 下载 Insert Binlog（各字段）
  │              ├─ 下载 Index 文件
  │              ├─ 下载 Delta Binlog（删除记录）
  │              └─ 写入 DiskCache
  │
  ▼
加载到内存（根据索引类型和配置）
  ├─ 全内存加载（HNSW / IVF）
  ├─ Mmap 加载（INVERTED / 配置为 mmap 的字段）
  └─ 磁盘索引（DiskANN — 仅加载部分元数据到内存）
```

**段加载请求** (`internal/querynodev2/segments/requests.go:42`):
- `LoadFieldDataRequest` 指定每个字段的加载模式（内存 / Mmap）
- `LoadFieldDataInfo` 包含 binlog 路径列表

### 5.7 Step 6: C++ Core 向量检索

**CGO 桥接层** (`internal/util/segcore/segment.go`):

```go
// CGO 头文件引用
// #include "segcore/collection_c.h"
// #include "segcore/segment_c.h"
// #include "segcore/plan_c.h"

type cSegmentImpl struct { ... }

func (s *cSegmentImpl) Search(ctx, searchReq *SearchRequest) (*SearchResult, error)
```

**执行流程**:
1. Go 层序列化 SearchRequest（包含查询向量和查询计划）
2. 通过 CGO 调用 C++ Knowhere 引擎
3. C++ 层执行：
   - 解析查询计划中的过滤条件
   - 在向量索引上执行 ANN 搜索
   - 对无索引字段执行线性扫描
4. 返回 `SearchResult`（TopK 个 ID + Score）

**段搜索时的并发控制**:
```go
// 搜索前 Pin 住段，防止被释放
s.ptrLock.PinIf(...)
defer s.ptrLock.Unpin()
s.csegment.Search(ctx, searchReq)
```

### 5.8 Step 7: 结果归并 (Reduce)

**两级归并**:

**第一级：QueryNode 内部归并** (`internal/querynodev2/segments/result.go:50`):

```go
func ReduceSearchResults(ctx, results, info) (*internalpb.SearchResults, error)
```

1. `DecodeSearchResults()` — 解码各段的二进制结果
2. `InitSearchReducer()` — 根据度量类型创建归并器
3. `ReduceSearchResultData()` — 按 Score 归并 TopK
4. `EncodeSearchResultData()` — 编码归并后的结果
5. 聚合代价指标：扫描字节数、关联数据大小

**第二级：Proxy 跨分片归并**:
- Proxy 收集所有 QueryNode 返回的结果
- 按 Score 全局排序，取 TopK
- PostExecute 中执行后处理管线

### 5.9 Query（非向量查询）流程差异

**入口**: `internal/proxy/task_query.go:54`

```go
type queryTask struct {
    *internalpb.RetrieveRequest
    plan *planpb.PlanNode
}
```

**与 Search 的区别**:
- 不执行向量相似度计算
- 两种查询方式：
  - **主键查询**: `CreateRequeryPlan()` — 按 PK 直接查找
  - **过滤查询**: `QueryPlanNode` — 使用谓词过滤
- 返回格式: `segcorepb.RetrieveResults`（字段值而非 Score）
- 支持 GROUP BY + ORDER BY 聚合

### 5.10 Hybrid Search（混合搜索）流程

**入口**: `internal/proxy/task_search.go:414`

```go
func (t *searchTask) initAdvancedSearchRequest()
```

**两阶段执行**:

1. **多向量搜索阶段**: 每个子请求独立生成查询计划和 QueryInfo，分别在 QueryNode 执行
2. **Proxy 端重排序阶段**: 
   - `ReduceAdvancedSearchResults()` — 收集所有子结果（不合并）
   - `rerank.Reduce()` — 使用 FunctionScore 重新评分和排序
   - 支持按子向量的权重贡献进行聚合

**Rerank 元数据**: `rerankMeta` 接口 (`internal/proxy/rerank_meta.go`) 持有 FunctionScore 配置。

---

## 六、Delete 处理与 L0 Segment

### Delete 数据流

```
Client Delete Request
  │
  ▼
Proxy: 构建 DeleteMsg (PK + Timestamp)
  │
  ▼
WAL / MsgStream: 写入删除消息到对应 VChannel
  │
  ▼
StreamingNode: 消费删除消息
  │
  ├─ 写入 Growing Segment 的删除缓冲
  └─ Flush 时生成 Delta Binlog
        │
        ▼
    对象存储: delta_log/{collectionID}/{partitionID}/{segmentID}/{logID}
```

### DeltaData 结构

```go
// internal/storage/delta_data.go
type DeltaData struct {
    pkType           schemapb.DataType    // Int64 或 VarChar
    deletePks        PrimaryKeys          // 被删除的主键列表
    deleteTimestamps []Timestamp          // 删除时间戳
    delRowCount      int64
}
```

### L0 Segment

**定义** (`internal/querynodev2/segments/segment_l0.go`):

```go
type L0Segment struct {
    baseSegment
    pks []storage.PrimaryKey    // 删除的主键
    tss []uint64                // 删除时间戳
}
```

**特点**:
- L0 Segment **仅存储删除记录**
- **始终驻留内存**，不可释放
- 查询时，QueryNode 用 L0 的删除记录过滤搜索结果
- 通过 L0 Compaction 将删除标记合并到 L1/L2 段

### Segment 层级

| Level | 含义 | 特点 |
|-------|------|------|
| **L0** | 删除层 | 仅存删除记录，始终在内存 |
| **L1** | 普通段 | Flush 后的标准段 |
| **L2** | 压缩段 | Compaction 产出的大段 |

---

## 七、Compaction 压缩机制

**接口定义** (`internal/datacoord/compaction_task.go`):

```go
type CompactionTask interface {
    Process() bool
    Clean() bool
    BuildCompactionRequest() (*datapb.CompactionPlan, error)
    GetSlotUsage() int64
    GetLabel() string
}
```

### Compaction 类型

| 类型 | 用途 | 触发条件 |
|------|------|---------|
| **L0 Delete Compaction** | 将 L0 删除记录应用到 L1/L2 段 | L0 段积累到阈值 |
| **Mix Compaction** | 合并小段为大段 | 小段数量过多 |
| **Clustering Compaction** | 按聚类键重新组织数据 | 数据分布不均 |
| **Major Compaction** | 全量压缩 | 手动触发或定时策略 |
| **Minor Compaction** | 部分压缩 | 增量优化 |

### Compaction 策略

| 策略 | 文件 | 说明 |
|------|------|------|
| `l0CompactionPolicy` | `compaction_policy_l0.go` | 检测活跃 Collection 的 L0 段 |
| `clusteringCompaction` | `compaction_policy_clustering.go` | 基于聚类的数据重组 |
| `mergingCompaction` | `compaction_policy_merging.go` | 标准小段合并 |

### L0 Compaction 流程

```
L0 Segment (删除记录) + L1/L2 Segments (数据)
  │
  ▼
DataCoord: 评估 L0 段大小，超阈值触发 Compaction
  │
  ▼
DataNode: 执行 L0 Compaction
  ├─ 读取 L0 的 DeltaData（删除 PK 列表）
  ├─ 应用到目标 L1/L2 段
  ├─ 生成新的段（不含已删除行）
  └─ 原 L0 段标记为 Dropped
```

---

## 八、关键代码路径速查表

### 插入流程

| 步骤 | 文件路径 | 关键函数/类型 |
|------|---------|-------------|
| Proxy 入口 | `internal/proxy/impl.go:2750` | `Proxy.Insert()` |
| Insert Task | `internal/proxy/task_insert.go:24` | `insertTask` |
| 消息打包 | `internal/proxy/msg_pack.go:30` | `genInsertMsgsByPartition()` |
| Streaming 执行 | `internal/proxy/task_insert_streaming.go:28` | `insertTask.Execute()` |
| Channel 管理 | `internal/proxy/channels_mgr.go:39` | `channelsMgr` |
| Hash 路由 | `internal/proxy/util.go:2404` | `assignChannelsByPK()` |
| WAL Flusher | `internal/streamingnode/server/flusher/flusherimpl/wal_flusher.go:59` | `WALFlusherImpl` |
| 消息处理器 | `internal/streamingnode/server/flusher/flusherimpl/msg_handler_impl.go:40` | `msgHandlerImpl` |
| Write Buffer | `internal/flushcommon/writebuffer/manager.go:26` | `BufferManager` |
| 封存策略 | `internal/datacoord/segment_allocation_policy.go` | `sealL1Segment*()` |
| Sync 任务 | `internal/flushcommon/syncmgr/task.go:49` | `SyncTask` |
| DataCoord 段管理 | `internal/datacoord/segment_manager.go` | `SegmentManager` |
| 索引构建 | `internal/datanode/index/` | `TaskScheduler` |

### 查询/搜索流程

| 步骤 | 文件路径 | 关键函数/类型 |
|------|---------|-------------|
| Proxy 入口 | `internal/proxy/task_search.go:63` | `searchTask` |
| 表达式解析 | `internal/parser/planparserv2/plan_parser_v2.go:465` | `CreateSearchPlan()` |
| 负载均衡 | `internal/proxy/shardclient/lb_policy.go:301` | `LBPolicyImpl.Execute()` |
| QN 搜索入口 | `internal/querynodev2/handlers.go` | RPC handler |
| 历史段搜索 | `internal/querynodev2/segments/search.go:118` | `SearchHistorical()` |
| 流式段搜索 | `internal/querynodev2/segments/search.go:133` | `SearchStreaming()` |
| 段级搜索 | `internal/querynodev2/segments/search.go:35` | `searchSegments()` |
| CGO 桥接 | `internal/util/segcore/segment.go` | `cSegmentImpl.Search()` |
| 结果归并 | `internal/querynodev2/segments/result.go:50` | `ReduceSearchResults()` |
| 混合搜索 | `internal/proxy/task_search.go:414` | `initAdvancedSearchRequest()` |
| Query 任务 | `internal/proxy/task_query.go:54` | `queryTask` |

### 存储与缓存

| 组件 | 文件路径 | 关键类型 |
|------|---------|---------|
| ChunkManager 接口 | `internal/storage/types.go` | `ChunkManager` |
| 本地存储 | `internal/storage/local_chunk_manager.go` | `LocalChunkManager` |
| 远程存储 | `internal/storage/remote_chunk_manager.go` | `RemoteChunkManager` |
| Binlog 写入 | `internal/storage/binlog_writer.go` | `BinlogWriter` |
| 路径生成 | `pkg/util/metautil/binlog.go` | 路径构建函数 |
| Delta 数据 | `internal/storage/delta_data.go` | `DeltaData` |
| L0 段 | `internal/querynodev2/segments/segment_l0.go` | `L0Segment` |
| 段接口 | `internal/querynodev2/segments/segment_interface.go` | `Segment`, `ResourceUsage` |
| 索引属性缓存 | `internal/querynodev2/segments/index_attr_cache.go` | 索引加载决策 |
| 索引 CGO | `internal/util/indexcgowrapper/index.go` | `CodecIndex` |
| Compaction | `internal/datacoord/compaction_task.go` | `CompactionTask` |
| 元数据 Catalog | `internal/metastore/catalog.go` | `RootCoordCatalog` |
| 会话管理 | `internal/util/sessionutil/session_util.go` | `SessionRaw` |

### 组件启动

| 组件 | 文件路径 |
|------|---------|
| 主入口 | `cmd/main.go` |
| 命令路由 | `cmd/milvus/milvus.go` |
| 角色启动 | `cmd/milvus/run.go` |
| MixCoord | `cmd/components/mix_coord.go` → `internal/coordinator/mix_coord.go` |
| Proxy | `cmd/components/proxy.go` → `internal/distributed/proxy/service.go` |
| QueryNode | `cmd/components/query_node.go` → `internal/querynodev2/` |
| DataNode | `cmd/components/data_node.go` → `internal/datanode/` |
| StreamingNode | `cmd/components/streaming_node.go` → `internal/streamingnode/server/` |
