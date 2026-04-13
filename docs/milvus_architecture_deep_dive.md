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
  - [2.1 元数据存储 (Meta Storage)](#21-元数据存储-meta-storage)
    - [2.1.1 索引链路涉及的四类元数据](#211-索引链路涉及的四类元数据)
    - [2.1.2 具体示例](#212-具体示例)
    - [2.1.3 元数据创建时序](#213-元数据创建时序)
    - [2.1.4 BuildID 和索引文件的生命周期](#214-buildid-和索引文件的生命周期)
    - [2.1.5 五类信息并排对照表](#215-五类信息并排对照表)
    - [2.1.6 一条完整时序：从 CreateIndex 到 QueryNode LoadIndex](#216-一条完整时序从-createindex-到-querynode-loadindex)
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
  - [4.0 用户视角：想插入一批数据，到底要准备什么](#40-用户视角想插入一批数据到底要准备什么)
  - [4.1 流程概览](#41-流程概览)
  - [4.2 Step 1: Proxy 接收请求](#42-step-1-proxy-接收请求)
  - [4.3 Step 2: 主键分配与分片路由](#43-step-2-主键分配与分片路由)
  - [4.4 Step 3: 写入 WAL / 消息队列](#44-step-3-写入-wal--消息队列)
  - [4.5 Step 4: StreamingNode 消费与缓冲](#45-step-4-streamingnode-消费与缓冲)
    - [4.5.1 Go 到 C++ 的真正插入点](#451-go-到-c-的真正插入点)
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
    - [5.7.1 QueryNode 到 C++ 的调用链](#571-querynode-到-c-的调用链)
    - [5.7.2 C++ 源码地图：查询到底看哪几个目录](#572-c-源码地图查询到底看哪几个目录)
    - [5.7.3 示例：一条查询在 C++ 里如何执行](#573-示例一条查询在-c-里如何执行)
  - [5.8 Step 7: 结果归并 (Reduce)](#58-step-7-结果归并-reduce)
  - [5.9 Query（非向量查询）流程差异](#59-query非向量查询流程差异)
  - [5.10 Hybrid Search（混合搜索）流程](#510-hybrid-search混合搜索流程)
  - [5.11 同一批数据在各层的形态变化总表](#511-同一批数据在各层的形态变化总表)
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

Milvus 不是“全 Go”或者“全 C++”的架构，而是明显的**分层协作**：

- **Go** 负责：
  - RPC / HTTP API
  - 元数据
  - 调度
  - 分布式组件通信
  - WAL / flush / compaction / index task 编排
- **C++ Core** 负责：
  - Segment 内的数据结构
  - 向量搜索
  - 标量过滤
  - retrieve
  - growing/sealed segment 的实际执行
  - 向量/标量索引构建与加载

#### 从代码目录看这条边界

```text
Go 业务层
  internal/proxy/
  internal/datacoord/
  internal/querynodev2/
  internal/datanode/
  internal/flushcommon/
        |
        | CGO wrapper
        v
Go/C++ 桥接层
  internal/util/segcore/
  internal/util/indexcgowrapper/
        |
        | C API / protobuf blob
        v
C++ Core
  internal/core/src/segcore/
  internal/core/src/query/
  internal/core/src/exec/
  internal/core/src/index/
  internal/core/src/indexbuilder/
```

#### 最重要的判断方法

如果你看到下面这些特征，通常说明已经到了 Go/C++ 边界：

- `#cgo pkg-config: milvus_core`
- `import "C"`
- `s.csegment.Search(...)`
- `s.csegment.Insert(...)`
- `indexcgowrapper.CreateIndex(...)`
- `C.AsyncSearch(...)`
- `C.Insert(...)`

对应代码：

- query/search 桥接：
  [segment.go](/root/xty/milvus/internal/util/segcore/segment.go)
- QueryNode segment 封装：
  [segment.go](/root/xty/milvus/internal/querynodev2/segments/segment.go)
- index build 桥接：
  [task_index.go](/root/xty/milvus/internal/datanode/index/task_index.go)

#### 一句话理解

可以把 Milvus 想成：

```text
Go 负责把“哪张表、哪几个段、哪条请求、哪些参数”组织好
C++ 负责在“具体的 segment 上把这次执行真的跑出来”
```

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

#### 2.1.1 索引链路涉及的四类元数据

理解索引构建前，最好先把这几类东西分开：

1. **Collection Schema**
2. **Collection Index 定义**
3. **Segment Meta**
4. **SegmentIndex / Build Meta**

它们都属于“元数据”，但不是一回事。

```text
                    metastore (etcd / TiKV)
┌──────────────────────────────────────────────────────────────┐
│  1. Collection Schema                                       │
│     - collection 里有哪些字段                              │
│     - field type / dim / type params                        │
│                                                              │
│  2. Collection Index 定义                                   │
│     - 哪个 field 要建什么 index                             │
│     - HNSW / IVF / metric_type / user params                │
│                                                              │
│  3. Segment Meta                                            │
│     - segmentID / partitionID / channel / numRows           │
│     - state / storageVersion / binlog paths / manifest      │
│                                                              │
│  4. SegmentIndex / Build Meta                               │
│     - 某个 segment 的某个 index 的 build 任务状态           │
│     - buildID / fileKeys / size / state                     │
└──────────────────────────────────────────────────────────────┘

                    object storage (MinIO / S3)
┌──────────────────────────────────────────────────────────────┐
│  A. 字段 binlog                                              │
│     - 真正的原始列数据                                       │
│                                                              │
│  B. index files                                               │
│     - 真正的索引产物                                         │
└──────────────────────────────────────────────────────────────┘
```

最容易混淆的地方是：

- **Schema / Index / Segment / SegmentIndex** 这四类是元数据，主要在 **metastore**
- **binlog / index files** 才是对象存储里的实体文件

#### 这四类元数据分别是什么

| 类型 | 作用 | 什么时候创建 | 存哪里 | 关键代码 |
|------|------|-------------|-------|---------|
| Collection Schema | 定义 field type / dim / type params | `CreateCollection` 时 | RootCoordCatalog / metastore | `internal/rootcoord/meta_table.go` |
| Collection Index 定义 | 定义某 field 要建什么索引 | `CreateIndex` 时 | DataCoordCatalog / metastore | `internal/datacoord/index_service.go`, `internal/datacoord/index_meta.go` |
| Segment Meta | 描述某个 segment 当前状态和路径 | 开 segment 时创建，flush 时更新 | DataCoordCatalog / metastore | `internal/datacoord/segment_manager.go`, `internal/datacoord/meta.go`, `internal/datacoord/services.go` |
| SegmentIndex / Build Meta | 描述某 segment 的某 index 的 build 任务与产物 | DataCoord 为 flushed segment 创建 build task 时 | DataCoordCatalog / metastore | `internal/datacoord/index_inspector.go`, `internal/datacoord/task_index.go`, `internal/datacoord/index_meta.go` |

#### 这四类元数据谁创建、谁使用

| 元数据 | 谁创建 | 谁读取 |
|--------|-------|-------|
| Collection Schema | RootCoord | Proxy / DataCoord / QueryCoord / QueryNode |
| Collection Index 定义 | DataCoord（响应用户 CreateIndex） | DataCoord |
| Segment Meta | DataCoord | DataCoord / QueryCoord / QueryNode |
| SegmentIndex / Build Meta | DataCoord | DataCoord / QueryNode |

#### 一个特别容易搞混的点

**字段类型、维度 (`field type / dim`) 不属于 segment meta。**

它们来自 **Collection Schema**。

也就是说：

- `NumRows / StorageVersion / State / Binlogs` 是 segment 的属性
- `FieldType / dim / nullable / type params` 是 field schema 的属性

构建索引时，DataCoord 会把它们拼起来组成 `CreateJobRequest`。

#### 2.1.2 具体示例

下面固定一个例子：

```text
collectionID = 3001
partitionID  = 5001
segmentID    = 7001
fieldID      = 103
fieldName    = embedding
indexID      = 9001
buildID      = 88001
indexType    = HNSW
metricType   = L2
```

假设 `segment 7001` 里这个字段有两行向量：

```text
embedding = [
  [0.10, 0.20, 0.30, 0.40],
  [0.12, 0.18, 0.33, 0.39],
]
```

##### A. Collection Schema 长什么样

这是“字段本身是什么”的定义，来自 collection schema：

```text
CollectionSchema
  CollectionID = 3001
  Fields:
    FieldSchema{
      FieldID    = 103
      Name       = "embedding"
      DataType   = FloatVector
      TypeParams = {"dim":"4"}
    }
```

你可以在这些代码里看到它的来源和读取：

- collection 创建时落库：
  [meta_table.go#L553](/root/xty/milvus/internal/rootcoord/meta_table.go#L553)
- schema 模型本身：
  [collection.go](/root/xty/milvus/internal/metastore/model/collection.go)
  [field.go](/root/xty/milvus/internal/metastore/model/field.go)

##### B. Collection Index 定义长什么样

这是“这个 field 应该建什么索引”的逻辑定义：

```text
Index
  CollectionID    = 3001
  FieldID         = 103
  IndexID         = 9001
  IndexName       = "idx_embedding_hnsw"
  TypeParams      = {"dim":"4"}
  IndexParams     = {
    "index_type":"HNSW",
    "metric_type":"L2",
    "M":"16",
    "efConstruction":"200"
  }
```

它的创建和落库链路是：

- Proxy 转发用户 `CreateIndex`：
  [task_index.go#L746](/root/xty/milvus/internal/proxy/task_index.go#L746)
- DataCoord 构造 `model.Index` 并广播：
  [index_service.go#L264](/root/xty/milvus/internal/datacoord/index_service.go#L264)
  [index_service.go#L279](/root/xty/milvus/internal/datacoord/index_service.go#L279)
- ack callback 后正式持久化：
  [ddl_callbacks_create_index.go#L26](/root/xty/milvus/internal/datacoord/ddl_callbacks_create_index.go#L26)
  [index_meta.go#L496](/root/xty/milvus/internal/datacoord/index_meta.go#L496)
- catalog 接口定义：
  [catalog.go#L199](/root/xty/milvus/internal/metastore/catalog.go#L199)

##### C. Segment Meta 长什么样

这是“这个 segment 当前怎么样”的定义：

```text
SegmentInfo
  ID              = 7001
  CollectionID    = 3001
  PartitionID     = 5001
  InsertChannel   = "ch0"
  NumOfRows       = 2
  State           = Flushed
  StorageVersion  = 3
  Binlogs         = [...]
  Statslogs       = [...]
  Deltalogs       = [...]
  ManifestPath    = "..."
```

它分两阶段形成：

**第一次创建**：segment 打开时

- 初始 `NumOfRows = 0`
- 初始 `State = Growing`
- 初始 `StorageVersion = req.StorageVersion`

代码：

- 创建 `datapb.SegmentInfo`：
  [segment_manager.go#L413](/root/xty/milvus/internal/datacoord/segment_manager.go#L413)
- 持久化到 metastore：
  [meta.go#L648](/root/xty/milvus/internal/datacoord/meta.go#L648)
  [catalog.go#L180](/root/xty/milvus/internal/metastore/catalog.go#L180)

**后续更新**：flush 完成时

- `NumOfRows` 更新为真实 flushed 行数
- `State` 变为 `Flushed`
- `Binlogs / Statslogs / Deltalogs / ManifestPath` 回写
- `StorageVersion` 也通过 `SaveBinlogPaths` 回写确认

代码：

- flush 端构造 `SaveBinlogPathsRequest`：
  [meta_writer.go#L99](/root/xty/milvus/internal/flushcommon/syncmgr/meta_writer.go#L99)
- DataCoord 收到后组装 operators：
  [services.go#L560](/root/xty/milvus/internal/datacoord/services.go#L560)
  [services.go#L662](/root/xty/milvus/internal/datacoord/services.go#L662)
- 最终 `AlterSegments(...)` 持久化：
  [meta.go#L1380](/root/xty/milvus/internal/datacoord/meta.go#L1380)
  [catalog.go#L181](/root/xty/milvus/internal/metastore/catalog.go#L181)

##### D. SegmentIndex / Build Meta 长什么样

这是“某个具体 segment 的某个索引 build 到哪一步了”的元数据：

```text
SegmentIndex
  SegmentID            = 7001
  CollectionID         = 3001
  PartitionID          = 5001
  NumRows              = 2
  IndexID              = 9001
  BuildID              = 88001
  IndexState           = Unissued / InProgress / Finished / Failed
  IndexVersion         = 1
  IndexFileKeys        = ["hnsw_meta", "hnsw_graph", ...]
  IndexSerializedSize  = ...
  IndexMemSize         = ...
  IndexType            = "HNSW"
```

这个结构和上面的 `Index` 不是一回事：

- `Index`：collection 级逻辑定义
- `SegmentIndex`：segment 级实际 build 任务

代码：

- 模型定义：
  [segment_index.go](/root/xty/milvus/internal/metastore/model/segment_index.go)
- flushed segment 触发 build task：
  [index_inspector.go#L183](/root/xty/milvus/internal/datacoord/index_inspector.go#L183)
- 创建并持久化 `SegmentIndex`：
  [index_inspector.go#L238](/root/xty/milvus/internal/datacoord/index_inspector.go#L238)
  [index_meta.go#L516](/root/xty/milvus/internal/datacoord/index_meta.go#L516)
- catalog 接口定义：
  [catalog.go#L204](/root/xty/milvus/internal/metastore/catalog.go#L204)

##### 这四类元数据如何拼成一次真正的索引构建请求

构建 `CreateJobRequest` 时，DataCoord 实际做的是：

```text
CreateJobRequest =
  Segment Meta
    + Collection Index 定义
    + Collection Schema
    + 对象存储里的字段 binlog 路径
```

对应代码：

- `prepareJobRequest()`：
  [task_index.go#L227](/root/xty/milvus/internal/datacoord/task_index.go#L227)

最关键几项：

```text
来自 Collection Schema:
  FieldType = FloatVector
  Dim       = 4

来自 Collection Index 定义:
  IndexParams = HNSW / L2 / M / efConstruction

来自 Segment Meta:
  SegmentID      = 7001
  NumRows        = 2
  StorageVersion = 3

来自对象存储:
  DataIds / DataPaths -> insert_log/.../7001/103/...
```

##### 最后用一张表收束

| 信息 | 本例中的值 | 创建时机 | 存储位置 | 用途 |
|------|-----------|---------|---------|------|
| Collection Schema | `field 103 = embedding, FloatVector, dim=4` | CreateCollection | metastore | 告诉系统 field 的物理类型 |
| Collection Index | `index 9001 = HNSW(L2, M=16, efConstruction=200)` | CreateIndex | metastore | 告诉系统要建什么索引 |
| Segment Meta | `segment 7001, numRows=2, storageVersion=3, state=Flushed` | 开 segment + flush 更新 | metastore | 告诉系统这个段当前状态和路径 |
| 字段 binlog | `insert_log/.../7001/103/...` | Flush | object storage | 提供真正的原始向量数据 |
| SegmentIndex | `buildID 88001, state=Finished, fileKeys=[...]` | 为 flushed segment 创任务时 | metastore | 跟踪这次 build 的产物和状态 |

所以你前面问的那三个输入，如果更准确地重写，应该是：

```text
1. Flush 后对象存储里的字段 binlog         -> 原始数据
2. Collection 上定义好的索引元数据         -> 建什么索引
3. Segment 自身的 numRows / storageVersion -> 这个段的运行时属性
4. Collection Schema 里的 field type / dim -> 这个字段的类型说明
```

#### 2.1.3 元数据创建时序

把前面的元数据放到时间线上看，会更容易理解“谁先有，谁后有”：

```text
T0: CreateCollection
  RootCoord
    -> 创建 Collection Schema
    -> 持久化到 metastore

T1: Insert 开始，StreamingNode / DataCoord 开新段
  DataCoord
    -> 创建 Segment Meta
    -> 初始:
       NumRows = 0
       State = Growing
       StorageVersion = V2/V3/...
    -> 持久化到 metastore

T2: Flush
  Object Storage
    -> 写出字段 binlog / statslog / deltalog
  DataCoord
    -> 更新 Segment Meta
       NumRows = flushed rows
       State = Flushed
       Binlogs / Statslogs / Deltalogs / ManifestPath 已可见
    -> 持久化到 metastore

T3: 用户调用 CreateIndex
  DataCoord
    -> 创建 Collection Index 定义
    -> 持久化到 metastore

T4: indexInspector 发现 “这个 collection 有索引定义” 且 “这个 segment 已 flushed”
  DataCoord
    -> 为 (segment, index) 创建 SegmentIndex
    -> 分配 BuildID
    -> 持久化到 metastore

T5: DataNode Build
  DataNode
    -> 读字段 binlog
    -> 结合 schema + index params build 索引
    -> 上传 index files 到 object storage

T6: DataCoord 查询 worker 结果
  DataCoord
    -> 更新 SegmentIndex
       State = Finished
       IndexFileKeys = [...]
       IndexSerializedSize / IndexMemSize = ...
    -> 持久化到 metastore

T7: QueryNode LoadIndex
  QueryNode
    -> 调 DataCoord GetIndexInfos
    -> 拿到 IndexFilePaths
    -> 从 object storage / DiskCache 加载索引文件
```

最关键的依赖关系是：

```text
Collection Schema
    先于
Collection Index 定义
    和
Segment Meta
    先于
SegmentIndex(BuildID)
    先于
index files 真正生成
```

换句话说：

- 没有 collection schema，就不知道 `field 103` 是不是向量、维度是多少
- 没有 flushed segment meta，就不知道有哪些 segment 需要建索引
- 没有 collection index 定义，就不知道该建 HNSW 还是 IVF
- 没有 BuildID，就没有一轮具体的 segment 级 build 任务

#### 2.1.4 BuildID 和索引文件的生命周期

`BuildID` 是索引链路里最容易迷糊、但又最关键的那个 ID。

它不是：

- collection ID
- field ID
- index ID
- segment ID

它表示的是：

**一次具体的“给某个 segment 构建某个 index”的任务实例 ID。**

继续用前面的例子：

```text
CollectionID = 3001
FieldID      = 103
IndexID      = 9001
SegmentID    = 7001
BuildID      = 88001
```

这几个 ID 的关系可以这样看：

```text
Collection 3001
  └─ field 103 = embedding
      └─ index 9001 = idx_embedding_hnsw
          ├─ segment 7001 -> build 88001
          ├─ segment 7002 -> build 88002
          └─ segment 7015 -> build 88015
```

也就是说：

- 一个 collection index 定义，会展开成很多个 segment build
- 每个 segment build 都有自己的 `BuildID`

##### BuildID 在代码里是怎么走的

**Step A. 分配 BuildID**

- `indexInspector.createIndexForSegment()` 里分配
  [index_inspector.go#L205](/root/xty/milvus/internal/datacoord/index_inspector.go#L205)

**Step B. 写入 SegmentIndex 元数据**

- `BuildID` 被写入 `model.SegmentIndex`
  [index_inspector.go#L238](/root/xty/milvus/internal/datacoord/index_inspector.go#L238)
  [segment_index.go](/root/xty/milvus/internal/metastore/model/segment_index.go)

**Step C. 发送给 DataNode**

- `CreateJobRequest.BuildID = it.BuildID`
  [task_index.go#L323](/root/xty/milvus/internal/datacoord/task_index.go#L323)

**Step D. DataNode build 完后记录 file keys**

- `StoreIndexFilesAndStatistic(buildID, fileKeys, ...)`
  [task_index.go#L367](/root/xty/milvus/internal/datanode/index/task_index.go#L367)

**Step E. DataCoord 查询结果并回写**

- 通过 `BuildID` 查询 worker result
  [task_index.go#L390](/root/xty/milvus/internal/datacoord/task_index.go#L390)

##### index files 和 BuildID 的关系

对象存储里的索引文件路径是按 `BuildID` 组织的：

```text
index/{buildID}/{indexVersion}/{partitionID}/{segmentID}/{fileKey}
```

例如：

```text
index/88001/1/5001/7001/hnsw_meta
index/88001/1/5001/7001/hnsw_graph
index/88001/1/5001/7001/hnsw_data
```

所以你可以把 `BuildID` 理解成：

- segment 级索引构建任务的主键
- index files 在对象存储里的一级目录 key
- DataCoord / DataNode / QueryNode 串联这轮索引构建的“工作单号”

##### QueryNode 最终拿到的不是 BuildID，而是 `IndexFilePaths`

QueryNode 加载索引前，会先向 DataCoord 查询：

- `GetIndexInfos`
  [index_service.go#L1050](/root/xty/milvus/internal/datacoord/index_service.go#L1050)

DataCoord 会把 `SegmentIndex + Collection Index 定义` 拼成：

```text
IndexFilePathInfo
  SegmentID       = 7001
  FieldID         = 103
  IndexID         = 9001
  BuildID         = 88001
  IndexName       = "idx_embedding_hnsw"
  IndexParams     = ...
  IndexFilePaths  = [
    "index/88001/1/5001/7001/hnsw_meta",
    "index/88001/1/5001/7001/hnsw_graph",
    ...
  ]
  NumRows         = 2
```

然后 QueryNode 在 `LoadIndex()` 里真正消费的是：

- `IndexFilePaths`
- `IndexParams`
- `FieldID`
- `BuildID`

对应代码：

- DataCoord 组装 `IndexFilePathInfo`
  [index_service.go#L1078](/root/xty/milvus/internal/datacoord/index_service.go#L1078)
- QueryNode 加载
  [segment_loader.go#L2253](/root/xty/milvus/internal/querynodev2/segments/segment_loader.go#L2253)

##### 用一句话把 BuildID 说透

如果 `IndexID` 是“这个 collection 的这个 field 应该有一个 HNSW 索引”，

那么 `BuildID` 就是：

**“具体给 segment 7001 构建这套 HNSW 索引的这一轮任务编号。”**

#### 2.1.5 五类信息并排对照表

为了避免再把 schema、index、segment、build、文件混在一起，下面把它们并排摆出来。

继续固定这个例子：

```text
collectionID = 3001
fieldID      = 103
segmentID    = 7001
indexID      = 9001
buildID      = 88001
```

##### A. 站在“存储介质”维度看

| 名称 | 属于元数据还是实体文件 | 存储介质 | 是否长期存在 | 主要内容 |
|------|------------------------|----------|-------------|---------|
| Collection Schema | 元数据 | metastore | 是 | 字段定义、类型、维度、type params |
| Collection Index | 元数据 | metastore | 是 | 某字段应该建什么索引 |
| Segment Meta | 元数据 | metastore | 是 | 段状态、行数、binlog 路径、storage version |
| SegmentIndex | 元数据 | metastore | 是 | 某段某索引的一次 build 任务及结果 |
| 字段 binlog | 实体文件 | object storage | 是 | 原始列数据 |
| index files | 实体文件 | object storage | 是 | 真正的索引产物 |

##### B. 站在“谁生产、谁消费”维度看

| 信息 | 生产者 | 消费者 | 典型用途 |
|------|-------|-------|---------|
| Collection Schema | RootCoord | Proxy / DataCoord / QueryNode | 告诉系统 field 103 是 FloatVector(dim=4) |
| Collection Index | DataCoord | DataCoord | 告诉系统 field 103 要建 HNSW(L2) |
| Segment Meta | DataCoord | DataCoord / QueryCoord / QueryNode | 告诉系统 segment 7001 已 flushed，numRows=2 |
| SegmentIndex | DataCoord + DataNode | DataCoord / QueryNode | 告诉系统 build 88001 是否 finished、产物在哪 |
| 字段 binlog | Flush 链路 | DataNode / QueryNode | build 索引、加载原始字段数据 |
| index files | DataNode | QueryNode | 真正参与 Sealed Segment 搜索 |

##### C. 站在“代码对象”维度看

| 概念 | 代码模型 | 关键字段 |
|------|---------|---------|
| Collection Schema | `model.Collection` + `model.Field` | `Fields`, `TypeParams`, `DataType` |
| Collection Index | `model.Index` | `CollectionID`, `FieldID`, `IndexID`, `IndexParams` |
| Segment Meta | `datapb.SegmentInfo` / `datacoord.SegmentInfo` | `NumOfRows`, `State`, `StorageVersion`, `Binlogs` |
| SegmentIndex | `model.SegmentIndex` | `BuildID`, `IndexState`, `IndexFileKeys`, `NumRows` |
| QueryNode 加载视图 | `indexpb.IndexFilePathInfo` / `querypb.FieldIndexInfo` | `BuildID`, `IndexFilePaths`, `NumRows`, `IndexParams` |

##### D. 站在“这个例子具体长什么样”维度看

| 信息 | 本例中的近似内容 |
|------|------------------|
| Collection Schema | `embedding(fieldID=103, FloatVector, dim=4)` |
| Collection Index | `idx_embedding_hnsw(indexID=9001, HNSW, metric=L2, M=16, efConstruction=200)` |
| Segment Meta | `segment 7001, state=Flushed, numRows=2, storageVersion=3` |
| SegmentIndex | `build 88001, segment=7001, index=9001, state=Finished, fileKeys=[...]` |
| 字段 binlog | `insert_log/.../7001/103/...` |
| index files | `index/88001/1/5001/7001/...` |

##### E. 最容易误解的 5 句话

1. `Collection Index` 不是索引文件，它只是“要建什么索引”的逻辑定义。
2. `SegmentIndex` 不是 collection 级配置，它是某个 segment 的一次具体 build 结果。
3. `field type / dim` 不在 segment meta 里，它们来自 collection schema。
4. `index files` 不是 flush 顺手写出来的，它们是 DataNode build 完后单独上传的。
5. QueryNode 真正加载索引时，消费的是 `IndexFilePaths`，不是 `Collection Index` 本身。

#### 2.1.6 一条完整时序：从 CreateIndex 到 QueryNode LoadIndex

下面用一条完整时序把“请求、元数据、对象存储、build、加载”串起来：

```text
用户
  |
  | 1. CreateIndex(collection=3001, field=103, index=HNSW)
  v
Proxy
  |
  | 2. 转发 indexpb.CreateIndexRequest
  v
DataCoord.CreateIndex
  |
  | 3. 读取 Collection Schema，确认 field 103 存在且是向量字段
  | 4. 构造 model.Index(indexID=9001)
  | 5. 广播 CreateIndexMessage
  v
DataCoord DDL Ack Callback
  |
  | 6. indexMeta.CreateIndex()
  |    -> metastore 持久化 Collection Index
  |
  | 7. notifyIndexChan <- collectionID
  v
indexInspector
  |
  | 8. 找到所有 flushed segments
  |    发现 segment 7001 符合条件
  |
  | 9. 为 (segment 7001, index 9001) 分配 BuildID = 88001
  | 10. 创建 model.SegmentIndex
  | 11. metastore 持久化 SegmentIndex
  | 12. prepareJobRequest()
  |     -> 从 Segment Meta 拿 NumRows / StorageVersion / binlog IDs
  |     -> 从 Collection Schema 拿 FieldType / Dim
  |     -> 从 Collection Index 拿 IndexParams
  v
DataNode indexBuildTask
  |
  | 13. 读取字段 binlog:
  |     insert_log/3001/5001/7001/103/...
  |
  | 14. 调底层 CreateIndex(buildIndexInfo)
  |     -> build HNSW
  |
  | 15. 上传 index files:
  |     index/88001/1/5001/7001/...
  |
  | 16. 本地记录 fileKeys / size / state
  v
DataCoord QueryIndex
  |
  | 17. 查询 worker build 88001 状态
  | 18. 回写 SegmentIndex:
  |     state = Finished
  |     fileKeys = [...]
  |     serializedSize = ...
  v
QueryCoord / QueryNode
  |
  | 19. 需要加载 segment 7001 时，请求 GetIndexInfos
  v
DataCoord.GetIndexInfos
  |
  | 20. 根据 SegmentIndex + Collection Index
  |     组装 IndexFilePathInfo:
  |       BuildID = 88001
  |       FieldID = 103
  |       IndexFilePaths = index/88001/1/5001/7001/...
  v
QueryNode.LoadIndex
  |
  | 21. 从 object storage / DiskCache 读 index files
  | 22. append 到本地 Sealed Segment
  v
后续 Search
  |
  | 23. segment 7001 可优先走 HNSW，而不是只靠原始向量暴力扫描
```

##### 这条时序里，每一步最依赖哪份信息

| 步骤 | 依赖的核心信息 |
|------|---------------|
| 3 | Collection Schema |
| 4 | Collection Index 定义（用户请求） |
| 8 | Segment Meta |
| 12 | Segment Meta + Collection Schema + Collection Index + 字段 binlog 路径 |
| 13 | 字段 binlog |
| 15 | BuildID |
| 18 | SegmentIndex |
| 20 | SegmentIndex + Collection Index |
| 21 | index files |

##### 用一句话把这一大段收束

```text
CreateIndex 创建的是“定义”，
Flush 产出的是“原始数据文件”，
BuildID 绑定的是“某个 segment 的一次具体构建任务”，
QueryNode 最终加载的是“这次任务产出的 index files”。
```

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

### 4.0 用户视角：想插入一批数据，到底要准备什么

如果先不看内部实现，只站在“我要把一批数据写进 Milvus”这个角度，最重要的是先分清：

- **插入数据本身需要什么**
- **为了后续搜索/查询还要做什么**
- **哪些步骤是必须的，哪些是可选的**

#### 4.0.1 最短答案

如果你只是想把一批数据插进去，最少需要：

1. 一个数据库（默认 `default` 也可以）
2. 一个 collection
3. collection 的 schema
4. 一批符合 schema 的数据

如果你还希望这批数据后面能高效地被搜索，一般还需要：

5. 给向量字段建索引（`CreateIndex`）
6. 把 collection/partition load 到 QueryNode（`LoadCollection` / `LoadPartitions`）

#### 4.0.2 “必须 / 可选 / 为了搜索建议做”的分类

| 步骤 | 是否必须 | 作用 | 不做会怎样 |
|------|---------|------|-----------|
| `CreateCollection` | 必须 | 定义 schema、主键、向量字段 | 没地方插数据 |
| `CreatePartition` | 可选 | 如果你想显式写到某个 partition | 不写就用默认 partition 或 partition key 路由 |
| `Insert` | 必须 | 真正写入数据 | 没有数据 |
| `CreateIndex` | 对“插入”不是必须；对“高性能搜索”强烈建议 | 给向量字段或其他字段建立索引定义 | 还能搜索，但通常会更慢，很多段会走 brute force |
| `LoadCollection` / `LoadPartitions` | 对“插入”不是必须；对“查询/搜索”通常必须 | 把 segment 和索引加载到 QueryNode | 不能正常 search/query |
| `Flush` | 可选 | 让数据尽快从 growing 变成 flushed | 不影响 insert 成功；只是数据可能暂时还主要在 growing segment |
| `ReleaseCollection` | 可选 | 释放查询资源 | 不影响插入，只影响资源占用 |

#### 4.0.3 一次插入前，用户真正要准备的 6 件事

##### 1. 先决定你的 collection 长什么样

你至少要想清楚这些问题：

| 你要决定的事 | 例子 | 为什么要先定 |
|-------------|------|-------------|
| collection 名字 | `product_catalog` | 数据最终写到哪张逻辑表 |
| 主键字段 | `id` | 每行数据如何唯一标识 |
| 向量字段 | `embedding` | 后面在哪个字段上做向量搜索 |
| 向量维度 | `dim = 4` 或 `dim = 768` | 插入向量时必须匹配 |
| 标量字段 | `title`, `price` | 过滤条件和结果返回要用 |
| 主键模式 | 手动主键 / AutoID | 决定 insert 时要不要自己传主键 |

对于向量库场景，通常至少要有：

- 一个主键字段
- 一个向量字段

##### 2. 想清楚 partition 要不要自己管

你有三种常见模式：

1. **只用默认 partition**
   最简单，不额外创建 partition。

2. **手动指定 partition**
   例如你想把数据写到 `p1`，那就要先 `CreatePartition("p1")`。

3. **partition key 自动路由**
   让系统根据某个字段自动决定 partition。

如果你这次 insert 带了：

```json
"partition_name": "p1"
```

那一般意味着：

- 你要么已经先创建了 `p1`
- 要么系统处于 partition key 模式并会另外处理路由

##### 3. 你的数据必须符合 schema

还是用这个例子：

```json
{
  "db_name": "demo",
  "collection_name": "product_catalog",
  "partition_name": "p1",
  "num_rows": 3,
  "fields_data": [
    {"field_name": "id",        "type": "Int64", "data": [101, 102, 103]},
    {"field_name": "title",     "type": "VarChar", "data": ["red mug", "blue bottle", "green tea"]},
    {"field_name": "price",     "type": "Float", "data": [19.8, 29.9, 9.9]},
    {"field_name": "embedding", "type": "FloatVector(dim=4)", "data": [
      0.10, 0.20, 0.30, 0.40,
      0.40, 0.10, 0.20, 0.30,
      0.12, 0.18, 0.33, 0.39
    ]}
  ]
}
```

你至少要保证：

- `id` 真的是 `Int64`
- `price` 真的是 `Float`
- `embedding` 的维度真的是 4
- `num_rows = 3`
- 每一列都对应 3 行

也就是说，下面这些都必须对齐：

```text
id    -> 3 个值
title -> 3 个值
price -> 3 个值
embedding -> 3 个向量，每个向量 dim=4
```

##### 4. 先搞清楚：Insert 不等于 CreateIndex

这是最容易误解的点之一。

`Insert` 只做一件事：

- 把数据写进去

它不会要求你每次都顺手带上：

- indexName
- indexType
- metricType
- `M`
- `efConstruction`

这些属于 **`CreateIndex`** 这条 API，不属于 `Insert`。

所以：

- `Insert`：你传的是数据
- `CreateIndex`：你传的是索引定义

#### 4.0.4 用户完整流程：只插入 vs 插入后还要搜索

##### 场景 A：我只想把数据插进去

这时最小流程是：

```text
CreateCollection
  -> (可选) CreatePartition
  -> Insert
```

这就够了。

索引不是必须，load 也不是必须。

##### 场景 B：我插完之后还要做向量搜索

这时常见流程是：

```text
CreateCollection
  -> (可选) CreatePartition
  -> (建议) CreateIndex on embedding
  -> LoadCollection
  -> Insert
  -> Search
```

这里几个关键点：

- `CreateIndex` 是为了加速搜索，不是为了让 insert 成功
- `LoadCollection` 是为了让 QueryNode 能接管搜索
- 新插入的数据即使还没 flush，也可能先以 growing segment 的形式被搜索到
- flush 完并建好索引后，历史段搜索通常会更快

##### 场景 C：我要“可控地”让索引尽快生效

这时你可能会走：

```text
CreateCollection
  -> CreateIndex
  -> LoadCollection
  -> Insert
  -> Flush
  -> 等索引构建完成
  -> Search
```

这样做的目的通常不是“才能搜索”，而是：

- 希望数据尽快进入 flushed segment
- 希望索引尽快 build 完
- 希望后续搜索尽量走索引

#### 4.0.5 一个最小 API 清单

下面是从“什么都没有”到“能插入并搜索”的最小用户流程。

##### Step 1. CreateCollection

你至少需要定义：

- collection 名
- 主键字段
- 向量字段
- 向量维度
- 可选标量字段

近似可以理解成：

```text
CreateCollection(
  collection = "product_catalog",
  schema = {
    id:        Int64 primary key,
    title:     VarChar,
    price:     Float,
    embedding: FloatVector(dim=4)
  }
)
```

##### Step 2. Optional CreatePartition

如果你后面要显式指定：

```json
"partition_name": "p1"
```

那你通常先要：

```text
CreatePartition(collection="product_catalog", partition="p1")
```

##### Step 3. Insert

插入真正的数据：

```json
{
  "db_name": "demo",
  "collection_name": "product_catalog",
  "partition_name": "p1",
  "num_rows": 3,
  "fields_data": [
    {"field_name": "id", "type": "Int64", "data": [101, 102, 103]},
    {"field_name": "title", "type": "VarChar", "data": ["red mug", "blue bottle", "green tea"]},
    {"field_name": "price", "type": "Float", "data": [19.8, 29.9, 9.9]},
    {"field_name": "embedding", "type": "FloatVector(dim=4)", "data": [
      0.10, 0.20, 0.30, 0.40,
      0.40, 0.10, 0.20, 0.30,
      0.12, 0.18, 0.33, 0.39
    ]}
  ]
}
```

##### Step 4. Optional but Recommended CreateIndex

如果你想对 `embedding` 做向量搜索，通常会单独发：

```text
CreateIndex(
  collection = "product_catalog",
  field      = "embedding",
  indexName  = "idx_embedding_hnsw",
  indexType  = "HNSW",
  metricType = "L2",
  params     = {"M":"16", "efConstruction":"200"}
)
```

重点：

- 这是用户 API，可以直接调用
- 它不是 insert 的一部分
- 它只对 `embedding` 这一个字段建索引，不会顺带给 `price/title/id` 建进这套 HNSW

##### Step 5. LoadCollection

如果你后面要 `Search / Query`，通常还要：

```text
LoadCollection("product_catalog")
```

这一步的作用是：

- 把 segment 和索引加载到 QueryNode
- 让后续查询请求能真正被执行

#### 4.0.6 “我没考虑到还需要创建 index，还有其他的吗？”

这个问题最实用的回答是：

**取决于你的目标。**

##### 如果你的目标只是“把数据存进去”

你只需要：

- `CreateCollection`
- `Insert`

以及可选的：

- `CreatePartition`

##### 如果你的目标是“后面还能高效搜索”

你通常还要考虑：

- `CreateIndex`
- `LoadCollection`

##### 如果你的目标是“尽快让索引参与历史段搜索”

你还可能关心：

- `Flush`
- 等待 index build 完成

#### 4.0.7 最终把“必须”和“建议”收成一句话

如果你今天就要把数据插进去，脑子里记这一版就够了：

```text
必须：
  1. 先有 collection（schema 要对）
  2. 可选有 partition
  3. 插入的数据要和 schema 对齐

为了后面搜索更好：
  4. 给向量字段单独 CreateIndex
  5. LoadCollection 到 QueryNode

可选优化：
  6. Flush，等索引构建完成
```

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

#### 统一示例：后面所有插入和查询都复用这批数据

为了把“数据形态”说清楚，下面固定使用同一个集合和同一批 3 行数据。

**示例 Collection Schema**:

| 字段 | FieldID | 类型 | 说明 |
|------|---------|------|------|
| `id` | 100 | Int64 | 业务主键 |
| `title` | 101 | VarChar | 商品标题 |
| `price` | 102 | Float | 标量字段 |
| `embedding` | 103 | FloatVector(dim=4) | 向量字段 |

**用户脑子里通常看到的是“按行”的数据**:

```json
[
  {"id": 101, "title": "red mug",     "price": 19.8, "embedding": [0.10, 0.20, 0.30, 0.40]},
  {"id": 102, "title": "blue bottle", "price": 29.9, "embedding": [0.40, 0.10, 0.20, 0.30]},
  {"id": 103, "title": "green tea",   "price":  9.9, "embedding": [0.12, 0.18, 0.33, 0.39]}
]
```

但 Milvus 在传输和内部处理时，核心是**列式**组织，不是行式。

**InsertRequest 在 gRPC / Proxy 这一层更接近下面这样**:

```json
{
  "db_name": "demo",
  "collection_name": "product_catalog",
  "partition_name": "p1",
  "num_rows": 3,
  "fields_data": [
    {"field_name": "id",        "type": "Int64",            "data": [101, 102, 103]},
    {"field_name": "title",     "type": "VarChar",          "data": ["red mug", "blue bottle", "green tea"]},
    {"field_name": "price",     "type": "Float",            "data": [19.8, 29.9, 9.9]},
    {"field_name": "embedding", "type": "FloatVector(dim=4)", "data": [
      0.10, 0.20, 0.30, 0.40,
      0.40, 0.10, 0.20, 0.30,
      0.12, 0.18, 0.33, 0.39
    ]}
  ]
}
```

这里最容易看错的是：

- `num_rows = 3` 表示有 3 行实体
- `fields_data` 长度是 4，表示有 4 列字段
- 向量字段在列式结构里通常会被展开成一个扁平数组，再配合 `dim=4` 解释

#### 这批数据在整个插入链路里的核心变化

先记住下面这几个变化，后面每一步只是把它展开：

1. 业务侧按“行”理解数据
2. Proxy / msgstream 按“列”处理数据
3. 分片时按“行 offset”拆分到不同 channel
4. Segment Flush 到对象存储后，又按“每个字段一个 binlog 文件”落盘
5. 查询时再把这些列式数据和索引加载回来做搜索

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

#### 进入 Proxy 后，这批数据长什么样

`insertTask.insertMsg.FieldsData` 仍然是列式的：

```text
FieldData[id]        = [101, 102, 103]
FieldData[title]     = ["red mug", "blue bottle", "green tea"]
FieldData[price]     = [19.8, 29.9, 9.9]
FieldData[embedding] = [
  0.10, 0.20, 0.30, 0.40,
  0.40, 0.10, 0.20, 0.30,
  0.12, 0.18, 0.33, 0.39
]
NumRows = 3
```

如果 `id` 是用户提供的业务主键，那么用户看到的主键还是 `[101, 102, 103]`；但 Proxy 仍然会为每一行额外分配**内部 RowID**，例如：

```text
Business PKs = [101, 102, 103]
Internal RowIDs = [90001, 90002, 90003]
```

这两个概念不要混：

- `id` 是业务主键，用于去重、删除、查询返回
- `RowID` 是系统内部行标识，用于写链路和底层存储组织

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

#### 这一步最关键：一行数据只会路由到一个 Channel

假设这个 Collection 有 2 个 VChannel：

- `by-dev-rootcoord-dml_0v0`
- `by-dev-rootcoord-dml_1v0`

假设主键 Hash 后的结果如下：

```text
id=101 -> channel 0
id=102 -> channel 1
id=103 -> channel 0
```

那么按行 offset 分组结果就是：

```text
channel 0 -> rowOffsets [0, 2]
channel 1 -> rowOffsets [1]
```

接下来原来的 1 个 `InsertRequest` 会被拆成 2 个 `InsertMsg`。

**InsertMsg for channel 0**:

```text
CollectionID = 3001
PartitionID  = 5001
ShardName    = "by-dev-rootcoord-dml_0v0"
NumRows      = 2
RowIDs       = [90001, 90003]
PrimaryKeys  = [101, 103]

FieldsData:
  id        = [101, 103]
  title     = ["red mug", "green tea"]
  price     = [19.8, 9.9]
  embedding = [
    0.10, 0.20, 0.30, 0.40,
    0.12, 0.18, 0.33, 0.39
  ]
```

**InsertMsg for channel 1**:

```text
CollectionID = 3001
PartitionID  = 5001
ShardName    = "by-dev-rootcoord-dml_1v0"
NumRows      = 1
RowIDs       = [90002]
PrimaryKeys  = [102]

FieldsData:
  id        = [102]
  title     = ["blue bottle"]
  price     = [29.9]
  embedding = [0.40, 0.10, 0.20, 0.30]
```

也就是说，**插入链路是“按行切分、按列存储”**：

- 按行切分：哪一行去哪个 channel
- 按列存储：每个 channel 内部还是列式 `FieldsData`

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

#### WAL 里的一条消息，头和 body 分别是什么

继续以上面的 `channel 0` 那条消息为例，它在 WAL 里更接近下面的结构：

```text
MutableMessage
  VChannel = "by-dev-rootcoord-dml_0v0"
  Header = {
    CollectionId: 3001,
    Partitions: [
      {
        PartitionId: 5001,
        Rows: 2
      }
    ]
  }
  Body = InsertRequest{
    CollectionName: "product_catalog",
    PartitionName:  "p1",
    SegmentID:      0 or placeholder,
    NumRows:        2,
    RowIDs:         [90001, 90003],
    FieldsData:     ...
  }
```

这里要注意：

- **Header** 更偏路由和元信息
- **Body** 才是实际插入的数据
- 在 streaming 场景下，某些 `SegmentID` 可能先是占位，后续再由下游链路分配/确定

#### 客户端最终拿到什么

插入成功后，客户端看到的返回值一般可以理解为：

```text
MutationResult
  Status    = Success
  IDs       = [101, 102, 103]     // 返回业务主键，不是内部 RowID
  InsertCnt = 3
  Timestamp = 4567890001
```

所以从业务视角看，“插入结束”发生在 **Proxy 成功把消息写入 WAL 并拿到可见时间戳** 这一刻，不等于已经 Flush 到对象存储。

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

#### 写缓冲里再看一次这批数据

假设 `channel 0` 当前对应的 Growing Segment 是 `7001`，`channel 1` 对应的是 `7002`，那么写缓冲里大致会变成：

```text
Growing Segment 7001 (channel 0)
  InsertRecord:
    num_rows = 2
    fields_data:
      id        = [101, 103]
      title     = ["red mug", "green tea"]
      price     = [19.8, 9.9]
      embedding = [
        0.10, 0.20, 0.30, 0.40,
        0.12, 0.18, 0.33, 0.39
      ]

Growing Segment 7002 (channel 1)
  InsertRecord:
    num_rows = 1
    fields_data:
      id        = [102]
      title     = ["blue bottle"]
      price     = [29.9]
      embedding = [0.40, 0.10, 0.20, 0.30]
```

如果你**刚 insert 完就立刻 search**，而此时还没 flush，这批数据就是从这些 **Growing Segments** 被搜索到的。

#### 4.5.1 Go 到 C++ 的真正插入点

这里很容易误解成“插入全程都在调 C++”。其实不是。

系统级 insert 的大部分步骤仍然是 Go 在做：

- Proxy 收请求
- 校验 schema
- 列式组织
- 按 channel 分片
- 写 WAL
- StreamingNode / WriteBuffer 缓冲
- Flush 到对象存储

但是当一批数据真正要进入**执行层里的 growing segment** 时，底层会调 C++ segcore。

##### Go 层看到的数据

继续用 `segment 7001` 这个例子：

```text
segment 7001
  id        = [101, 103]
  title     = ["red mug", "green tea"]
  price     = [19.8, 9.9]
  embedding = [
    0.10, 0.20, 0.30, 0.40,
    0.12, 0.18, 0.33, 0.39
  ]
```

在 QueryNode / segcore 这一层，这批数据会被封装成：

```text
InsertRequest
  RowIDs     = [90001, 90003]
  Timestamps = [ts, ts]
  Record     = InsertRecord{
    fields_data = [...]
    num_rows    = 2
  }
```

##### 真正进入 C++ 的位置

Go 侧封装：

- `LocalSegment.Insert(...)`
  [segment.go#L760](/root/xty/milvus/internal/querynodev2/segments/segment.go#L760)

里面真正调用 CGO：

- `s.csegment.Insert(...)`
  [segment.go#L781](/root/xty/milvus/internal/querynodev2/segments/segment.go#L781)

再往下一层的桥接函数：

- `cSegmentImpl.Insert(...)`
  [segment.go#L242](/root/xty/milvus/internal/util/segcore/segment.go#L242)

这里会做：

1. `preInsert()` 让 C++ segment 预留 offset
2. 把 `InsertRecord` 序列化成 protobuf blob
3. 调 `C.Insert(...)` 真正写进 C++ segment

##### 这一步在 C++ 代码里大概看哪里

最值得先看的目录是：

- `internal/core/src/segcore/`

尤其是这些文件：

- `SegmentGrowingImpl.cpp`
- `InsertRecord.h`
- `SegmentInterface.h`
- `Collection.cpp`

一句话概括这一步：

```text
系统级 insert 是 Go 在编排；
segment 内真正的“把列式数据写进执行引擎”这一步，是 C++ segcore 在做。
```

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

#### Flush 到对象存储后，文件会长什么样

假设 `segment 7001` 被 flush，那么对象存储里会看到类似路径：

```text
insert_log/3001/5001/7001/100/910001   -> id 列
insert_log/3001/5001/7001/101/910002   -> title 列
insert_log/3001/5001/7001/102/910003   -> price 列
insert_log/3001/5001/7001/103/910004   -> embedding 列
stats_log/3001/5001/7001/100/920001    -> 主键统计
stats_log/3001/5001/7001/103/920002    -> 向量字段统计
```

这些文件里的内容仍然是**按字段分开的**，例如：

```text
Field 100 (id) binlog:
  [101, 103]

Field 101 (title) binlog:
  ["red mug", "green tea"]

Field 102 (price) binlog:
  [19.8, 9.9]

Field 103 (embedding) binlog:
  [
    0.10, 0.20, 0.30, 0.40,
    0.12, 0.18, 0.33, 0.39
  ]

Stats binlog:
  minPK = 101
  maxPK = 103
  rowCount = 2
```

也就是说，**落盘以后不是一个 JSON 文件一行一条记录**，而是：

- 一个 Segment
- 里面每个字段各有自己的 binlog 文件
- QueryNode 加载时再把这些字段重新拼成可查询的段

### 4.8 Step 7: 索引构建

Flush 完成后触发索引构建：

1. DataCoord 通过 `notifyIndexChan` 通知索引检查器
2. `indexInspector` 监控已 Flush 的段
3. 通过 `globalScheduler` 调度索引构建任务到 DataNode
4. DataNode 的 `index/TaskScheduler` 执行索引构建

**索引构建位置**: `internal/datanode/index/`

#### 先记住一个核心结论

**索引不是从 WAL 直接建的，也不是从业务 JSON 直接建的。**

当前链路里，索引构建的输入主要是：

- Flush 后对象存储里的 **字段 binlog**
- Collection 上定义好的 **索引元数据**
- Segment 自身的 **行数、字段类型、维度、存储版本**

也就是说，索引构建更像：

```text
Segment 7001 已 Flush
  ├─ insert_log/.../field 103  -> 原始向量列
  ├─ insert_log/.../field 102  -> 可选标量列（某些索引可能会带上）
  └─ collection index meta     -> index_type=HNSW, metric_type=L2, M=16 ...

DataNode 读取这些输入
  -> 调 Knowhere / segcore build
  -> 生成 index 文件
  -> 上传到 object storage
```

#### 继续复用同一个例子

假设前面 `segment 7001` 已经 flush，里面有两行数据：

```text
id        = [101, 103]
title     = ["red mug", "green tea"]
price     = [19.8, 9.9]
embedding = [
  0.10, 0.20, 0.30, 0.40,
  0.12, 0.18, 0.33, 0.39
]
```

Collection 上事先已经创建了一个向量索引定义：

```text
indexName   = "idx_embedding_hnsw"
field       = "embedding" (fieldID = 103)
indexType   = "HNSW"
metricType  = "L2"
indexParams = {"M":"16", "efConstruction":"200"}
```

那么 DataCoord 会为这个 `(segment=7001, index=idx_embedding_hnsw)` 组合创建一条**段级索引任务**。

#### Step 7.1 DataCoord 先决定“这个段需不需要建索引”

关键位置：

- `internal/datacoord/index_inspector.go`
- `internal/datacoord/task_index.go`

`indexInspector.createIndexForSegment()` 负责：

1. 给这次构建分配一个全局唯一 `BuildID`
2. 找到这个 collection 上对应的 `IndexID / FieldID / IndexParams`
3. 判断字段大小，估算这次构建要占多少 task slot
4. 创建 `SegmentIndex` 元数据
5. 把任务塞进全局调度器

这时元数据里大致会出现一条记录：

```text
SegmentIndex
  SegmentID    = 7001
  CollectionID = 3001
  PartitionID  = 5001
  IndexID      = 9001
  BuildID      = 88001
  IndexType    = "HNSW"
  NumRows      = 2
  State        = Init
```

这里一个很重要的点是：

- **Collection Index** 是“逻辑定义”
- **SegmentIndex** 是“某个具体段的一次实际构建任务”

所以“给 collection 建了一个索引”在底层会展开成很多条 segment 级任务。

#### Step 7.2 DataCoord 组织成 CreateJobRequest 发给 DataNode

`task_index.go` 里的 `prepareJobRequest()` 会把构建索引所需的输入凑齐。

一个典型的 `CreateJobRequest` 可以近似理解成：

```text
CreateJobRequest
  BuildID        = 88001
  CollectionID   = 3001
  PartitionID    = 5001
  SegmentID      = 7001
  FieldID        = 103
  FieldName      = "embedding"
  FieldType      = FloatVector
  NumRows        = 2
  Dim            = 4
  IndexParams    = {"index_type":"HNSW", "metric_type":"L2", "M":"16", "efConstruction":"200"}
  TypeParams     = {"dim":"4"}
  IndexFilePrefix = "{rootPath}/index"
  DataIds / DataPaths
    -> 指向 field 103 的 insert binlog
  OptionalScalarFields
    -> 某些索引/物化视图场景下附带的标量字段 binlog
  StorageConfig
    -> 告诉 DataNode 去哪个对象存储读原始数据、往哪里写索引文件
```

最关键的是这几个输入：

- `DataIds/DataPaths`: 告诉 DataNode 去哪里拿这个字段的原始列数据
- `Field/Dim/NumRows`: 告诉 DataNode 这列数据应该怎么解释
- `IndexParams`: 告诉 DataNode 要建什么索引
- `IndexFilePrefix`: 告诉 DataNode 产物要写到哪里

#### Step 7.3 DataNode 并不是凭空建索引，而是“读原始列数据再 build”

关键位置：

- `internal/datanode/index/scheduler.go`
- `internal/datanode/index/task_index.go`

`TaskScheduler` 做的是调度：

- 从队列里取索引任务
- 跑 `PreExecute -> Execute -> PostExecute`
- 向量索引任务通常进专门的 build pool

真正的 build 发生在 `indexBuildTask.Execute()` 里。它会：

1. 把 `CreateJobRequest` 转成 `BuildIndexInfo`
2. 把 `InsertFiles`、`FieldSchema`、`IndexParams`、`StorageConfig` 传给 `indexcgowrapper.CreateIndex`
3. 由底层 Knowhere / segcore 读取原始 binlog 并执行构建

可以把这一步理解成：

```text
DataNode Execute
  输入:
    embedding 原始列数据文件
    schema(field=embedding, dim=4)
    index params(HNSW, L2, M=16, efConstruction=200)

  调底层库 build 后得到:
    内存里的索引对象 CodecIndex
```

这里再强调一次：

- **索引构建依赖原始向量列数据**
- 建完索引后，原始 binlog 也不会消失

因为 QueryNode 搜索时并不是只靠索引文件：

- 标量过滤要用原始字段数据
- 输出字段返回要用原始字段数据
- 某些搜索/校验流程仍会依赖原始数据

#### Step 7.4 DataNode PostExecute 把索引产物上传到对象存储

`indexBuildTask.PostExecute()` 做两件大事：

1. `it.index.UpLoad()` 把索引对象序列化成文件
2. 记录 `fileKeys / serializedSize / memSize / version` 等信息

上传后的产物会进入类似路径：

```text
index/{buildID}/{indexVersion}/{partitionID}/{segmentID}/{fileKey}
```

例如：

```text
index/88001/1/5001/7001/hnsw_meta
index/88001/1/5001/7001/hnsw_graph
index/88001/1/5001/7001/hnsw_data
```

文件名会随具体索引类型不同而不同，但你可以把它理解为：

- 一个 BuildID 对应一批索引文件
- 这些文件共同描述了 `segment 7001` 在 `embedding` 字段上的索引

DataNode 这时会把执行结果先存在自己的任务管理器里，包含：

```text
IndexTaskInfo
  State          = Finished
  FileKeys       = [...]
  SerializedSize = ...
  MemSize        = ...
```

#### Step 7.5 DataCoord 轮询结果并把索引元数据“正式写回”

DataCoord 不会在发出任务后就假设任务成功，而是会继续：

1. `QueryIndex` 到对应 worker
2. 看到状态是 `Finished / Failed / Retry`
3. `FinishTask(result)` 把 file keys、大小、状态、失败原因等写回 index meta

所以一个索引任务完整闭环是：

```text
DataCoord create task
  -> DataNode build
  -> DataNode upload index files
  -> DataCoord query worker result
  -> DataCoord finish meta
```

直到这一步，系统才真正知道：

- 这个 segment 的这个 field 已经有可用索引
- 索引文件在 object storage 的哪些 path
- 当前索引版本是多少

#### Step 7.6 为什么有些段看起来“没建索引”

这不是一定失败，也可能是系统故意这么做。

`task_index.go` 里有两个典型分支：

1. **段太小**
2. **某些 no-train index / 特殊索引类型**

这时任务可能直接被标成“fake finished”或者跳过，因为：

- 小段建索引收益很小
- 构建成本可能高于搜索收益

所以“collection 建了索引”不一定意味着“每一个 segment 都真的生成了一套重型索引文件”。

#### Step 7.7 QueryNode 最后怎么把索引用起来

关键位置：

- `internal/querynodev2/segments/segment_loader.go`
- `internal/querynodev2/segments/segment.go`

当 QueryCoord / QueryNode 要加载一个 Sealed Segment 时，会在 `SegmentLoadInfo` 里带上 `FieldIndexInfo`，里面有：

```text
FieldIndexInfo
  FieldID         = 103
  IndexID         = 9001
  BuildID         = 88001
  IndexFilePaths  = [...]
  IndexParams     = ...
  IndexSize       = ...
  NumRows         = 2
```

随后 QueryNode 会：

1. 根据 `IndexFilePaths` 去对象存储或 DiskCache 找索引文件
2. 构造 `LoadIndexInfo`
3. 调底层 C 接口把索引 append 到 segment
4. 更新该 segment 的本地索引信息

这一步之后，这个 Sealed Segment 才能真正以“有索引”的方式参与搜索。

#### Step 7.8 把索引链路和前面插入链路连起来

用一句最直白的话概括就是：

```text
Insert -> WAL -> Growing Segment -> Flush 生成字段 binlog
      -> DataCoord 发现 segment 已 flush
      -> 为每个需要建索引的 field 创建 BuildID
      -> DataNode 读取该 field 的原始 binlog 构建索引
      -> 上传 index files 到 object storage
      -> DataCoord 回写 FieldIndexInfo / SegmentIndex 状态
      -> QueryNode 后续加载 segment 时把 index files 一起加载
```

所以你刚才问“这才是关键部分吗”，答案是：

- **写入链路** 解决的是“数据先活下来并可见”
- **索引构建链路** 解决的是“后续搜索能不能快”

对向量数据库来说，这一段确实非常关键，因为它直接决定：

- 搜索延迟
- 内存占用
- 索引文件大小
- 小段/大段的构建策略
- QueryNode 最终加载什么资源

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

#### 继续复用上面的 3 条数据

假设刚才那批数据已经写入成功，并且当前系统里的段分布是：

```text
channel 0 -> segment 7001 -> rows: id [101, 103]
channel 1 -> segment 7002 -> rows: id [102]
```

下面用两个最常见的读取动作来说明：

1. **向量 Search**: 给一个查询向量，找最相似的 TopK
2. **标量 Query / Retrieve**: 不做向量相似度，只按条件把字段值拿回来

#### 先看客户端 Search 请求长什么样

业务上通常会写成：

```json
{
  "db_name": "demo",
  "collection_name": "product_catalog",
  "partition_names": ["p1"],
  "vectors": [[0.11, 0.19, 0.31, 0.41]],
  "dsl": "price > 10",
  "search_params": [
    {"key": "anns_field",  "value": "embedding"},
    {"key": "metric_type", "value": "L2"},
    {"key": "topk",        "value": "2"},
    {"key": "params",      "value": "{\"nprobe\": 16}"}
  ],
  "output_fields": ["id", "title", "price"],
  "nq": 1
}
```

这个请求的意思是：

- 用 1 个查询向量 `q0 = [0.11, 0.19, 0.31, 0.41]`
- 在 `embedding` 字段上做相似度搜索
- 只看 `price > 10` 的数据
- 返回最相似的前 2 条
- 同时把 `id/title/price` 这些字段一起带回来

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

#### Search 请求进入 Proxy 后，内部数据形态是什么

内部 `internalpb.SearchRequest` 里，最重要的是这几个字段：

```text
collectionID         = 3001
partitionIDs         = [5001]
nq                   = 1
topk                 = 2
metricType           = "L2"
output_fields_id     = [100, 101, 102]
placeholder_group    = bytes(...)
serialized_expr_plan = bytes(...)
```

其中最容易看不懂的是 `placeholder_group`。如果把它解码成人能看懂的形式，大概是：

```text
PlaceholderGroup
  placeholders = [
    {
      tag: "$0",
      type: FloatVector,
      values: [
        bytes([0.11, 0.19, 0.31, 0.41])
      ]
    }
  ]
```

也就是说：

- 外部请求里你看到的是 `vectors: [[...]]`
- 内部真正传给执行层时，是一个二进制 `PlaceholderGroup`

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

**示例**: `search(q0, filter="price > 10", topK=2, anns_field="embedding")`
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

#### 和 Insert 最不一样的一点：Search 会扇出到所有相关分片

插入时是：

- 第 1 行去 channel 0
- 第 2 行去 channel 1
- 第 3 行去 channel 0

但搜索时不是“查询向量只去一个 channel”，而是**同一个查询向量会 fan-out 到所有相关 channel**：

```text
q0 = [0.11, 0.19, 0.31, 0.41]

Proxy fan-out:
  q0 + 同一份 Plan -> channel 0 -> QueryNode-A
  q0 + 同一份 Plan -> channel 1 -> QueryNode-B
```

所以可以这么对比理解：

- **Insert**: 一行只进入一个 shard
- **Search**: 一条查询会广播到所有相关 shard，再做归并

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

#### 刚插入完立刻查，和 Flush 之后再查，命中的段类型可能不同

同一批数据会出现在两种不同位置：

- **刚插入后立即查**: 通常命中 `Growing Segments`，走 `SearchStreaming`
- **Flush + Load 完成后再查**: 通常命中 `Sealed Segments`，走 `SearchHistorical`

但是对用户来说，返回的业务结果应该保持一致；差别主要在内部数据来源不同。

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

#### 5.7.1 QueryNode 到 C++ 的调用链

查询链路里，Go 和 C++ 的分工比插入更清楚：

- Go：负责路由、收集、归并、调度
- C++：负责 segment 内真正搜索/过滤/检索

调用链大致是：

```text
Proxy.Search
  -> QueryNode.Search
    -> SearchHistorical / SearchStreaming
      -> searchSegments
        -> LocalSegment.Search
          -> s.csegment.Search
            -> C.AsyncSearch(...)
              -> C++ query / segcore 执行
```

对应代码：

- QueryNode 按段并发搜索：
  [search.go#L36](/root/xty/milvus/internal/querynodev2/segments/search.go#L36)
- Go segment 封装：
  [segment.go#L632](/root/xty/milvus/internal/querynodev2/segments/segment.go#L632)
- CGO 桥接：
  [segment.go#L134](/root/xty/milvus/internal/util/segcore/segment.go#L134)

普通 `Query/Retrieve` 也是类似路径：

```text
Proxy.Query
  -> QueryNode.Query
    -> segments.Retrieve
      -> LocalSegment.Retrieve
        -> s.csegment.Retrieve
          -> C.AsyncRetrieve(...)
```

对应代码：

- Go retrieve 封装：
  [segment.go#L687](/root/xty/milvus/internal/querynodev2/segments/segment.go#L687)
- CGO retrieve：
  [segment.go#L172](/root/xty/milvus/internal/util/segcore/segment.go#L172)

#### 5.7.2 C++ 源码地图：查询到底看哪几个目录

如果你想直接追到 C++ 实现，最有价值的是下面几个目录：

##### A. `internal/core/src/segcore/`

职责：

- Segment 的核心数据结构
- Growing / Sealed Segment 实现
- 字段数据加载
- 索引加载
- segment 级操作入口

建议先看：

- `SegmentGrowingImpl.cpp`
- `ChunkedSegmentSealedImpl.cpp`
- `SegmentInterface.cpp`
- `SegmentLoadInfo.cpp`
- `InsertRecord.h`

可以理解成：

```text
segcore = “segment 作为执行对象”的核心实现
```

##### B. `internal/core/src/query/`

职责：

- Search / Retrieve / brute force / index search 的主要查询逻辑
- 查询计划执行
- search on growing / search on sealed

建议先看：

- `SearchOnGrowing.cpp`
- `SearchOnSealed.cpp`
- `SearchOnIndex.cpp`
- `SearchBruteForce.cpp`
- `Plan.cpp`
- `PlanProto.cpp`

可以理解成：

```text
query = “给定 plan 和 segment，具体怎么搜”
```

##### C. `internal/core/src/exec/`

职责：

- 标量表达式执行
- 过滤算子
- 向量搜索算子
- aggregation / order by / project 等执行节点

建议先看：

- `operator/VectorSearchNode.cpp`
- `operator/FilterBitsNode.cpp`
- `operator/QueryOrderByNode.cpp`
- `expression/CompareExpr.cpp`
- `expression/ConjunctExpr.cpp`
- `expression/TermExpr.cpp`

可以理解成：

```text
exec = “过滤表达式和算子执行引擎”
```

##### D. `internal/core/src/index/`

职责：

- 索引结构本身
- 向量索引 / 标量索引 / 倒排索引等实现

建议先看：

- `VectorMemIndex.cpp`
- `VectorDiskIndex.cpp`
- `ScalarIndex.cpp`
- `InvertedIndexTantivy.cpp`
- `IndexFactory.cpp`

##### E. `internal/core/src/indexbuilder/`

职责：

- 真正 build 索引时的底层逻辑

建议先看：

- `VecIndexCreator.cpp`
- `ScalarIndexCreator.cpp`
- `index_c.cpp`

#### 5.7.3 示例：一条查询在 C++ 里如何执行

继续复用前面的例子：

```text
segment 7001
  id        = [101, 103]
  price     = [19.8, 9.9]
  embedding = [
    [0.10, 0.20, 0.30, 0.40],
    [0.12, 0.18, 0.33, 0.39]
  ]
```

用户查询：

```text
query vector = [0.11, 0.19, 0.31, 0.41]
filter       = "price > 10"
topK         = 2
metricType   = L2
searchField  = embedding
```

##### Go 层看到的大致内容

```text
SearchRequest
  placeholder_group    = bytes(query vector)
  serialized_expr_plan = bytes(plan: VectorANNS + price > 10)
  collectionID         = 3001
  segmentID            = 7001
```

##### 进入 C++ 前

Go 桥接层会把这些东西转换成：

- `cSearchPlan`
- `cPlaceholderGroup`
- `mvccTimestamp`
- `consistencyLevel`

然后调用：

```text
C.AsyncSearch(segment_ptr, search_plan, placeholder_group, ...)
```

##### C++ 层大致会做什么

你可以近似理解成下面这几步：

1. **解析查询计划**
   - 向量字段是 `embedding`
   - 度量方式是 `L2`
   - 标量谓词是 `price > 10`

2. **选择搜索路径**
   - 如果 segment 上 `embedding` 已加载索引，就走 `SearchOnIndex`
   - 如果没有索引，可能走 `SearchBruteForce`
   - 如果是 growing segment，常见是 `SearchOnGrowing`

3. **先做候选向量搜索**
   - 在 `embedding` 上找最相近候选

4. **做标量过滤**
   - 检查候选对应的 `price`
   - 过滤掉 `price <= 10` 的记录

5. **返回 segment 级结果**
   - `ids`
   - `scores`
   - `validCount`
   - 供 Go 层后续 reduce

##### 用本例结果表示

假设：

```text
id=101, distance=0.0004, price=19.8
id=103, distance=0.0022, price=9.9
```

那么 C++ 层 segment 级结果会近似变成：

```text
before filter:
  [101, 103]
  [0.0004, 0.0022]

after "price > 10":
  [101]
  [0.0004]
```

然后 Go 层再把多个 segment、多个 channel 的结果继续归并。

##### 一句话理解这段

```text
Go 负责“把查询组织好并发下去”；
C++ 负责“在某个具体 segment 上把相似度搜索和过滤真的跑出来”。
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

#### 用刚才那条查询，看看各层结果长什么样

查询条件：

- 查询向量: `q0 = [0.11, 0.19, 0.31, 0.41]`
- 过滤条件: `price > 10`
- `topK = 2`

那么各个 shard 可能返回：

```text
QueryNode-A / segment 7001
  原始候选:
    id=101, score=0.0004
    id=103, score=0.0022
  过滤后(price > 10):
    id=101, score=0.0004

QueryNode-B / segment 7002
  原始候选:
    id=102, score=0.1620
  过滤后(price > 10):
    id=102, score=0.1620
```

QueryNode 内部归并后，给 Proxy 的 `SearchResultData` 可以近似理解成：

```text
SearchResultData
  num_queries = 1
  topks       = [2]
  ids         = [101, 102]
  scores      = [0.0004, 0.1620]
  fields_data:
    title = ["red mug", "blue bottle"]
    price = [19.8, 29.9]
```

最后客户端拿到的结果可以理解成：

```json
{
  "results": [
    {"id": 101, "score": 0.0004, "title": "red mug", "price": 19.8},
    {"id": 102, "score": 0.1620, "title": "blue bottle", "price": 29.9}
  ]
}
```

如果度量类型是 `L2`，一般是**分数越小越相似**；如果是 `IP` / `COSINE`，解释方式会不同。

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

#### Query 请求和返回长什么样

如果现在不是做向量 Search，而是做普通 Query：

```json
{
  "db_name": "demo",
  "collection_name": "product_catalog",
  "expr": "id in [101, 103]",
  "output_fields": ["id", "title", "price"]
}
```

那么内部会更接近：

```text
RetrieveRequest
  collectionID         = 3001
  partitionIDs         = [5001]
  serialized_expr_plan = bytes(plan for "id in [101, 103]")
  output_fields_id     = [100, 101, 102]
  limit                = 2
```

返回结果不再有向量分数，而是直接把字段值取回来：

```text
RetrieveResults
  ids = [101, 103]
  fields_data:
    id    = [101, 103]
    title = ["red mug", "green tea"]
    price = [19.8, 9.9]
```

所以要把这两种结果分清：

- `SearchResults`: 重点是 `ids + score + 可选输出字段`
- `RetrieveResults`: 重点是 `ids + fields_data`，没有相似度分数

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

### 5.11 同一批数据在各层的形态变化总表

下面这张表把“同一批数据”从写入到查询的形态连起来：

| 阶段 | 结构 | 本例中长什么样 |
|------|------|----------------|
| 业务侧 | 行式对象 | `{"id":101,"title":"red mug","price":19.8,"embedding":[...]}` |
| Insert 请求 | `milvuspb.InsertRequest` | `num_rows=3`, `fields_data` 是 4 列 |
| Proxy 内部 | `msgstream.InsertMsg` | 额外带上 `RowIDs=[90001,90002,90003]` |
| 分片后 | 多个 `InsertMsg` | channel 0 保存 `[101,103]`，channel 1 保存 `[102]` |
| WAL | `MutableMessage(Header + Body)` | header 记录 collection/partition，body 记录真正字段数据 |
| Growing 段 | `InsertRecord` | 仍然是列式字段数组，但已经按 segment 分开缓存 |
| Flush 后 | `insert_log/stats_log/...` | 每个字段一个 binlog 文件，不是整行 JSON |
| Search 请求 | `internalpb.SearchRequest` | 查询向量被编码到 `placeholder_group` |
| Query 请求 | `internalpb.RetrieveRequest` | 没有查询向量，只有表达式 plan 和输出字段 |
| QueryNode 段结果 | `SearchResultData` / `RetrieveResults` | 先是段级局部结果，再节点级归并 |
| Proxy 最终结果 | `milvuspb.SearchResults` / Query 返回 | Search 给 `ids + score`，Query 给 `ids + fields` |

如果只记一句话，可以记这个：

- **写入**: 行式业务数据 -> 列式字段数据 -> 按行分片 -> 按字段落盘
- **查询**: 查询向量/表达式 -> 广播到各 shard -> 段级执行 -> 分层归并 -> 返回业务结果

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
