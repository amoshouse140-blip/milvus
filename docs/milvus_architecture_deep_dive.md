# Milvus 架构深度解析

## 目录

- [一、核心概念](#一核心概念)
- [二、系统架构](#二系统架构)
- [三、数据与存储](#三数据与存储)
- [四、插入端到端流程（含索引构建）](#四插入端到端流程含索引构建)
- [五、搜索端到端流程](#五搜索端到端流程)
- [六、HNSW 算法原理](#六hnsw-算法原理)

---

## 一、核心概念

后续章节会反复用到以下术语，先统一定义：

| 概念 | 说明 |
|------|------|
| **Collection** | 类似关系数据库的"表"，包含 Schema 定义（字段名、类型、维度等） |
| **Partition** | Collection 内的逻辑分区，数据按 Partition Key 或手动指定分布 |
| **Field** | Collection 内的一列，有唯一 FieldID。向量字段存向量，标量字段存普通数据 |
| **Segment** | 数据的物理存储单元。一个 Partition 下有多个 Segment，每个 Segment 存一批行 |
| **VChannel** | 虚拟通道。每个 Collection 有 N 个 VChannel，Insert 时按主键 Hash 路由到某个 VChannel |
| **WAL** | Write-Ahead Log，写入先落 WAL 再异步持久化，保证数据不丢 |
| **Binlog** | Segment 的列式持久化文件，每个字段单独一个文件 |
| **Index** | 向量索引（如 HNSW），加速近似最近邻搜索 |
| **Sealed Segment** | 已封存不再写入的段，已 Flush 到对象存储，可建索引 |
| **Growing Segment** | 正在接收写入的段，数据在内存中，搜索走暴力扫描 |
| **L0 Segment** | 仅存储删除记录（PK + Timestamp），搜索时用于过滤已删除行 |
| **BuildID** | 给某个 Segment 构建某个 Index 的一次任务编号，不等于 IndexID |
| **IndexID** | Collection 级别的索引定义 ID（"这个字段要建 HNSW 索引"） |
| **Delegator** | QueryNode 中负责 Shard 级搜索编排的组件，协调 Growing + Sealed 段的搜索 |
| **segcore** | C++ 核心执行层，负责段内向量搜索、标量过滤、数据插入等底层计算 |
| **Knowhere** | 向量索引库，封装了 HNSW、IVF、DiskANN 等索引算法的构建和搜索 |
| **PlaceholderGroup** | 搜索时查询向量的二进制编码格式，Proxy 编码后传给 QueryNode |

### Segment 生命周期

```
                    写入数据
                      │
                      ▼
              ┌──────────────┐
              │   Growing    │ ← 内存中，接收新写入
              │   (可写)      │   搜索走暴力扫描
              └──────┬───────┘
                     │ 达到行数/时间/大小阈值
                     ▼
              ┌──────────────┐
              │   Sealed     │ ← 不再接受写入
              │   (封存)      │   等待 Flush
              └──────┬───────┘
                     │ Flush
                     ▼
              ┌──────────────┐
              │   Flushed    │ ← 数据已落对象存储
              │   (已持久化)  │   可触发索引构建
              └──────┬───────┘
                     │ 索引构建完成 + QueryNode 加载
                     ▼
              ┌──────────────┐
              │  可查询      │ ← Sealed Segment + 索引
              │  (SearchOnIndex) │  搜索走向量索引
              └──────────────┘
```

### Segment 层级

```
┌──────────────────────────────────────────────┐
│  L0 — 仅存删除记录 (PK + Timestamp)          │
│       始终在内存，搜索时过滤已删除行           │
├──────────────────────────────────────────────┤
│  L1 — Flush 产出的标准段                      │
│       包含 insert binlog + stats + delta      │
├──────────────────────────────────────────────┤
│  L2 — Compaction 产出的大段                   │
│       删除已合并，数据更紧凑                   │
└──────────────────────────────────────────────┘
```

---

## 二、系统架构

Milvus 采用**存算分离**架构：Go 负责分布式协调和业务编排，C++ 负责段内向量执行，Rust 用于部分索引集成（tantivy）。

### 2.1 组件总图

```
                          ┌──────────────────────┐
                          │    Client SDK        │
                          │  Python/Java/Go/REST │
                          └──────────┬───────────┘
                                     │ gRPC / HTTP
                          ┌──────────▼───────────┐
                          │       Proxy          │
                          │  请求入口 · 路由 · 归并 │
                          └──┬─────┬─────┬───────┘
                             │     │     │
              ┌──────────────┘     │     └──────────────┐
              │                    │                     │
    ┌─────────▼─────────┐  ┌──────▼──────┐    ┌────────▼────────┐
    │     MixCoord      │  │  QueryNode  │    │  StreamingNode  │
    │   (统一协调器)      │  │  (查询执行)  │    │  (WAL + 写缓冲)  │
    │                   │  │             │    │                 │
    │ ┌───────────────┐ │  │  Go 编排    │    │  接收 Insert    │
    │ │  RootCoord    │ │  │     +       │    │  WAL 持久化     │
    │ │  DDL · 元数据  │ │  │  C++ segcore│    │  Growing 段管理 │
    │ ├───────────────┤ │  │  向量搜索    │    │  触发 Flush     │
    │ │  DataCoord    │ │  │  标量过滤    │    └────────┬────────┘
    │ │  Segment 管理  │ │  └──────┬──────┘             │
    │ │  索引调度      │ │         │                     │
    │ ├───────────────┤ │         │                     │
    │ │  QueryCoord   │ │         │                     │
    │ │  加载/均衡     │ │         │                     │
    │ ├───────────────┤ │         │                     │
    │ │StreamingCoord │ │         │                     │
    │ │  WAL 协调     │ │         │                     │
    │ └───────────────┘ │         │                     │
    └─────────┬─────────┘         │                     │
              │                   │                     │
              │          ┌────────▼────────┐            │
              │          │    DataNode     │            │
              │          │  索引构建       │            │
              │          │  Compaction     │            │
              │          └────────┬────────┘            │
              │                   │                     │
    ┌─────────▼───────────────────▼─────────────────────▼───┐
    │                    外部依赖层                           │
    │                                                       │
    │   etcd / TiKV          MinIO / S3          WAL 后端   │
    │   (元数据)             (数据文件)          (消息持久化) │
    └───────────────────────────────────────────────────────┘
```

### 2.2 各组件职责

| 组件 | 做什么 | 不做什么 |
|------|--------|---------|
| **Proxy** | 接收用户请求 · 校验参数 · 分配主键 · Hash 分片路由 · 写 WAL · 发搜索请求到 QueryNode · 归并结果返回 | 不存数据，不执行向量计算 |
| **MixCoord** | 统一协调器容器，内含四个子协调器 | 不直接处理用户读写请求 |
| ├ **RootCoord** | DDL（建库/建表/建分区）· 时间戳分配 · ID 分配 · Schema 管理 | — |
| ├ **DataCoord** | Segment 生命周期管理 · 封存/Flush 调度 · 索引构建任务编排 · Compaction 调度 | 不执行索引构建本身 |
| ├ **QueryCoord** | Collection 加载/释放 · Segment 到 QueryNode 的分配 · 副本均衡 | 不执行搜索 |
| └ **StreamingCoord** | WAL 的协调管理 | — |
| **QueryNode** | 持有 Sealed/Growing 段 · 执行向量搜索(C++ segcore) · 标量过滤 · 段级结果归并 | 不负责数据持久化 |
| **StreamingNode** | 持有 WAL · 接收 Proxy 写入 · Growing Segment 写缓冲 · 驱动 Flush 到对象存储 | 不执行搜索 |
| **DataNode** | 索引构建执行 · Compaction 执行 · Import 执行 | 不负责实时写入路径 |

### 2.3 数据流概览

```
         ┌─────── 写入流 ──────────────────────────────────────┐
         │                                                     │
  Client ──Insert──► Proxy ──WAL──► StreamingNode ──Flush──► 对象存储
                                         │                     │
                                    QueryNode 同时              │
                                    消费 WAL 回放           DataCoord
                                    到 Growing 段           检测到 Flushed
                                    (实时可查)                   │
                                                          DataNode
                                                          建索引上传
                                                               │
                                                          QueryNode
                                                          加载索引
                                                          (高速可查)
         └─────────────────────────────────────────────────────┘

         ┌─────── 查询流 ──────────────────────────────────────┐
         │                                                     │
  Client ──Search──► Proxy ──广播──► QueryNode-A ──► 段级搜索  │
                       │                   │                   │
                       │             QueryNode-B ──► 段级搜索  │
                       │                   │                   │
                       └──── 归并 TopK ◄───┘                   │
         └─────────────────────────────────────────────────────┘
```

**写入 vs 查询的关键区别**：

| 维度 | Insert | Search |
|------|--------|--------|
| 路由 | 一行只进一个 shard (Hash PK) | 一条查询广播到所有 shard |
| 数据方向 | Client → Proxy → WAL → Segment → 对象存储 | Client → Proxy → QueryNode → C++ → 归并返回 |
| 返回时机 | WAL 写入成功即返回（异步持久化） | 所有 shard 搜索完成并归并后返回 |

---

## 三、数据与存储

### 3.1 三层存储全景

```
┌───────────────────────────────────────────────────────────────────┐
│                                                                   │
│  ┌─── 元数据层 (etcd / TiKV) ─────────────────────────────────┐  │
│  │                                                             │  │
│  │  Collection Schema   字段名/类型/维度                       │  │
│  │  Index 定义          哪个字段建什么索引                      │  │
│  │  Segment Meta        段状态/行数/binlog路径                  │  │
│  │  SegmentIndex        段级索引构建任务与产物                   │  │
│  │  Session / RBAC      服务注册/权限                           │  │
│  │                                                             │  │
│  └─────────────────────────────────────────────────────────────┘  │
│                                                                   │
│  ┌─── 对象存储层 (MinIO / S3) ─────────────────────────────────┐  │
│  │                                                             │  │
│  │  insert_log/   字段列式 binlog（每字段一组文件）              │  │
│  │  stats_log/    统计信息（min/max PK, rowCount）              │  │
│  │  delta_log/    删除日志（PK + Timestamp）                    │  │
│  │  index/        索引文件（HNSW graph、IVF lists 等）          │  │
│  │                                                             │  │
│  └─────────────────────────────────────────────────────────────┘  │
│                                                                   │
│  ┌─── WAL 层 (StreamingNode / Pulsar / Kafka) ─────────────────┐  │
│  │                                                             │  │
│  │  Insert / Delete / CreateSegment / Flush 等消息              │  │
│  │  按 VChannel 分区，保序                                     │  │
│  │                                                             │  │
│  └─────────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────────┘
```

**关键点**：元数据（Schema、索引定义、段状态）在 etcd，实际数据文件（binlog、索引文件）在对象存储。两者分开存放。

### 3.2 元数据：存了什么

四类核心元数据，以及它们之间的依赖关系：

```
  CreateCollection
        │
        ▼
  Collection Schema ──────────────────┐
  (字段定义/类型/维度)                 │
        │                             │
        │ Insert 触发                  │
        ▼                             │
  Segment Meta ─────────────┐         │
  (段ID/状态/行数/binlog路径)│         │
        │                   │         │
        │ Flush 完成        │         │
        ▼                   │         │
  Segment Meta (Flushed)    │         │
        │                   │         │
        │ CreateIndex       │         │
        │       ▼           │         │
        │ Index 定义 ───────┤         │
        │ (字段/索引类型/参数)│         │
        │                   ▼         ▼
        │          SegmentIndex ◄── 三者组合
        │          (BuildID/状态/索引文件路径)
        │               │
        │          DataNode 构建
        │               ▼
        │          索引文件 (对象存储)
        │               │
        │          QueryNode 加载
        ▼               ▼
     段可查询 ◄─────────┘
```

**具体示例**（后续章节复用）：

```
Collection: 3001            Index 定义:
  Fields:                     CollectionID = 3001
    100 id       Int64        FieldID      = 103
    101 title    VarChar      IndexID      = 9001
    102 price    Float        IndexType    = HNSW
    103 embedding FloatVector  Params       = {L2, M=16, ef=200}
        dim=4

Segment Meta:               SegmentIndex:
  ID    = 7001                SegmentID = 7001
  State = Flushed             IndexID   = 9001
  Rows  = 2                   BuildID   = 88001
  Binlogs = [...]             State     = Finished
                               FileKeys  = [hnsw_meta, ...]
```

**IndexID vs BuildID**：

```
Index 9001 = "embedding 字段要建 HNSW 索引"      ← Collection 级定义
  ├─ Segment 7001 → Build 88001                  ← 段级构建任务
  ├─ Segment 7002 → Build 88002
  └─ Segment 7015 → Build 88015
```

### 3.3 对象存储：一个 Segment 的文件长什么样

以 `Segment 7001`（2 行数据，4 个字段）为例：

```
insert_log/3001/5001/7001/          ← collectionID/partitionID/segmentID
  ├── 100/910001   → id 列:        [101, 103]
  ├── 101/910002   → title 列:     ["red mug", "green tea"]
  ├── 102/910003   → price 列:     [19.8, 9.9]
  └── 103/910004   → embedding 列: [0.10,0.20,0.30,0.40, 0.12,0.18,0.33,0.39]
                                     ↑ 连续 float 数组，按 dim=4 解释

stats_log/3001/5001/7001/
  └── 100/920001   → 主键统计: minPK=101, maxPK=103, rowCount=2

delta_log/3001/5001/7001/           ← 删除日志（如果有删除）
  └── 930001       → {pk=101, ts=T5} 表示 101 在 T5 被删除

index/88001/1/5001/7001/            ← buildID/indexVersion/partitionID/segmentID
  ├── hnsw_meta    → HNSW 元信息
  ├── hnsw_graph   → HNSW 图结构
  └── hnsw_data    → HNSW 向量数据
```

**关键：每个字段各自一个 binlog 文件，不是一个 JSON 文件一行一条记录。** 这是列式存储。

路径规则定义在 `pkg/util/metautil/binlog.go`。

### 3.4 Binlog 文件内部结构

```
┌──────────────────────────────┐
│ Magic Number (0xfffabc)      │
├──────────────────────────────┤
│ Descriptor Event             │
│   Timestamp · TypeCode       │
├──────────────────────────────┤
│ Data Event (Insert/Delete)   │
│   payload = Arrow/Parquet    │
│   列式编码的字段数据          │
├──────────────────────────────┤
│ ...更多 Event...             │
└──────────────────────────────┘
```

实现：`internal/storage/binlog_writer.go`，数据序列化使用 Arrow/Parquet 格式。

### 3.5 向量在内存中的形态

```
┌────────── Growing Segment (内存) ──────────┐     ┌────────── Sealed Segment (加载后) ──────┐
│                                             │     │                                        │
│  InsertRecord (C++ segcore)                 │     │  原始字段数据                            │
│  ├─ id:        [101, 103]                   │     │  ├─ id:      [101, 103]    内存/Mmap    │
│  ├─ title:     ["red mug", "green tea"]     │     │  ├─ title:   [...]         内存/Mmap    │
│  ├─ price:     [19.8, 9.9]                  │     │  ├─ price:   [...]         内存/Mmap    │
│  ├─ embedding:                              │     │  └─ embedding: 可能不单独加载            │
│  │   [0.10, 0.20, 0.30, 0.40,  ← row 0     │     │                                        │
│  │    0.12, 0.18, 0.33, 0.39]  ← row 1     │     │  向量索引 (HNSW)                        │
│  ├─ RowIDs:    [90001, 90003]               │     │  ┌─────────────────────────────────┐    │
│  └─ Timestamps: [ts, ts]                    │     │  │  Layer 2:  ○                    │    │
│                                             │     │  │           / \                    │    │
│  ◆ 无索引，搜索走 brute force               │     │  │  Layer 1: ○───○                 │    │
│  ◆ C++ SegmentGrowingImpl 管理              │     │  │          /|\ /|\                │    │
│                                             │     │  │  Layer 0: ○─○─○─○ (所有向量)     │    │
└─────────────────────────────────────────────┘     │  └─────────────────────────────────┘    │
                                                    │                                        │
                                                    │  删除位图 (来自 L0 delta)               │
                                                    │                                        │
                                                    │  ◆ 搜索优先走索引 (SearchOnIndex)       │
                                                    └────────────────────────────────────────┘
```

---

## 四、插入端到端流程（含索引构建）

### 4.1 全景图

用户写入 3 行数据到 Collection 3001（2 个 VChannel）：

```json
[
  {"id": 101, "title": "red mug",     "price": 19.8, "embedding": [0.10, 0.20, 0.30, 0.40]},
  {"id": 102, "title": "blue bottle", "price": 29.9, "embedding": [0.40, 0.10, 0.20, 0.30]},
  {"id": 103, "title": "green tea",   "price":  9.9, "embedding": [0.12, 0.18, 0.33, 0.39]}
]
```

```
 ① Proxy 接收 + 校验 + 分配 RowID + Hash 分片
 ┌──────────────────────────────────────────────────────────────┐
 │  行式 JSON → 列式 FieldsData                                 │
 │                                                              │
 │  Hash(PK):  101 → ch0,  102 → ch1,  103 → ch0              │
 │                                                              │
 │  拆成 2 个 InsertMsg:                                        │
 │    ch0: id=[101,103] embedding=[vec0,vec2]  (2行)            │
 │    ch1: id=[102]     embedding=[vec1]       (1行)            │
 └──────┬──────────────────────────────────┬────────────────────┘
        │                                  │
 ② 写入 WAL                                │
        │                                  │
        ▼                                  ▼
 ┌──────────────┐                   ┌──────────────┐
 │  WAL (ch0)   │                   │  WAL (ch1)   │
 └──────┬───────┘                   └──────┬───────┘
        │                                  │
 ③ StreamingNode 分配 Segment + 缓冲        │
        │                                  │
        ▼                                  ▼
 ┌──────────────────┐            ┌──────────────────┐
 │ StreamingNode     │            │ StreamingNode     │
 │                  │            │                  │
 │ shard拦截器:      │            │ shard拦截器:      │
 │ 分配 Seg 7001    │            │ 分配 Seg 7002    │
 │                  │            │                  │
 │ WriteBuffer:     │            │ WriteBuffer:     │
 │ id=[101,103]     │            │ id=[102]         │
 │ (Go层缓冲,       │            │                  │
 │  等待Flush)      │            │                  │
 └──────┬───────────┘            └──────┬───────────┘
        │                               │
        │  ④ 同时 QueryNode 消费同一批消息回放到 Growing 段
        │     (CGO → C++ segcore.Insert → 立即可搜索)
        │                               │
 ⑤ Seal + Flush                         │
        │                               │
        ▼                               ▼
 ┌────────────────────────────────────────────────────────┐
 │  对象存储 (MinIO / S3)                                  │
 │                                                        │
 │  insert_log/3001/5001/7001/                            │
 │    ├── 100/...  (id 列)     每个字段一个 binlog 文件     │
 │    ├── 101/...  (title 列)                              │
 │    ├── 102/...  (price 列)                              │
 │    └── 103/...  (embedding 列)                          │
 │                                                        │
 └──────────────────────────┬─────────────────────────────┘
                            │
 ⑥ 索引构建                  │
                            ▼
 ┌────────────────────────────────────────────────────────┐
 │  DataCoord: indexInspector 发现 Flushed + 有索引定义    │
 │  → 创建 BuildID=88001                                  │
 │  → 发给 DataNode                                       │
 │                                                        │
 │  DataNode:                                             │
 │  → 读取 embedding 列 binlog                            │
 │  → Knowhere build HNSW                                 │
 │  → 上传 index/88001/1/5001/7001/{meta,graph,data}     │
 │                                                        │
 │  DataCoord: 更新 SegmentIndex state=Finished           │
 └──────────────────────────┬─────────────────────────────┘
                            │
 ⑦ 段可查询                  │
                            ▼
 ┌────────────────────────────────────────────────────────┐
 │  QueryNode: 加载 binlog + index files → Sealed 段可查  │
 └────────────────────────────────────────────────────────┘
```

**"插入成功"发生在 Proxy 写 WAL 成功的那一刻**，不等于已 Flush 到对象存储。

### 4.2 第①步：Proxy 接收与校验

入口：`internal/proxy/impl.go` → `Proxy.Insert()`

```
InsertRequest 进入
  → insertTask.PreExecute():
      校验 Collection 名 · 检查请求大小
      获取 Schema · 校验字段类型和维度
      处理 Auto-ID（分配业务主键）
      分配内部 RowID: [90001, 90002, 90003]   ← 不是业务主键
  → insertTask.Execute():
      Hash(PK) 路由到 VChannel
      拆成每个 channel 一个 InsertMsg
      写入 WAL
```

**业务主键 vs 内部 RowID**：

| | 业务主键 (id) | 内部 RowID |
|---|---|---|
| 用途 | 去重、删除、查询返回 | 底层存储组织 |
| 来源 | 用户提供或 Auto-ID | Proxy 分配 |
| 示例 | 101, 102, 103 | 90001, 90002, 90003 |

### 4.3 第②步：Hash 分片与写入 StreamingNode WAL

```
3 行数据, 2 个 VChannel (ch0, ch1)

  Hash(101) % 2 = 0 → ch0     InsertMsg(ch0):
  Hash(102) % 2 = 1 → ch1       id=[101,103], title=["red mug","green tea"]
  Hash(103) % 2 = 0 → ch0       price=[19.8,9.9], embedding=[vec0,vec2]

                                InsertMsg(ch1):
                                  id=[102], title=["blue bottle"]
                                  price=[29.9], embedding=[vec1]
```

关键：**一行只进一个 channel**（按 PK Hash），但每个 channel 内部数据是列式的。

Proxy 本地没有 WAL。`streaming.WAL().AppendMessages()` 的内部实现是通过 gRPC 把 InsertMsg 发给持有该 PChannel 的 StreamingNode，由 StreamingNode 持久化到 WAL 后返回。对 Proxy 来说是一次 RPC 调用，"写 WAL"和"发到 StreamingNode"是同一个动作。

代码：`internal/proxy/task_insert_streaming.go`

#### VChannel → PChannel → StreamingNode 路由

InsertMsg 按 VChannel 分组后，需要路由到持有对应 WAL 的 StreamingNode。路由分三层：

```
VChannel (逻辑通道)                    PChannel (物理通道)
  by0_3001v0  ──┐                       by0  ─── Assignment ──► StreamingNode-1
  by0_3001v1  ──┘ ToPhysicalChannel()   by0       (StreamingCoord 维护)
                  截掉 _3001v0 后缀
                  → "by0"
```

**1. VChannel → PChannel**：字符串截断。VChannel 格式为 `{PChannel}_{collectionID}v{idx}`，`funcutil.ToPhysicalChannel()` 截掉最后一个 `_` 后的后缀。同一 Collection 的多个 VChannel 可能映射到同一 PChannel。

**2. PChannel → StreamingNode**：通过 **Assignment（通道分配表）** 查找。StreamingCoord 维护 PChannel 到 StreamingNode 的映射，Proxy 侧的 `assignment.Watcher` 监听该分配表。

**3. 连接管理**：`walAccesserImpl` 按 PChannel 维护 `ResumableProducer` 池（`map[string]*ResumableProducer`）。同一 PChannel 的所有 VChannel 共享一个 Producer。Producer 创建时通过 Assignment 获取目标 StreamingNode 的 ServerID，建立 gRPC 连接。PChannel 迁移时 ResumableProducer 自动重连。

```
AppendMessages(msgs...)
  → dispatchMessages(): 按 msg.VChannel() 分组
  → getProducer(vchannel):
      pchannel = ToPhysicalChannel(vchannel)    // "by0_3001v0" → "by0"
      复用或创建 producers[pchannel]
  → ResumableProducer.BeginProduce(msgs...)
      → Watcher.Get(pchannel)                   // 查分配表
      → PChannelInfoAssigned { Channel, Node{ServerID} }
      → 本地有 WAL？直接写 : gRPC 写远程 StreamingNode
  → producer.Append(msg)
```

代码：
- VChannel→PChannel：`pkg/util/funcutil/func.go:393`
- Producer 路由：`internal/distributed/streaming/append.go:11`
- Assignment 发现：`internal/streamingnode/client/handler/handler_client_impl.go:153`
- 连接建立：`internal/distributed/streaming/internal/producer/producer.go:196`

### 4.4 第③步：StreamingNode 分配 Segment 与缓冲

**Segment 不是 Proxy 决定的，而是 StreamingNode 的 shard 拦截器分配的**：

```
InsertMessage 到达 StreamingNode
  → shard_interceptor.handleInsertMessage()
    → shardManager.AssignSegment()
      → 当前 (collection, partition, channel) 下有可写段？
        → 有：分配到该段
        → 无：先往 WAL 写 CreateSegmentMessage，再分配

WAL 中实际序列：
  1. CreateSegment(7001, channel=ch0)
  2. Insert(segment=7001, rows=[101,103])
  3. CreateSegment(7002, channel=ch1)
  4. Insert(segment=7002, rows=[102])
```

然后 Flusher 消费 WAL，按 SegmentID 分组写入 WriteBuffer（纯 Go 缓冲，不涉及 C++）。

代码：`internal/streamingnode/server/wal/interceptors/shard/shard_interceptor.go`

### 4.5 第④步：QueryNode 回放实现实时可查

**为什么刚 Insert 完就能 Search 到？** 因为 QueryNode 也在消费同一条 WAL，把数据回放到 Growing Segment：

```
QueryNode 消费 WAL
  → filterNode → insertNode
    → TransferInsertMsgToInsertRecord()     ← 转换为 C++ InsertRecord
    → delegator.ProcessInsert()
      → growing.Insert()
        → LocalSegment.Insert()
          → CGO: C.Insert(segment_ptr, ...)
            → SegmentGrowingImpl::Insert()  ← C++ 真正插入
```

C++ 内部执行：

```
对 Segment 7001:
  PreInsert(2) → reserved_offset = [0, 2)

  offset 0: rowID=90001, ts=t, pk=101, embedding=vec0
  offset 1: rowID=90003, ts=t, pk=103, embedding=vec2

  → 写入列存数组 + 建立 pk→offset 映射
  → 立即可被 SearchOnGrowing 暴力扫描
```

**两条并行链路的区别**：

| | StreamingNode WriteBuffer | QueryNode Growing Segment |
|---|---|---|
| 目的 | 持久化到对象存储 | 实时可查询 |
| 实现 | 纯 Go 缓冲 | C++ segcore |
| 是否调 CGO | 否 | 是 |
| 后续 | Flush → binlog 文件 | Sealed 后被 Sealed Segment 替代 |

### 4.6 第⑤步：封存与 Flush

**封存触发条件**（`internal/datacoord/segment_allocation_policy.go`）：

| 策略 | 条件 |
|------|------|
| 容量封存 | 行数达到上限 |
| 时间封存 | 存活时间超过阈值 |
| Binlog 数量 | binlog 文件数过多 |
| 空闲封存 | 长时间无新写入 |

**Flush 过程**：WriteBuffer 中的列式数据 → 编码为 binlog 文件 → 上传到对象存储

产出文件：

| 文件类型 | 内容 |
|---------|------|
| `insertBinlogs` | 每个字段的列数据 |
| `statsBinlogs` | 统计信息（min/max PK, rowCount） |
| `deltaBinlog` | 删除日志（如有） |

状态转换：`Growing → Sealed → Flushing → Flushed`

代码：`internal/flushcommon/syncmgr/task.go`，`internal/flushcommon/writebuffer/write_buffer.go`

### 4.7 第⑥步：索引构建

**Insert 和 CreateIndex 是两条完全独立的 API**，交汇点在 Flushed Segment：

```
┌─── Insert 链路 ───┐      ┌─── CreateIndex 链路 ───┐
│                   │      │                        │
│ Proxy → WAL →     │      │ Proxy → DataCoord      │
│ StreamingNode →   │      │ → 保存 Index 定义       │
│ Flush → binlog    │      │   到 metastore          │
│                   │      │                        │
└────────┬──────────┘      └────────┬───────────────┘
         │                          │
         └──────────┬───────────────┘
                    │
       indexInspector 持续扫描:
       "有 Flushed Segment" ∩ "有 Index 定义"
                    │
                    ▼
         ┌──────────────────────────────────────────┐
         │  DataCoord:                               │
         │    分配 BuildID = 88001                   │
         │    组装 CreateJobRequest:                 │
         │      Segment Meta → numRows=2            │
         │      Schema → FloatVector, dim=4         │
         │      Index 定义 → HNSW, L2, M=16, ef=200 │
         │      binlog paths                        │
         │    发送给 DataNode                        │
         ├──────────────────────────────────────────┤
         │  DataNode:                               │
         │    读取 embedding 列 binlog              │
         │    调 Knowhere 构建 HNSW 索引            │
         │    上传到 index/88001/1/5001/7001/       │
         │    报告完成                               │
         ├──────────────────────────────────────────┤
         │  DataCoord:                               │
         │    更新 SegmentIndex state=Finished       │
         │    记录 IndexFilePaths                    │
         └──────────────────────────────────────────┘
```

**不论谁先到都能正常工作**：

| 顺序 | 行为 |
|------|------|
| 先 Insert 后 CreateIndex | 已 Flushed 段被 indexInspector 发现并补建 |
| 先 CreateIndex 后 Insert | Flush 后 indexInspector 触发构建 |
| 只 Insert 不 CreateIndex | 搜索走 brute force，性能差但正常工作 |

代码：`internal/datacoord/index_inspector.go`，`internal/datanode/index/task_index.go`

### 4.8 第⑦步：段可查询

```
QueryCoord 检测到新的 Flushed + 有索引的段
  → 分配到 QueryNode
  → QueryNode 下载 binlog + index files
  → 构建 Sealed Segment（C++ ChunkedSegmentSealedImpl）
  → 段可查询，搜索走 SearchOnIndex
  → 对应的 Growing Segment 被释放
```

### 4.9 数据形态变化总表

```
 用户 JSON (行式)
   {"id":101, "embedding":[0.10,0.20,0.30,0.40]}
                │
                ▼
 Proxy 内部 (列式 FieldData)
   ids=[101,102,103], embeddings=[vec0,vec1,vec2]
                │
                ▼ Hash 分片
 InsertMsg (每 channel 一份列式子集)
   ch0: ids=[101,103], embeddings=[vec0,vec2]
                │
                ▼
 WAL 消息 (Header + Body)
   header={collectionID, partitionID}
   body={列式字段数据}
                │
          ┌─────┴─────┐
          ▼           ▼
 Growing 段        WriteBuffer
 (C++连续数组)     (Go缓冲)
          │           │
          │           ▼ Flush
          │     每字段一个 binlog 文件
          │           │
          │           ▼ 索引构建
          │     index files (HNSW graph等)
          │           │
          ▼           ▼
       QueryNode 加载为 Sealed 段
```

---

## 五、搜索端到端流程

### 5.1 全景图

用户搜索：`q=[0.11,0.19,0.31,0.41], filter="price>10", topK=2`

```
 ① Proxy: 解析 + 生成查询计划 + 编码查询向量
 ┌──────────────────────────────────────────────────────────────┐
 │                                                              │
 │  Schema 校验 · 验证输出字段                                   │
 │                                                              │
 │  生成 PlanNode:                                              │
 │    VectorANNS {                                              │
 │      field = 103 (embedding)                                 │
 │      metric = L2                                             │
 │      topK = 2                                                │
 │      predicates: price > 10                                  │
 │    }                                                         │
 │                                                              │
 │  查询向量 → PlaceholderGroup (二进制编码)                     │
 │                                                              │
 │  广播到所有 VChannel (Insert 一行只进一个 shard;              │
 │                       Search 广播到所有 shard)               │
 └──────┬──────────────────────────────────────┬────────────────┘
        │                                      │
 ② QueryNode 执行段级搜索                       │
        │                                      │
        ▼                                      ▼
 ┌───────────────────────────┐      ┌───────────────────────────┐
 │  QueryNode-A (ch0)        │      │  QueryNode-B (ch1)        │
 │                           │      │                           │
 │  Delegator 编排:          │      │  Delegator 编排:          │
 │  ├─ Sealed Seg 7001      │      │  ├─ Sealed Seg 7002      │
 │  └─ Growing Segs (如有)   │      │  └─ Growing Segs (如有)   │
 │                           │      │                           │
 │  对每个段并行:             │      │  对每个段并行:             │
 │  ┌────────────────────┐   │      │  ┌────────────────────┐   │
 │  │ C++ segcore 执行:  │   │      │  │ C++ segcore 执行:  │   │
 │  │                    │   │      │  │                    │   │
 │  │ 1. HNSW ANN search│   │      │  │ 1. HNSW ANN search│   │
 │  │    → 候选集        │   │      │  │    → 候选集        │   │
 │  │                    │   │      │  │                    │   │
 │  │ 2. 标量过滤        │   │      │  │ 2. 标量过滤        │   │
 │  │    price > 10      │   │      │  │    price > 10      │   │
 │  │                    │   │      │  │                    │   │
 │  │ 3. L0 删除过滤     │   │      │  │ 3. L0 删除过滤     │   │
 │  └────────────────────┘   │      │  └────────────────────┘   │
 │                           │      │                           │
 │  段级结果:                 │      │  段级结果:                 │
 │  id=101, score=0.0004     │      │  id=102, score=0.162      │
 │  (103 被 price≤10 过滤)   │      │                           │
 │                           │      │                           │
 │  节点内 Reduce → TopK     │      │  节点内 Reduce → TopK     │
 └──────────┬────────────────┘      └──────────┬────────────────┘
            │                                   │
 ③ Proxy 跨分片归并                              │
            │                                   │
            └─────────────┬─────────────────────┘
                          │
                          ▼
 ┌────────────────────────────────────────────────────────────────┐
 │  Proxy: 跨分片 TopK Reduce                                    │
 │                                                                │
 │  合并所有 shard 结果 → 全局排序 → 取 TopK=2                    │
 │                                                                │
 │  ┌──────┬──────┬─────────┬───────────────┬────────┐           │
 │  │ rank │  id  │  score  │  title        │  price │           │
 │  │   1  │  101 │  0.0004 │  "red mug"    │  19.8  │           │
 │  │   2  │  102 │  0.162  │  "blue bottle"│  29.9  │           │
 │  └──────┴──────┴─────────┴───────────────┴────────┘           │
 │                                                                │
 │  L2 距离: 越小越相似                                            │
 └────────────────────────────────────────────────────────────────┘
```

### 5.2 第①步：Proxy 解析与查询计划生成

入口：`internal/proxy/task_search.go` → `searchTask`

```
SearchRequest 进入
  → PreExecute():
      解析 Collection Schema
      验证输出字段 (translateOutputFields)
      解析搜索参数: metric_type, topK, nq, search_params
      生成查询计划: tryGeneratePlan()
        → 解析 filter 表达式 "price > 10"
        → 构建 PlanNode: VectorANNS + Predicates
      编码查询向量为 PlaceholderGroup (二进制)
  → Execute():
      获取所有 VChannel
      通过 LBPolicy 广播到各 QueryNode
```

表达式解析器：`internal/parser/planparserv2/plan_parser_v2.go`

### 5.3 第②步：分片路由与 QueryNode 执行

**路由**（`internal/proxy/shardclient/lb_policy.go`）：

```
Collection 有 N 个 VChannel
  → GetShardLeaderList(): VChannel → QueryNode 映射
  → errgroup 并行发送搜索请求到各 Shard Leader
  → 支持 replica failover + round-robin 负载均衡
```

**QueryNode 内部执行**（`internal/querynodev2/`）：

```
Search RPC 到达
  → handlers.go: 接收请求
  → delegator: 编排搜索
      ├─ SearchHistorical(): 对所有 Sealed Segment 并行搜索
      └─ SearchStreaming():  对所有 Growing Segment 并行搜索

对每个 Segment (errgroup 并发):
  → LocalSegment.Search()
    → ptrLock.PinIf(...)              // 保证搜索期间段不被释放
    → csegment.Search(searchReq)      // CGO 桥接
      → C.AsyncSearch(segment_ptr, plan, placeholder_group, ...)

C++ segcore 内部:
  → 解析 PlanNode
  → 有 HNSW 索引 → SearchOnIndex (图遍历)
     无索引      → SearchBruteForce (暴力扫描)
     Growing 段  → SearchOnGrowing (暴力扫描)
  → 执行标量过滤 (price > 10)
  → 应用 L0 删除过滤
  → 返回段级 TopK 结果
```

### 5.4 第③步：两级结果归并

```
            段级结果                  节点级结果              全局结果
         ┌──────────┐
Seg 7001 │101: 0.0004│──┐
         └──────────┘  │
                       ├──► QN-A Reduce ──┐
         ┌──────────┐  │    [(101,0.0004)] │
Seg 700x │...       │──┘                  │
         └──────────┘                     ├──► Proxy Reduce
                                          │    全局 TopK
         ┌──────────┐                     │    [(101,0.0004),
Seg 7002 │102: 0.162│──┐                  │     (102,0.162)]
         └──────────┘  │                  │
                       ├──► QN-B Reduce ──┘
         ┌──────────┐  │    [(102,0.162)]
Seg 700y │...       │──┘
         └──────────┘
```

**第一级**：QueryNode 内 `ReduceSearchResults()` — 合并同一节点下各段的结果（`internal/querynodev2/segments/result.go`）

**第二级**：Proxy `reduceSearchResults()` — 合并所有 QueryNode 的结果，全局排序取 TopK

### 5.5 Query vs Search vs Hybrid Search

| | Search | Query | Hybrid Search |
|---|---|---|---|
| 入口 | `task_search.go` | `task_query.go` | `task_search.go` |
| 是否做向量计算 | 是 (ANN) | 否 | 是 (多路 ANN) |
| 返回 | ids + scores + 输出字段 | ids + 字段值 | ids + rerank 分数 |
| 路由 | 广播到所有 shard | 广播到所有 shard | 广播到所有 shard |
| C++ 调用 | `AsyncSearch` | `AsyncRetrieve` | 多次 `AsyncSearch` + Proxy 端 rerank |

---

## 六、HNSW 算法原理

HNSW（Hierarchical Navigable Small World）是 Milvus 默认推荐的向量索引算法。

### 核心思想

将向量数据组织成一个**多层图结构**，上层稀疏用于快速跳跃，底层稠密保证精度：

```
                    入口点
Layer 3:              ○                          ← 极少节点，长距离跳跃
                     / \
Layer 2:            ○───○                        ← 较少节点
                   /|   |\
Layer 1:          ○─○───○─○                      ← 更多节点
                 /|\ \ / /|\
Layer 0:        ○─○─○─○─○─○─○─○                 ← 所有节点，短距离精确连接
```

### 构建过程

1. 每个新向量随机分配一个层级 L（概率指数衰减，大多数在 Layer 0）
2. 从最高层的入口点开始，**贪心搜索**找到距离新向量最近的节点
3. 逐层下降，在每一层都连接 M 个最近邻居
4. 参数 `M` 控制每个节点的连接数，`efConstruction` 控制构建时的搜索宽度

### 搜索过程

```
查询向量 q = [0.11, 0.19, 0.31, 0.41]

Step 1: 从 Layer 3 入口点开始
        贪心地找最近邻 → 跳到更近的节点

Step 2: 下降到 Layer 2
        在当前区域扩展搜索

Step 3: 下降到 Layer 1
        搜索范围更广

Step 4: 在 Layer 0 精细搜索
        用 ef_search 控制候选集大小
        返回 TopK 最近邻
```

### 关键参数

| 参数 | 含义 | 影响 |
|------|------|------|
| `M` | 每个节点的最大连接数 | 越大 → 精度越高，内存越多，构建越慢 |
| `efConstruction` | 构建时搜索宽度 | 越大 → 索引质量越高，构建越慢 |
| `ef` (搜索时) | 搜索时候选集大小 | 越大 → 精度越高，搜索越慢 |

### 为什么 HNSW 快

- **跳表思想**：高层稀疏图实现长距离跳跃，避免在底层逐个遍历
- **贪心搜索**：每一步都走向更近的邻居，快速收敛到目标区域
- **全内存**：图结构完全驻留内存，无磁盘 IO
- 时间复杂度：O(log N)，远优于暴力扫描的 O(N)

### 在 Milvus 中的位置

```
用户 CreateIndex(index_type="HNSW", metric_type="L2", M=16, efConstruction=200)
  → DataCoord 保存 Index 定义
  → indexInspector 为每个 Flushed Segment 创建 Build 任务
  → DataNode 读取向量 binlog
  → 调用 Knowhere 库构建 HNSW 图
  → 上传 index files (hnsw_meta + hnsw_graph + hnsw_data)
  → QueryNode 加载后，Search 走 SearchOnIndex (图遍历)
     而非 SearchBruteForce (暴力扫描)
```
