# Milvus 向量数据库架构深度解析

## 目录

- [一、系统架构总览](#一系统架构总览)
  - [1.1 整体架构](#11-整体架构)
  - [1.2 核心组件](#12-核心组件)
  - [1.3 组件通信](#13-组件通信)
  - [1.4 服务发现与会话管理](#14-服务发现与会话管理)
  - [1.5 组件生命周期与状态机](#15-组件生命周期与状态机)
  - [1.6 Go / C++ Core 边界与代码地图](#16-go--c-core-边界与代码地图)
- [二、存储层架构](#二存储层架构)
  - [2.1 存储层全景](#21-存储层全景)
  - [2.2 元数据存储 (Meta Storage)](#22-元数据存储-meta-storage)
  - [2.3 对象存储 (Object Storage)](#23-对象存储-object-storage)
  - [2.4 消息队列 / WAL](#24-消息队列--wal)
  - [2.5 Segment 文件布局与 Binlog 格式](#25-segment-文件布局与-binlog-格式)
  - [2.6 向量在 Segment 内的存储与加载](#26-向量在-segment-内的存储与加载)
  - [2.7 索引存储](#27-索引存储)
- [三、缓存与内存管理](#三缓存与内存管理)
  - [3.1 QueryNode 段管理](#31-querynode-段管理)
  - [3.2 Mmap 内存映射](#32-mmap-内存映射)
  - [3.3 DiskCache 磁盘缓存](#33-diskcache-磁盘缓存)
  - [3.4 内存估算与资源管理](#34-内存估算与资源管理)
- [四、数据写入端到端流程](#四数据写入端到端流程)
  - [4.1 用户视角：写入前需要准备什么](#41-用户视角写入前需要准备什么)
  - [4.2 写入流程全景图](#42-写入流程全景图)
  - [4.3 Proxy 接收与验证](#43-proxy-接收与验证)
  - [4.4 主键分配与分片路由](#44-主键分配与分片路由)
  - [4.5 写入 WAL](#45-写入-wal)
  - [4.6 StreamingNode 消费与缓冲](#46-streamingnode-消费与缓冲)
  - [4.7 Segment 封存与 Flush](#47-segment-封存与-flush)
  - [4.8 索引构建](#48-索引构建)
  - [4.9 段可查询](#49-段可查询)
- [五、查询向量端到端流程](#五查询向量端到端流程)
  - [5.1 查询流程全景图](#51-查询流程全景图)
  - [5.2 Proxy 接收与查询计划生成](#52-proxy-接收与查询计划生成)
  - [5.3 分片路由与负载均衡](#53-分片路由与负载均衡)
  - [5.4 QueryNode 执行搜索](#54-querynode-执行搜索)
  - [5.5 C++ Core 向量检索](#55-c-core-向量检索)
  - [5.6 结果归并](#56-结果归并)
  - [5.7 Query / Hybrid Search 差异](#57-query--hybrid-search-差异)
  - [5.8 数据在各层的形态变化总表](#58-数据在各层的形态变化总表)
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

### 1.6 Go / C++ Core 边界与代码地图

Milvus 不是"全 Go"或者"全 C++"的架构，而是明显的**分层协作**：

```
┌─────────────────────────────────────────────────────────────┐
│                      Go 业务层                               │
│  RPC/HTTP API · 元数据 · 调度 · 分布式通信 · WAL 编排        │
│                                                             │
│  internal/proxy/                                            │
│  internal/datacoord/                                        │
│  internal/querynodev2/                                      │
│  internal/datanode/                                         │
│  internal/flushcommon/                                      │
├─────────────────────────────────────────────────────────────┤
│                   CGO 桥接层                                 │
│  internal/util/segcore/           (search/insert 桥接)      │
│  internal/util/indexcgowrapper/   (index build 桥接)        │
│                                                             │
│  识别特征: #cgo pkg-config / import "C" / C.AsyncSearch     │
├─────────────────────────────────────────────────────────────┤
│                    C++ Core                                  │
│  Segment 数据结构 · 向量搜索 · 标量过滤 · 索引构建/加载       │
│                                                             │
│  internal/core/src/segcore/       (segment 实现)            │
│  internal/core/src/query/         (搜索逻辑)                │
│  internal/core/src/exec/          (表达式执行)               │
│  internal/core/src/index/         (索引结构)                 │
│  internal/core/src/indexbuilder/  (索引构建)                 │
└─────────────────────────────────────────────────────────────┘
```

**一句话理解**：Go 负责把"哪张表、哪几个段、哪条请求、哪些参数"组织好；C++ 负责在"具体的 segment 上把这次执行真的跑出来"。

**关键桥接代码**:
- search/insert 桥接：[segment.go](/root/xty/milvus/internal/util/segcore/segment.go)
- QueryNode segment 封装：[segment.go](/root/xty/milvus/internal/querynodev2/segments/segment.go)
- index build 桥接：[task_index.go](/root/xty/milvus/internal/datanode/index/task_index.go)

---

## 二、存储层架构

### 2.1 存储层全景

Milvus 的存储分为三个层次，分别存放不同类型的数据：

```
┌─────────────────────────────────────────────────────────────────────┐
│                         存储层全景                                   │
│                                                                     │
│  ┌─── 元数据层 (etcd / TiKV) ───────────────────────────────────┐  │
│  │                                                               │  │
│  │  Collection Schema ─► field 定义、类型、维度                   │  │
│  │  Index 定义        ─► 哪个 field 建什么索引                   │  │
│  │  Segment Meta      ─► 段状态、行数、binlog 路径               │  │
│  │  SegmentIndex      ─► 段级索引构建任务状态与产物               │  │
│  │  RBAC / Alias      ─► 权限、别名                             │  │
│  │                                                               │  │
│  └───────────────────────────────────────────────────────────────┘  │
│                                                                     │
│  ┌─── 对象存储层 (MinIO / S3 / Azure / GCP) ────────────────────┐  │
│  │                                                               │  │
│  │  insert_log/  ─► 字段列式 binlog (每字段一组文件)              │  │
│  │  stats_log/   ─► 统计信息 (min/max PK, rowCount)             │  │
│  │  delta_log/   ─► 删除日志 (PK + timestamp)                   │  │
│  │  index/       ─► 索引文件 (HNSW graph, IVF lists, ...)       │  │
│  │                                                               │  │
│  └───────────────────────────────────────────────────────────────┘  │
│                                                                     │
│  ┌─── WAL 层 (StreamingNode / Pulsar / Kafka / RocksMQ) ────────┐  │
│  │                                                               │  │
│  │  Insert / Delete / Flush / DDL 消息                           │  │
│  │  按 VChannel 分区，保序                                       │  │
│  │                                                               │  │
│  └───────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────┘
```

**最容易混淆的一点**：元数据（schema / index 定义 / segment 状态）和实体文件（binlog / 索引文件）是分开存放的。元数据在 etcd，实体文件在对象存储。

### 2.2 元数据存储 (Meta Storage)

元数据通过 **etcd** 或 **TiKV** 存储，接口定义在 `internal/metastore/catalog.go`。

#### 四类核心元数据

| 元数据类型 | 作用 | 创建时机 | 创建者 | 消费者 |
|-----------|------|---------|-------|-------|
| **Collection Schema** | 定义 field type / dim / type params | `CreateCollection` | RootCoord | Proxy / DataCoord / QueryCoord / QueryNode |
| **Index 定义** | 定义某 field 要建什么索引 | `CreateIndex` | DataCoord | DataCoord |
| **Segment Meta** | 描述段当前状态和路径 | 开 segment 时创建，flush 时更新 | DataCoord | DataCoord / QueryCoord / QueryNode |
| **SegmentIndex** | 描述某 segment 的某 index 的 build 任务与产物 | DataCoord 为 flushed segment 创建 build task 时 | DataCoord | DataCoord / QueryNode |

#### 元数据依赖关系

```
CreateCollection (T0)
  │
  ▼
Collection Schema ─────────────────────────┐
  │                                         │
  │  Insert 开始 (T1)                       │
  ▼                                         │
Segment Meta ──────────────────────┐        │
  │                                 │        │
  │  Flush (T2)                     │        │
  ▼                                 │        │
Segment Meta (updated)              │        │
  │                                 │        │
  │  CreateIndex (T3)               │        │
  │          ▼                      │        │
  │  Index 定义 ────────────────────┤        │
  │                                 │        │
  │  indexInspector 发现需要 build   │        │
  │          ▼                      ▼        ▼
  │  SegmentIndex ◄── 组合: Segment Meta + Index 定义 + Schema
  │       │
  │       │  DataNode build + 上传
  │       ▼
  │  index files (对象存储)
  │       │
  │       │  QueryNode 加载
  │       ▼
  └──► 段可查询
```

**关键代码**:
- Collection Schema 落库：[meta_table.go](/root/xty/milvus/internal/rootcoord/meta_table.go)
- Index 定义创建：[index_service.go](/root/xty/milvus/internal/datacoord/index_service.go)
- Segment Meta 创建：[segment_manager.go](/root/xty/milvus/internal/datacoord/segment_manager.go)
- SegmentIndex 创建：[index_inspector.go](/root/xty/milvus/internal/datacoord/index_inspector.go)
- Catalog 接口：[catalog.go](/root/xty/milvus/internal/metastore/catalog.go)

#### 具体示例

固定一个例子，后续所有章节复用：

```
collectionID = 3001    fieldID = 103 (embedding, FloatVector, dim=4)
partitionID  = 5001    indexID = 9001 (HNSW, L2, M=16, efConstruction=200)
segmentID    = 7001    buildID = 88001
```

四类元数据在这个例子中的具体内容：

```
Collection Schema                    │  Index 定义
  CollectionID = 3001                │    CollectionID = 3001
  Fields:                            │    FieldID      = 103
    FieldID  = 103                   │    IndexID      = 9001
    Name     = "embedding"           │    IndexName    = "idx_embedding_hnsw"
    DataType = FloatVector           │    IndexParams  = {HNSW, L2, M=16, ef=200}
    TypeParams = {"dim":"4"}         │
─────────────────────────────────────┼──────────────────────────────────────
Segment Meta                         │  SegmentIndex
  ID = 7001                          │    SegmentID  = 7001
  CollectionID = 3001                │    IndexID    = 9001
  NumOfRows    = 2                   │    BuildID    = 88001
  State        = Flushed             │    IndexState = Finished
  StorageVersion = 3                 │    IndexFileKeys = [hnsw_meta, ...]
  Binlogs = [...]                    │    IndexType  = "HNSW"
```

#### BuildID：索引链路的"工作单号"

`BuildID` 是"给某个 segment 构建某个 index"的一次任务实例 ID，不要和 IndexID 混淆：

```
Collection 3001
  └─ field 103 = embedding
      └─ index 9001 = idx_embedding_hnsw    ◄── IndexID: collection 级定义
          ├─ segment 7001 -> build 88001     ◄── BuildID: segment 级任务
          ├─ segment 7002 -> build 88002
          └─ segment 7015 -> build 88015
```

- **IndexID**：这个 collection 的这个 field 应该有一个 HNSW 索引
- **BuildID**：具体给 segment 7001 构建这套 HNSW 索引的这一轮任务编号

对象存储里的索引文件路径按 BuildID 组织：`index/{buildID}/{indexVersion}/{partitionID}/{segmentID}/{fileKey}`

#### 容易误解的 5 句话

1. **Index 定义**不是索引文件，它只是"要建什么索引"的逻辑定义
2. **SegmentIndex** 不是 collection 级配置，它是某个 segment 的一次具体 build 结果
3. **field type / dim** 不在 segment meta 里，它们来自 collection schema
4. **index files** 不是 flush 顺手写出来的，它们是 DataNode build 完后单独上传的
5. QueryNode 真正加载索引时，消费的是 **IndexFilePaths**，不是 Index 定义本身

### 2.3 对象存储 (Object Storage)

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

### 2.4 消息队列 / WAL

**MsgStream 工厂** (`pkg/mq/msgstream/mq_factory.go`):

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

### 2.5 Segment 文件布局与 Binlog 格式

#### 对象存储路径规则

路径定义在 `pkg/util/metautil/binlog.go`：

```
{rootPath}/
├── insert_log/{collectionID}/{partitionID}/{segmentID}/{fieldID}/{logID}
├── delta_log/{collectionID}/{partitionID}/{segmentID}/{logID}
├── stats_log/{collectionID}/{partitionID}/{segmentID}/{fieldID}/{logID}
└── index/{buildID}/{indexVersion}/{partitionID}/{segmentID}/{fileKey}
```

#### 一个 Segment 在对象存储里长什么样

以 `segment 7001`（2 行数据，4 个字段）为例：

```
insert_log/3001/5001/7001/
  ├── 100/910001   ─► id 列:        [101, 103]
  ├── 101/910002   ─► title 列:     ["red mug", "green tea"]
  ├── 102/910003   ─► price 列:     [19.8, 9.9]
  └── 103/910004   ─► embedding 列: [0.10,0.20,0.30,0.40, 0.12,0.18,0.33,0.39]

stats_log/3001/5001/7001/
  ├── 100/920001   ─► 主键统计: minPK=101, maxPK=103, rowCount=2
  └── 103/920002   ─► 向量字段统计

index/88001/1/5001/7001/
  ├── hnsw_meta     ─► HNSW 元信息
  ├── hnsw_graph    ─► HNSW 图结构
  └── hnsw_data     ─► HNSW 向量数据
```

**关键：每个字段各有自己的 binlog 文件，不是一个 JSON 文件一行一条记录。** QueryNode 加载时再把这些字段重新拼成可查询的段。

#### Binlog 文件内部结构

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

| TypeCode | 名称 | 用途 |
|----------|------|------|
| 0 | DescriptorEvent | 文件描述符 |
| 1 | InsertEvent | 插入数据 |
| 2 | DeleteEvent | 删除数据 |
| 7 | IndexFileEvent | 索引文件 |

数据序列化支持 Arrow / Parquet 格式，通过 `PayloadWriter/Reader` 进行列式编码。

#### SegmentInfo Proto 定义

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

### 2.6 向量在 Segment 内的存储与加载

这一节回答一个核心问题：**一组向量从写入到被搜索到，在内存中是什么形态？**

#### Growing Segment：实时写入的内存结构

```
┌─────────────────────────────────────────────────────────┐
│              Growing Segment (内存)                       │
│                                                          │
│  InsertRecord                                            │
│  ├─ field 100 (id):        AckData → [101, 103]         │
│  ├─ field 101 (title):     AckData → ["red mug", ...]   │
│  ├─ field 102 (price):     AckData → [19.8, 9.9]        │
│  ├─ field 103 (embedding): AckData → 连续 float 数组     │
│  │   ┌──────┬──────┬──────┬──────┐                       │
│  │   │ 0.10 │ 0.20 │ 0.30 │ 0.40 │ ← row 0             │
│  │   │ 0.12 │ 0.18 │ 0.33 │ 0.39 │ ← row 1             │
│  │   └──────┴──────┴──────┴──────┘                       │
│  ├─ RowIDs:    [90001, 90003]                            │
│  └─ Timestamps: [ts, ts]                                 │
│                                                          │
│  ◆ 无索引，搜索时走 brute force                           │
│  ◆ C++ SegmentGrowingImpl 管理                           │
│  ◆ 通过 CGO Insert() 写入                                │
└─────────────────────────────────────────────────────────┘
```

- 向量以**连续 float 数组**存储在内存中，按 dim 解释
- Growing Segment 没有向量索引，Search 走暴力扫描（`SearchOnGrowing`）
- 实现：`internal/core/src/segcore/SegmentGrowingImpl.cpp`

#### Sealed Segment：从对象存储加载后的内存结构

```
┌──────────────────────────────────────────────────────────────┐
│              Sealed Segment (加载后)                           │
│                                                               │
│  ┌─ 原始字段数据 ──────────────────────────────────────────┐  │
│  │                                                         │  │
│  │  field 100 (id):     [101, 103]        ← 内存/Mmap     │  │
│  │  field 101 (title):  ["red mug", ...]  ← 内存/Mmap     │  │
│  │  field 102 (price):  [19.8, 9.9]       ← 内存/Mmap     │  │
│  │  field 103 (embedding): 原始向量       ← 可能不单独加载  │  │
│  │                                                         │  │
│  └─────────────────────────────────────────────────────────┘  │
│                                                               │
│  ┌─ 向量索引 ──────────────────────────────────────────────┐  │
│  │                                                         │  │
│  │  ┌─────────────────────────────────────────────────┐    │  │
│  │  │ HNSW Index (field 103)                          │    │  │
│  │  │                                                 │    │  │
│  │  │   Layer 2:  ○                                   │    │  │
│  │  │            / \                                   │    │  │
│  │  │   Layer 1: ○───○                                │    │  │
│  │  │           /|\ /|\                               │    │  │
│  │  │   Layer 0: ○─○─○─○─○  (所有向量节点)             │    │  │
│  │  │                                                 │    │  │
│  │  │   内存占用 = indexSize                           │    │  │
│  │  └─────────────────────────────────────────────────┘    │  │
│  │                                                         │  │
│  │  加载模式由索引类型决定:                                 │  │
│  │  · HNSW / IVF    → 全内存                              │  │
│  │  · DiskANN       → 磁盘 + 部分内存                     │  │
│  │  · INVERTED      → Mmap                                │  │
│  │                                                         │  │
│  └─────────────────────────────────────────────────────────┘  │
│                                                               │
│  ┌─ 删除位图 ─────────────────────────────────────────────┐  │
│  │  来自 L0 Segment 的 delta 记录                         │  │
│  │  用于在搜索结果中排除已删除行                            │  │
│  └─────────────────────────────────────────────────────────┘  │
│                                                               │
│  ◆ C++ ChunkedSegmentSealedImpl 管理                         │
│  ◆ Search 优先走索引 (SearchOnIndex)                         │
│  ◆ 无索引时退化为 brute force (SearchBruteForce)             │
└──────────────────────────────────────────────────────────────┘
```

#### 向量数据的完整生命周期

```
用户 JSON                      Proxy 列式                   WAL 消息
{"embedding":                  FieldData[103] =            Body.FieldsData =
 [0.10,0.20,                    [0.10,0.20,0.30,0.40,       (同左, 按 channel
  0.30,0.40]}                    0.12,0.18,0.33,0.39]        分片后的子集)
      │                              │                           │
      ▼                              ▼                           ▼
  业务行式理解               内部始终列式处理            StreamingNode 消费
                                                              │
                    ┌─────────────────────────────────────────┘
                    ▼
            Growing Segment                    Flush
         (C++ 内存连续数组)  ──────────────►  字段 binlog 文件
         SearchOnGrowing                   insert_log/.../103/...
                                                    │
                                                    │  DataNode 读取
                                                    ▼
                                            索引构建 (Knowhere)
                                                    │
                                                    │  上传
                                                    ▼
                                           index/88001/.../
                                           hnsw_meta + graph + data
                                                    │
                                                    │  QueryNode 加载
                                                    ▼
                                           Sealed Segment (有索引)
                                           SearchOnIndex
```

### 2.7 索引存储

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

| 索引类型 | 加载方式 | 内存开销 | 搜索方式 |
|---------|---------|---------|---------|
| HNSW | 全内存 | indexSize | 图遍历 ANN |
| IVF_FLAT / IVF_SQ8 | 全内存 | indexSize | 倒排 + 聚类中心 |
| DiskANN | 磁盘 + 部分内存 | indexSize / UsedDiskMemoryRatio | 磁盘图遍历 |
| INVERTED | Mmap | 0（mmap binlog） | 倒排索引 |

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

```
QueryNode 需要段数据
  → 检查本地 DiskCache 是否存在
    → 命中: 直接从本地磁盘加载（Mmap 或读取）
    → 未命中: 从对象存储（MinIO/S3）下载到本地磁盘 → 加载
  → 缓存空间不足时 LRU 淘汰
```

**监控指标** (`internal/querynodev2/segments/metricsutil/observer.go`):

| 指标 | 含义 |
|------|------|
| `DiskCacheLoadTotal` | 磁盘缓存加载总次数 |
| `DiskCacheLoadBytes` | 加载到缓存的字节数 |
| `DiskCacheLoadDuration` | 加载耗时（按资源组） |
| `DiskCacheEvictTotal` | 缓存淘汰总次数 |
| `DiskCacheEvictBytes` | 淘汰的字节数 |

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

## 四、数据写入端到端流程

### 4.1 用户视角：写入前需要准备什么

| 步骤 | 是否必须 | 作用 | 不做会怎样 |
|------|---------|------|-----------|
| `CreateCollection` | **必须** | 定义 schema、主键、向量字段 | 没地方插数据 |
| `CreatePartition` | 可选 | 显式写到某个 partition | 用默认 partition 或 partition key 路由 |
| `Insert` | **必须** | 真正写入数据 | 没有数据 |
| `CreateIndex` | 建议 | 给向量字段建立索引定义 | 搜索走 brute force，性能差 |
| `LoadCollection` | 搜索时必须 | 把 segment 和索引加载到 QueryNode | 不能 search/query |
| `Flush` | 可选 | 让数据尽快从 growing 变成 flushed | 数据暂时在 growing segment |

**最小流程对比**：

```
只插入:                        插入 + 搜索:
  CreateCollection               CreateCollection
  → Insert                       → CreateIndex (建议)
                                  → LoadCollection
                                  → Insert
                                  → Search
```

**Insert 和 CreateIndex 是两条独立 API**，插入时不需要带索引参数。

#### 统一示例

后续所有章节复用同一个集合和同一批数据：

| 字段 | FieldID | 类型 | 说明 |
|------|---------|------|------|
| `id` | 100 | Int64 | 业务主键 |
| `title` | 101 | VarChar | 商品标题 |
| `price` | 102 | Float | 标量字段 |
| `embedding` | 103 | FloatVector(dim=4) | 向量字段 |

```json
[
  {"id": 101, "title": "red mug",     "price": 19.8, "embedding": [0.10, 0.20, 0.30, 0.40]},
  {"id": 102, "title": "blue bottle", "price": 29.9, "embedding": [0.40, 0.10, 0.20, 0.30]},
  {"id": 103, "title": "green tea",   "price":  9.9, "embedding": [0.12, 0.18, 0.33, 0.39]}
]
```

### 4.2 写入流程全景图

```
Client
  │
  │ gRPC InsertRequest (3 rows, 4 fields, 列式)
  ▼
┌──────────────────────────────────────────────────────────────────┐
│  Proxy                                                           │
│                                                                  │
│  1. 校验 schema / Auto-ID / 字段类型                              │
│  2. 分配内部 RowID: [90001, 90002, 90003]                        │
│  3. Hash(PK) 按行分片到 VChannel:                                │
│     id=101 → ch0, id=102 → ch1, id=103 → ch0                   │
│  4. 拆成 2 个 InsertMsg (ch0: 2行, ch1: 1行)                    │
│                                                                  │
└──────┬──────────────────────────────────────┬────────────────────┘
       │                                      │
       ▼                                      ▼
┌──────────────┐                      ┌──────────────┐
│ WAL (ch0)    │                      │ WAL (ch1)    │
│ 2 rows       │                      │ 1 row        │
└──────┬───────┘                      └──────┬───────┘
       │                                      │
       ▼                                      ▼
┌──────────────────┐               ┌──────────────────┐
│ StreamingNode     │               │ StreamingNode     │
│ Growing Seg 7001  │               │ Growing Seg 7002  │
│ id=[101,103]      │               │ id=[102]          │
│ (C++ segcore)     │               │ (C++ segcore)     │
└──────┬───────────┘               └──────┬───────────┘
       │  Seal + Flush                     │
       ▼                                   ▼
┌────────────────────────────────────────────────────────┐
│  Object Storage (MinIO / S3)                            │
│  insert_log/3001/5001/7001/{100,101,102,103}/...       │
│  stats_log/3001/5001/7001/...                          │
└──────┬─────────────────────────────────────────────────┘
       │
       ▼
┌────────────────────────────────────────────────────────┐
│  DataCoord: 发现 flushed segment → 创建 index task     │
│  DataNode: 读 binlog → build HNSW → 上传 index files  │
│  QueryNode: 加载 segment + index → 段可查询            │
└────────────────────────────────────────────────────────┘
```

**数据形态核心变化**：行式业务数据 → 列式字段数据 → 按行分片 → 按字段落盘

返回给客户端：`MutationResult { IDs=[101,102,103], InsertCnt=3, Timestamp=... }`

"插入结束"发生在 **Proxy 成功把消息写入 WAL 并拿到可见时间戳** 这一刻，不等于已经 Flush 到对象存储。

### 4.3 Proxy 接收与验证

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

进入 Proxy 后，数据仍然是列式的 `FieldData`。如果 `id` 是用户提供的业务主键，Proxy 仍然会额外分配内部 RowID（如 `[90001, 90002, 90003]`）。这两个概念不要混：

- `id` 是业务主键，用于去重、删除、查询返回
- `RowID` 是系统内部行标识，用于写链路和底层存储组织

### 4.4 主键分配与分片路由

**Hash 分片** (`internal/proxy/util.go:2404`):

```go
func assignChannelsByPK(result IDs, channelNames []string, insertMsg *InsertMsg)
```

流程：
1. 对每条数据的主键计算 Hash 值: `typeutil.HashPK2Channels(pks, channelNames)`
2. Hash 值决定数据路由到哪个 Virtual Channel (VChannel)
3. 按 VChannel 将数据拆分成多个 `InsertMsg`

**核心：一行数据只路由到一个 Channel。** 假设 2 个 VChannel，Hash 后：

```
id=101 → channel 0 (rowOffset 0)     InsertMsg for ch0:
id=102 → channel 1 (rowOffset 1)       id=[101,103], embedding=[vec0,vec2], NumRows=2
id=103 → channel 0 (rowOffset 2)     InsertMsg for ch1:
                                        id=[102], embedding=[vec1], NumRows=1
```

插入链路是"**按行切分、按列存储**"：哪一行去哪个 channel；每个 channel 内部是列式 `FieldsData`。

**消息打包** (`internal/proxy/msg_pack.go:30`): 超过消息大小阈值时自动分包。

### 4.5 写入 WAL

**入口**: `internal/proxy/task_insert_streaming.go:28`

```go
msg := message.NewInsertMessageBuilderV1().
    WithVChannel(channel).          // 目标 VChannel
    WithHeader(header).             // Partition ID 等元信息
    WithBody(insertRequest).        // 实际数据
    BuildMutable()

resp := streaming.WAL().AppendMessages(ctx, msgs...)
```

WAL 消息结构：
- **Header**：路由和元信息（CollectionId, PartitionId, Rows）
- **Body**：实际插入的列式字段数据

### 4.6 StreamingNode 消费与缓冲

**WAL Flusher** (`internal/streamingnode/server/flusher/flusherimpl/wal_flusher.go:59`):
- 从 WAL Scanner 消费消息
- 将 Insert 消息路由到 Write Buffer Manager
- 处理 Flush、Seal Segment、Schema 变更等控制消息

**Write Buffer Manager** (`internal/flushcommon/writebuffer/manager.go:26`):
数据在内存中按 Segment 粒度缓冲。

如果**刚 insert 完就立刻 search**，而此时还没 flush，数据从 **Growing Segments** 被搜索到。

#### Go 到 C++ 的真正插入点

系统级 insert 的大部分步骤在 Go 中完成。但当数据要进入执行层的 growing segment 时，底层调 C++ segcore：

```
Go: LocalSegment.Insert(...)
  → s.csegment.Insert(...)                    // internal/querynodev2/segments/segment.go
    → cSegmentImpl.Insert(...)                // internal/util/segcore/segment.go
      → preInsert() + serialize + C.Insert()  // 写进 C++ segment
```

C++ 侧：`internal/core/src/segcore/SegmentGrowingImpl.cpp`

### 4.7 Segment 封存与 Flush

#### 封存策略

`internal/datacoord/segment_allocation_policy.go`:

| 策略 | 触发条件 |
|------|---------|
| 容量封存 | Segment 行数达到上限 |
| 时间封存 | Segment 存活时间超过阈值 |
| Binlog 文件数 | Binlog 文件数量过多 |
| 空闲封存 | Segment 长时间无新写入 |

#### Flush 过程

**Sync Task** (`internal/flushcommon/syncmgr/task.go:49`):

Flush 生成以下文件写入对象存储：

| 文件类型 | 说明 |
|---------|------|
| `insertBinlogs` | 每个字段的插入数据 |
| `statsBinlogs` | 统计信息（min/max/行数等） |
| `deltaBinlog` | 删除日志 |
| `bm25Binlogs` | BM25 搜索日志（如适用） |
| `manifestPath` | 段元数据清单 |

**Segment 状态转换**:
```
Growing → Sealed (SealSegments)
       → Flushing (postFlush)
       → Flushed (完成写入对象存储)
```

### 4.8 索引构建

#### 索引构建全景图

```
Segment 7001 Flushed
  │
  │  DataCoord indexInspector 发现需要 build
  ▼
┌────────────────────────────────────────────────────────────┐
│  DataCoord                                                  │
│                                                            │
│  1. 分配 BuildID = 88001                                   │
│  2. 创建 SegmentIndex 元数据 (state=Init)                  │
│  3. 组装 CreateJobRequest:                                 │
│     ├─ 来自 Segment Meta:  numRows=2, storageVersion=3    │
│     ├─ 来自 Schema:        FloatVector, dim=4             │
│     ├─ 来自 Index 定义:    HNSW, L2, M=16, ef=200        │
│     └─ 来自对象存储:       binlog paths                    │
│  4. 发送给 DataNode                                        │
└──────────────────────────┬─────────────────────────────────┘
                           │
                           ▼
┌────────────────────────────────────────────────────────────┐
│  DataNode                                                   │
│                                                            │
│  5. 读取 insert_log/.../7001/103/... (原始向量列)          │
│  6. 调 Knowhere/segcore build HNSW                         │
│  7. 上传 index files:                                      │
│     index/88001/1/5001/7001/hnsw_{meta,graph,data}        │
│  8. 记录 fileKeys / size / state                           │
└──────────────────────────┬─────────────────────────────────┘
                           │
                           ▼
┌────────────────────────────────────────────────────────────┐
│  DataCoord                                                  │
│                                                            │
│  9. 查询 worker 结果                                       │
│  10. 回写 SegmentIndex: state=Finished, fileKeys=[...]    │
└──────────────────────────┬─────────────────────────────────┘
                           │
                           ▼
┌────────────────────────────────────────────────────────────┐
│  QueryNode                                                  │
│                                                            │
│  11. GetIndexInfos → 拿到 IndexFilePaths                   │
│  12. 从 object storage / DiskCache 加载 index files        │
│  13. append 到 Sealed Segment → 段可查询                   │
└────────────────────────────────────────────────────────────┘
```

**关键代码路径**:
- 分配 BuildID：[index_inspector.go#L205](/root/xty/milvus/internal/datacoord/index_inspector.go#L205)
- 组装 CreateJobRequest：[task_index.go#L227](/root/xty/milvus/internal/datacoord/task_index.go#L227)
- DataNode build：[task_index.go](/root/xty/milvus/internal/datanode/index/task_index.go)
- DataCoord 查结果：[task_index.go#L390](/root/xty/milvus/internal/datacoord/task_index.go#L390)
- QueryNode 加载：[segment_loader.go](/root/xty/milvus/internal/querynodev2/segments/segment_loader.go)

#### 为什么有些段"没建索引"

不一定是失败，也可能是系统故意跳过：
- **段太小**：建索引收益很小
- **特殊索引类型**：某些 no-train index 直接标成 fake finished

所以"collection 建了索引"不代表"每个 segment 都有一套重型索引文件"。

#### 索引构建后原始 binlog 不会消失

因为 QueryNode 搜索时并不只靠索引文件：
- 标量过滤要用原始字段数据
- 输出字段返回要用原始字段数据
- 某些搜索/校验流程仍会依赖原始数据

### 4.9 段可查询

索引构建完成后：
1. DataCoord 更新段的元数据（添加索引信息）
2. QueryCoord 检测到新的可用段
3. QueryCoord 将段分配到 QueryNode 进行加载
4. QueryNode 从对象存储加载段数据和索引 → 段变为可查询状态

**写入链路** 解决的是"数据先活下来并可见"；**索引构建链路** 解决的是"后续搜索能不能快"。

---

## 五、查询向量端到端流程

### 5.1 查询流程全景图

```
Client
  │
  │ Search(q0=[0.11,0.19,0.31,0.41], filter="price>10", topK=2)
  ▼
┌─────────────────────────────────────────────────────────────────┐
│  Proxy                                                           │
│                                                                  │
│  1. 解析参数、验证输出字段                                        │
│  2. 生成查询计划 (PlanNode):                                     │
│     VectorANNS { field=103, metric=L2, topK=2,                  │
│                  predicates: price > 10 }                        │
│  3. 查询向量编码为 PlaceholderGroup                               │
│  4. Fan-out: 同一个 q0 广播到所有 VChannel                       │
│     (Insert 是一行只进一个 shard; Search 是广播到所有 shard)      │
│                                                                  │
└──────┬──────────────────────────────────────────┬────────────────┘
       │                                          │
       ▼                                          ▼
┌──────────────────┐                     ┌──────────────────┐
│ QueryNode-A      │                     │ QueryNode-B      │
│ (ch0, seg 7001)  │                     │ (ch1, seg 7002)  │
│                  │                     │                  │
│ SearchHistorical │                     │ SearchHistorical │
│ + SearchStreaming│                     │ + SearchStreaming│
│                  │                     │                  │
│ C++ segcore:     │                     │ C++ segcore:     │
│  ANN on HNSW     │                     │  ANN on HNSW     │
│  filter price>10 │                     │  filter price>10 │
│                  │                     │                  │
│ 结果: id=101     │                     │ 结果: id=102     │
│       score=0.0004                     │       score=0.162│
└──────┬───────────┘                     └──────┬───────────┘
       │                                        │
       └─────────────┬──────────────────────────┘
                     ▼
┌─────────────────────────────────────────────────────────────────┐
│  Proxy: 跨分片归并 (TopK Reduce)                                 │
│                                                                  │
│  最终结果:                                                       │
│  [{"id":101, "score":0.0004, "title":"red mug", "price":19.8}, │
│   {"id":102, "score":0.162, "title":"blue bottle","price":29.9}]│
└─────────────────────────────────────────────────────────────────┘
```

### 5.2 Proxy 接收与查询计划生成

**入口**: `internal/proxy/task_search.go:63`

```go
type searchTask struct {
    *internalpb.SearchRequest
    ctx            context.Context
    result         *milvuspb.SearchResults
    request        *milvuspb.SearchRequest
    lb             LBPolicy           // 负载均衡策略
    queryChannelsTs map[string]uint64
}
```

**PreExecute** (`task_search.go:146`):
1. 解析 Collection 元数据和 Schema
2. 验证输出字段 (`translateOutputFields()`)
3. 解析搜索参数: metric_type, topK, nq, search_params
4. 生成查询计划: `tryGeneratePlan()`

**查询计划示例**:

```
PlanNode {
    Node: VectorANNS {
        FieldID: 103,
        MetricType: L2,
        TopK: 2,
        Predicates: CompareExpr { Field: "price", Op: GT, Value: 10 }
    }
}
```

**解析器**: `internal/parser/planparserv2/plan_parser_v2.go:465`

内部 `SearchRequest` 中，查询向量被编码为二进制 `PlaceholderGroup`（不是明文 float 数组）。

### 5.3 分片路由与负载均衡

**负载均衡器** (`internal/proxy/shardclient/lb_policy.go:301`):

```go
func (lb *LBPolicyImpl) Execute(ctx, workload ChannelWorkload) error
```

路由流程：
1. `GetShardLeaderList()` — 从 Meta Cache 获取 VChannel → QueryNode 映射
2. 按 VChannel 将 nq 分配到不同 QueryNode
3. 使用 `errgroup` 并行发送搜索请求
4. 每个 VChannel 通过 `executeWithRetry()` 发送到对应 Shard Leader

```
Collection 有 N 个 VChannel
  → VChannel-0 → QueryNode-A (Shard Leader)
  → VChannel-1 → QueryNode-B (Shard Leader)
  → VChannel-2 → QueryNode-A (一个 QN 可负责多个分片)
```

### 5.4 QueryNode 执行搜索

搜索分为两阶段 (`internal/querynodev2/segments/search.go`):

| 阶段 | 数据源 | 说明 |
|------|--------|------|
| **SearchHistorical** | Sealed Segments | 已 Flush 到对象存储的历史数据 |
| **SearchStreaming** | Growing Segments | 实时流式写入的最新数据 |

段级并行搜索：使用 `errgroup` 对每个 segment 并行调用 `s.Search(ctx, searchReq)`。

刚插入后立即查通常命中 Growing Segments（`SearchStreaming`）；Flush + Load 完成后再查通常命中 Sealed Segments（`SearchHistorical`）。对用户来说结果保持一致，差别在内部数据来源。

### 5.5 C++ Core 向量检索

#### 调用链

```
Proxy.Search
  → QueryNode.Search (handlers.go)
    → SearchHistorical / SearchStreaming (search.go)
      → searchSegments (errgroup 并发)
        → LocalSegment.Search (segment.go)
          → s.ptrLock.PinIf(...)                // Pin 住段防释放
          → s.csegment.Search(searchReq)        // CGO 桥接
            → C.AsyncSearch(segment_ptr, plan, placeholder_group, ...)
              → C++ segcore 执行
```

**对应代码**:
- 按段并发搜索：[search.go#L36](/root/xty/milvus/internal/querynodev2/segments/search.go#L36)
- Go segment 封装：[segment.go#L632](/root/xty/milvus/internal/querynodev2/segments/segment.go#L632)
- CGO 桥接：[segment.go#L134](/root/xty/milvus/internal/util/segcore/segment.go#L134)

#### C++ 执行过程（以示例说明）

```
segment 7001: id=[101,103], price=[19.8,9.9], embedding=[vec0,vec1]
查询: q0=[0.11,0.19,0.31,0.41], filter="price>10", topK=2, metric=L2

Step 1: 解析查询计划
  → 向量字段 = embedding, 度量 = L2, 谓词 = price > 10

Step 2: 选择搜索路径
  → 有 HNSW 索引 → SearchOnIndex
  → 无索引 → SearchBruteForce
  → Growing segment → SearchOnGrowing

Step 3: 向量搜索 + 标量过滤
  before filter: [101(0.0004), 103(0.0022)]
  after "price > 10": [101(0.0004)]

Step 4: 返回段级结果 → Go 层后续 reduce
```

#### C++ 源码地图

| 目录 | 职责 | 建议先看 |
|------|------|---------|
| `internal/core/src/segcore/` | Segment 数据结构、Growing/Sealed 实现 | `SegmentGrowingImpl.cpp`, `ChunkedSegmentSealedImpl.cpp` |
| `internal/core/src/query/` | Search/Retrieve/BruteForce 逻辑 | `SearchOnGrowing.cpp`, `SearchOnSealed.cpp`, `SearchOnIndex.cpp` |
| `internal/core/src/exec/` | 标量表达式执行、过滤/搜索算子 | `VectorSearchNode.cpp`, `FilterBitsNode.cpp`, `CompareExpr.cpp` |
| `internal/core/src/index/` | 索引结构实现 | `VectorMemIndex.cpp`, `VectorDiskIndex.cpp`, `InvertedIndexTantivy.cpp` |
| `internal/core/src/indexbuilder/` | 索引构建逻辑 | `VecIndexCreator.cpp`, `index_c.cpp` |

### 5.6 结果归并

**两级归并**:

**第一级：QueryNode 内部归并** (`internal/querynodev2/segments/result.go:50`):

```go
func ReduceSearchResults(ctx, results, info) (*internalpb.SearchResults, error)
```

1. `DecodeSearchResults()` — 解码各段的二进制结果
2. `InitSearchReducer()` — 根据度量类型创建归并器
3. `ReduceSearchResultData()` — 按 Score 归并 TopK
4. `EncodeSearchResultData()` — 编码归并后的结果

**第二级：Proxy 跨分片归并**:
- 收集所有 QueryNode 返回的结果
- 按 Score 全局排序，取 TopK
- PostExecute 中执行后处理管线

### 5.7 Query / Hybrid Search 差异

#### Query（非向量查询）

**入口**: `internal/proxy/task_query.go:54`

与 Search 的区别：
- 不执行向量相似度计算
- 两种查询方式：主键查询 (`CreateRequeryPlan`) / 过滤查询 (`QueryPlanNode`)
- 返回格式: `segcorepb.RetrieveResults`（字段值而非 Score）
- 支持 GROUP BY + ORDER BY 聚合

调用链类似：`Proxy.Query → QueryNode.Query → segments.Retrieve → s.csegment.Retrieve → C.AsyncRetrieve`

```
Search 返回: ids + score + 可选输出字段
Query  返回: ids + fields_data, 没有相似度分数
```

#### Hybrid Search（混合搜索）

**入口**: `internal/proxy/task_search.go:414`

两阶段执行：
1. **多向量搜索阶段**: 每个子请求独立在 QueryNode 执行
2. **Proxy 端重排序阶段**: `rerank.Reduce()` 使用 FunctionScore 重新评分和排序

### 5.8 数据在各层的形态变化总表

| 阶段 | 结构 | 本例中长什么样 |
|------|------|----------------|
| 业务侧 | 行式对象 | `{"id":101,"title":"red mug","price":19.8,"embedding":[...]}` |
| Insert 请求 | `milvuspb.InsertRequest` | `num_rows=3`, `fields_data` 是 4 列 |
| Proxy 内部 | `msgstream.InsertMsg` | 额外带上 `RowIDs=[90001,90002,90003]` |
| 分片后 | 多个 `InsertMsg` | channel 0: `[101,103]`，channel 1: `[102]` |
| WAL | `MutableMessage(Header+Body)` | header=collection/partition，body=列式字段数据 |
| Growing 段 | C++ `InsertRecord` | 列式字段数组，按 segment 分开缓存，连续 float 数组 |
| Flush 后 | 对象存储文件 | 每个字段一个 binlog 文件 |
| 索引构建后 | index files | `index/88001/1/5001/7001/hnsw_{meta,graph,data}` |
| Search 请求 | `internalpb.SearchRequest` | 查询向量编码为 `PlaceholderGroup` |
| QueryNode 结果 | `SearchResultData` | 段级局部 ids+scores，再节点级归并 |
| 最终结果 | `milvuspb.SearchResults` | `ids=[101,102] + scores=[0.0004,0.162] + fields` |

**一句话总结**：
- **写入**: 行式业务数据 → 列式字段数据 → 按行分片 → 按字段落盘 → 异步建索引
- **查询**: 查询向量/表达式 → 广播到各 shard → 段级 C++ 执行 → 分层归并 → 返回业务结果

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

```go
// internal/querynodev2/segments/segment_l0.go
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

```
┌─────────────────────────────────────────────────────────┐
│  L0 (删除层)                                             │
│  仅存删除记录 (PK + timestamp)                           │
│  始终在内存，查询时用于过滤                               │
├─────────────────────────────────────────────────────────┤
│  L1 (普通段)                                             │
│  Flush 后的标准段                                        │
│  包含 insert binlog + stats + delta                     │
├─────────────────────────────────────────────────────────┤
│  L2 (压缩段)                                             │
│  Compaction 产出的大段                                   │
│  删除已合并，数据更紧凑                                   │
└─────────────────────────────────────────────────────────┘

L0 Compaction: L0 删除记录 + L1/L2 数据段
  → 生成新段（不含已删除行）
  → 原 L0 段标记为 Dropped
```

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
