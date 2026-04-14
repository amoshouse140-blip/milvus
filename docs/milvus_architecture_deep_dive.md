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

这一步有两个阶段：先由 shard 拦截器给 InsertMsg **打上 SegmentID 标签**，再由 Flusher 消费 WAL 把数据**按 Segment 分组缓冲**。

#### 阶段一：shard 拦截器分配 SegmentID

Proxy 发来的 InsertMsg 只携带 CollectionID、PartitionID、VChannel 和列式数据，**没有 SegmentID**。SegmentID 由 StreamingNode 的 shard 拦截器在写入 WAL 前分配：

```
InsertMessage 到达 StreamingNode
  → shard_interceptor.handleInsertMessage()
    → 遍历 header.Partitions:
        → shardManager.AssignSegment(AssignSegmentRequest{
              CollectionID, PartitionID,
              Rows,          // 本批行数
              BinarySize,    // 本批数据大小
              TimeTick,
          })
        → 当前 (collection, partition, channel) 下有可写的 Growing 段？
            → 有：返回该段的 SegmentID
            → 无：先往 WAL 写 CreateSegmentMessage 创建新段，再返回新 SegmentID
    → 将 SegmentID 写入 InsertMsg 的 header:
        partition.SegmentAssignment = { SegmentId: result.SegmentID }
    → OverwriteHeader(header)
    → appendOp(ctx, msg)   // 带着 SegmentID 写入 WAL
```

shard 拦截器在写入 WAL **之前**完成 SegmentID 填充，写入 WAL 的已经是带 SegmentID 的最终形态：

```
Proxy 发出的 InsertMsg:                 shard 拦截器处理后写入 WAL 的:
  InsertMsg(ch0):                        CreateSegment(7001, ch0)     ← 如果是新段
    collectionID = 3001                  InsertMsg(ch0):
    partitionID  = 5001                    collectionID = 3001
    id = [101, 103]                        partitionID  = 5001
    embedding = [vec0, vec2]               segmentID    = 7001        ← 拦截器填入
    segmentID = (无)                       id = [101, 103]
                                           embedding = [vec0, vec2]
```

代码：`internal/streamingnode/server/wal/interceptors/shard/shard_interceptor.go:144`

#### 阶段二：Flusher 消费 WAL，按 Segment 缓冲到 WriteBuffer

WAL 中的 InsertMsg 现在已经带有 SegmentID。StreamingNode 内部的 Flusher 消费 WAL，将数据按 SegmentID 分组转换为 `InsertData`，写入对应 Segment 的 WriteBuffer：

```
WAL Scanner 消费消息
  → writeNode.Operate()
    → PrepareInsert(schema, pkField, insertMsgs):
        lo.GroupBy(insertMsgs, msg.SegmentID)    // 按 SegmentID 分组
        对每组:
          InsertMsgToInsertData(msg, schema)      // InsertMsg → storage.InsertData
          提取 pkField、tsField、构建 pk→ts 映射
        → 返回 []*InsertData (每个 Segment 一份)
    → BufferManager.BufferData(channel, insertData, ...):
        对每个 InsertData:
          getOrCreateBuffer(segmentID)            // 每个 Segment 一个 SegmentBuffer
          segBuf.insertBuffer.Buffer(inData)      // 追加到缓冲区
```

数据形态变化：

```
InsertMsg (WAL 中)                    InsertData (WriteBuffer 中)
  channel 级别的列式数据                 Segment 级别的列式数据
  可能包含多个 Segment 的行              只属于一个 Segment
  protobuf 编码                        storage.InsertData (Go 内存结构)

  InsertMsg(ch0, seg=7001):            InsertData(seg=7001):
    id = [101, 103]                      segmentID = 7001
    embedding = [vec0, vec2]             partitionID = 5001
    price = [19.8, 9.9]                  data = []*storage.InsertData:
    title = ["red mug","green tea"]        field 100: [101, 103]
                                           field 101: ["red mug","green tea"]
                                           field 102: [19.8, 9.9]
                                           field 103: [vec0, vec2]
                                         pkField: [101, 103]
                                         tsField: [ts, ts]
                                         intPKTs: {101→ts, 103→ts}  ← pk→timestamp 映射
                                         rowNum: 2
```

WriteBuffer 是纯 Go 内存缓冲，不涉及 C++ segcore。后续 Flush 时，WriteBuffer 中的数据会被编码为 binlog 文件上传到对象存储（见 4.6）。

代码：
- PrepareInsert：`internal/flushcommon/writebuffer/write_buffer.go:702`
- BufferData：`internal/flushcommon/writebuffer/l0_write_buffer.go:62`
- InsertData 结构：`internal/flushcommon/writebuffer/write_buffer.go:412`

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

这一步分两个阶段：先由 SyncPolicy 决定哪些 Segment 的 WriteBuffer 需要刷盘，再由 SyncTask 将数据序列化为 binlog 文件上传到对象存储。

#### 阶段一：SyncPolicy 选择需要刷盘的 Segment

每次 `BufferData()` 写入新数据后，WriteBuffer 调用 `triggerSync()` 检查所有已注册的 SyncPolicy，决定哪些 Segment 需要刷盘：

```
BufferData(insertData)
  → triggerSync()
    → getSegmentsToSync(ts, policies...)
      → 对每个 policy:
          policy.SelectSegments(所有 segmentBuffer, 当前时间戳)
          → 返回需要 sync 的 segmentID 列表
    → syncSegments(segmentIDs)
```

SyncPolicy 类型：

| Policy | 触发条件 | 说明 |
|--------|---------|------|
| `GetFullBufferPolicy` | `buf.IsFull()` | 单个 Segment 的缓冲区写满 |
| `GetSyncStaleBufferPolicy` | 缓冲存活超过 staleDuration | 数据在内存停留过久 |
| `GetSealedSegmentsPolicy` | Segment 状态为 Sealed | DataCoord 封存了该段 |
| `GetDroppedSegmentPolicy` | Segment 状态为 Dropped | Segment 被标记删除 |
| `GetFlushTsPolicy` | checkpoint ≥ flushTs | 手动 Flush 触发 |
| `GetOldestBufferPolicy` | 内存压力时淘汰最老的 buffer | 外部 EvictBuffer 调用 |

封存（Seal）和 Flush 是不同的动作：
- **Seal**：DataCoord 决定某个 Segment 不再接受新写入，将 metaCache 中的状态从 Growing 改为 Sealed。触发条件在 `internal/datacoord/segment_allocation_policy.go`（行数上限、存活时间、binlog 文件数、空闲超时）。
- **Flush**：WriteBuffer 将该 Segment 的缓冲数据序列化并上传到对象存储。

状态转换：

```
Growing → Sealed (DataCoord 封存，不再接受写入)
       → Flushing (WriteBuffer 正在刷盘)
       → Flushed (数据已落对象存储)
```

代码：`internal/flushcommon/writebuffer/sync_policy.go`

#### 阶段二：SyncTask 将 InsertData 序列化为 binlog 上传

`syncSegments()` 为每个需要刷盘的 Segment 创建 SyncTask：

```
syncSegments(segmentIDs)
  → 对每个 segmentID:
      getSyncTask(segmentID)
        → yieldBuffer(segmentID)                    // 取出 WriteBuffer 中的数据
        → 返回 insert, delta, schema, timeRange
        → 组装 SyncPack { insertData, deleteData, segmentID, ... }
        → 创建 SyncTask
      syncMgr.SyncData(syncTask)
        → SyncTask.Run()
```

`SyncTask.Run()` 的核心流程：

```
SyncTask.Run()
  → 根据 storageVersion 选择 Writer (V2/V3/Legacy)
  → Writer.Write(SyncPack):
      遍历 SyncPack.insertData (即 WriteBuffer 中的 []*storage.InsertData)
      对每个字段:
        序列化为 Arrow/Parquet 格式
        上传到对象存储: insert_log/{collectionID}/{partitionID}/{segmentID}/{fieldID}/{logID}
      生成统计信息 (min/max PK, rowCount)
        上传到: stats_log/{collectionID}/{partitionID}/{segmentID}/{fieldID}/{logID}
      如有删除记录:
        上传到: delta_log/{collectionID}/{partitionID}/{segmentID}/{logID}
  → 返回 insertBinlogs, statsBinlogs, deltaBinlog, manifestPath
  → writeMeta(): 将 binlog 路径写回 DataCoord (元数据)
  → 更新 metaCache: FinishSyncing, 如果是 Flush 则更新状态为 Flushed
```

数据形态变化：

```
WriteBuffer 中 (Go 内存)                    对象存储中 (binlog 文件)

InsertData(seg=7001):                      insert_log/3001/5001/7001/
  Data = map[FieldID]FieldData               100/910001 → [101, 103]        Arrow/Parquet
    100: Int64FieldData                      101/910002 → ["red mug", ...]  编码
         {Data: [101, 103]}      ──────►     102/910003 → [19.8, 9.9]
    101: VarCharFieldData                    103/910004 → [vec0, vec2]
         {Data: ["red mug","green tea"]}
    102: FloatFieldData                    stats_log/3001/5001/7001/
         {Data: [19.8, 9.9]}                 100/920001 → {minPK=101, maxPK=103,
    103: FloatVectorFieldData                              rowCount=2}
         {Data: [0.10,0.20,...], Dim: 4}
```

Flush 后 InsertData 的内容和对象存储中 binlog 的内容是**同一批数据**，区别在于：
- **编码格式**：从 Go 原生类型变为 Arrow/Parquet 二进制
- **组织方式**：从 `map[FieldID]FieldData` 变为每个字段单独一个文件
- **附加产物**：额外生成 statsBinlog（统计信息）和 deltaBinlog（删除记录）
- **元数据更新**：binlog 文件路径回写到 DataCoord，Segment 状态变为 Flushed

代码：
- SyncTask：`internal/flushcommon/syncmgr/task.go:128`
- getSyncTask（组装 SyncPack）：`internal/flushcommon/writebuffer/write_buffer.go:563`
- syncSegments（触发刷盘）：`internal/flushcommon/writebuffer/write_buffer.go:325`

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
              触发索引构建
```

**不论谁先到都能正常工作**：

| 顺序 | 行为 |
|------|------|
| 先 Insert 后 CreateIndex | 已 Flushed 段被 indexInspector 发现并补建 |
| 先 CreateIndex 后 Insert | Flush 后 indexInspector 触发构建 |
| 只 Insert 不 CreateIndex | 搜索走 brute force，性能差但正常工作 |

#### 4.7.1 indexInspector 触发机制

DataCoord 内的 `indexInspector` 组件（`internal/datacoord/index_inspector.go`）通过三个来源触发索引构建：

```
indexInspector.createIndexForSegmentLoop()
├── ticker（定时扫描）
│     每隔 TaskCheckInterval 秒扫描所有 Flushed Segment
│     找出尚未建索引的 segment → 逐个创建任务
│     这是兜底机制
│
├── notifyIndexChan（CreateIndex 回调）
│     用户调用 CreateIndex → DataCoord 保存索引定义
│     → 发送 collectionID 到 notifyIndexChan
│     → inspector 立即扫描该 collection 下所有 Flushed Segment
│
└── getBuildIndexChSingleton()（Flush 完成通知）
      segment flush 完成 → 发送 segmentID
      → inspector 立即为该 segment 创建索引任务
```

扫描到需要建索引的 segment 后，`createIndexesForSegment()` 检查该 segment 上缺失哪些索引（collection 可能定义了多个索引），为每个缺失的索引调用 `createIndexForSegment()` 创建任务。

#### 4.7.2 DataCoord 组装 CreateJobRequest

`createIndexForSegment()` 将 segment 元数据 + 索引定义打包成一份 `CreateJobRequest` 发给 DataNode：

```
createIndexForSegment()
│
├── allocator.AllocID() → 分配全局唯一 BuildID (如 88001)
├── 从 indexMeta 读取索引定义 → IndexParams, TypeParams
├── 从 segment 元数据读取 → binlog 文件 ID 列表, 行数
├── 评估资源槽位（根据字段数据大小 + 索引类型）
│
└── 组装 SegmentIndex 元数据并持久化:
      SegmentIndex {
        SegmentID    = 7001
        CollectionID = 100
        IndexID      = 9001        // collection 级索引定义 ID
        BuildID      = 88001       // 本次构建的唯一 ID
        State        = Init
      }
```

随后 `prepareJobRequest()` 进一步填充：

```
CreateJobRequest {
  BuildID         = 88001
  SegmentID       = 7001
  FieldID         = 103 (embedding)
  NumRows         = 50000
  Dim             = 128
  IndexParams     = {index_type: HNSW, metric_type: L2, M: 16, efConstruction: 200}
  DataIds         = [401, 402, 403]  ← 该 segment 该字段的 binlog 文件 ID
  IndexFilePrefix = "index/"         ← 索引文件存储前缀
  StorageConfig   = {bucket, rootPath, accessKey, ...}
  Field           = FieldSchema{FieldID=103, DataType=FloatVector}
}
```

**关键：DataIds 指向的是 4.6 中 Flush 生成的字段 binlog 文件路径**。
DataNode 建索引时从对象存储读取这些 binlog 文件，而不是从 WAL 或内存读取。

代码：`internal/datacoord/task_index.go:227` `prepareJobRequest()`

#### 4.7.3 DataNode 接收与执行

DataNode 收到 `CreateJobRequest` 后创建 `indexBuildTask`，经过三个阶段：

```
indexBuildTask
├── PreExecute():  补全路径、参数、字段元信息
├── Execute():     读取 binlog + 调 C++ 构建索引
└── PostExecute(): 序列化 + 上传索引文件
```

**PreExecute — 路径构造**

将 `DataIds`（binlog 文件 ID）转换为完整的对象存储路径：

```
DataIds = [401, 402, 403]
           │
           ▼ metautil.BuildInsertLogPath()
DataPaths = [
  "{rootPath}/insert_log/100/5001/7001/103/401",
  "{rootPath}/insert_log/100/5001/7001/103/402",
  "{rootPath}/insert_log/100/5001/7001/103/403",
]
路径格式: {rootPath}/insert_log/{collectionID}/{partitionID}/{segmentID}/{fieldID}/{logID}
```

代码：`internal/datanode/index/task_index.go:174`

**Execute — 数据读取与索引构建**

Execute 将所有信息组装成 `BuildIndexInfo` protobuf，通过 CGO 传递给 C++ 层：

```
Go 层                                    C++ 层
──────                                   ──────
BuildIndexInfo (protobuf)
  ├ InsertFiles = DataPaths
  ├ FieldSchema = {FloatVector, dim=128}
  ├ IndexParams = {HNSW, L2, M=16, ...}     ParseFromArray()
  ├ NumRows = 50000                    ──────────────────►
  ├ StorageConfig = {...}                    │
  └ IndexFilePrefix = "index/..."            ▼
                                     IndexFactory::CreateIndex()
                                       → VecIndexCreator (HNSW)
                                             │
                                             ▼
                                         Build()
```

代码：`internal/datanode/index/task_index.go:350` → `internal/util/indexcgowrapper/index.go:104` `CreateIndex()`

#### 4.7.4 C++ 层数据变化详解

`VecIndexCreator::Build()` 内部的数据变化过程：

```
阶段 1: 读取 binlog 文件 → FieldData
─────────────────────────────────────────────────
file_manager_->CacheRawDataToMemory(config)
  │
  │  从对象存储批量下载 binlog 文件
  │  每个 binlog 文件是 Parquet 格式，存储单个字段的列数据
  │
  ▼
vector<FieldDataPtr> field_datas
  field_datas[0]: FieldData<FloatVector> { dim=128, rows=8192, data=[0.1, 0.3, ...] }
  field_datas[1]: FieldData<FloatVector> { dim=128, rows=8192, data=[0.5, 0.2, ...] }
  field_datas[2]: FieldData<FloatVector> { dim=128, rows=33616, data=[...] }
  (一个 binlog 文件对应一个 FieldData)


阶段 2: 拼接为连续内存 → float[]
─────────────────────────────────────────────────
for (auto& data : field_datas) {
    memcpy(buf + offset, data->Data(), data->DataSize());
}
  │
  ▼
uint8_t[] buf (连续内存块)
  总大小 = 50000 × 128 × 4 bytes = 25,600,000 bytes
  内存布局: [vec_0: 128 floats][vec_1: 128 floats]...[vec_49999: 128 floats]


阶段 3: 包装为 Knowhere Dataset
─────────────────────────────────────────────────
GenDataset(total_rows=50000, dim=128, buf)
  │
  ▼
knowhere::DataSet {
  rows = 50000
  dim  = 128
  data = buf (float* 指针，零拷贝)
}


阶段 4: 调用 Knowhere 构建 HNSW
─────────────────────────────────────────────────
index_.Build(dataset, index_config)
  │
  │  index_config = {
  │    index_type: HNSW,
  │    metric_type: L2,
  │    M: 16,             // 每个节点最大邻居数
  │    efConstruction: 200 // 构建时搜索宽度
  │  }
  │
  │  Knowhere 内部执行:
  │    1. 逐条向量插入 HNSW 图
  │    2. 对每条向量，使用 efConstruction 参数进行贪心搜索
  │       找到最近邻作为候选邻居
  │    3. 按照 M 参数限制连接数，建立双向边
  │    4. 多层结构：底层包含所有节点，上层概率递减
  │
  ▼
内存中的 HNSW 图结构 (CgoIndex.indexPtr)
  ├ 导航层 (layer 2): 少量节点 + 稀疏连接
  ├ 中间层 (layer 1): 更多节点 + 连接
  └ 底层   (layer 0): 全部 50000 个节点
                       每个节点最多 2*M=32 条边
                       每条边 = 邻居节点 ID
```

代码：`internal/core/src/index/VectorMemIndex.cpp:387` `Build()`

#### 4.7.5 PostExecute — 序列化与上传

```
阶段 5: HNSW 图 → 序列化二进制 → 上传到对象存储
─────────────────────────────────────────────────
index.UpLoad()
  │
  ├── index_.Serialize() → knowhere::BinarySet
  │     HNSW 图结构序列化为二进制字节流
  │     包含: 图的邻接表、向量数据、元数据
  │
  ├── file_manager_->AddFile(binary_set)
  │     上传到对象存储
  │     路径: index/{buildID}/{version}/{partitionID}/{segmentID}/{fileKey}
  │     示例: index/88001/1/5001/7001/abc123
  │
  └── 返回 IndexStats {
        SerializedIndexInfos: [{fileName: "abc123", fileSize: 28MB}, ...]
        MemSize: 30MB  // 加载后的内存占用
      }
```

DataNode 将结果（fileKeys、大小）存储在本地 TaskManager 中，等 DataCoord 查询时返回。

代码：`internal/datanode/index/task_index.go:366` `PostExecute()`

#### 4.7.6 DataCoord 确认完成

DataCoord 的 `QueryTaskOnWorker()` 定期轮询 DataNode 获取任务状态：

```
DataCoord                          DataNode
   │                                  │
   │── QueryJobs(buildID=88001) ────►│
   │                                  │
   │◄── IndexTaskInfo {              │
   │      BuildID: 88001              │
   │      State: Finished             │
   │      IndexFileKeys: ["abc123"]   │
   │      SerializedSize: 28MB        │
   │    } ────────────────────────────│
   │                                  │
   ▼
FinishTask():
  更新 metastore:
    SegmentIndex {
      BuildID = 88001
      State   = Finished
      IndexFileKeys = ["abc123"]
      IndexSize = 28MB
    }
```

至此，索引文件已持久化到对象存储，元数据已记录在 metastore。
后续 QueryNode 加载该 segment 时，会根据 `IndexFileKeys` 从对象存储下载索引文件并加载到内存。

#### 4.7.7 全流程数据变化总结

```
对象存储 binlog 文件 (Parquet)
  insert_log/.../7001/103/401  ← field 103 的列数据
  insert_log/.../7001/103/402
  insert_log/.../7001/103/403
        │
        │ C++ MemFileManager 批量下载 + 反序列化
        ▼
vector<FieldData<FloatVector>>
  [FieldData{rows=8192, data=[0.1,0.3,...]}, ...]
        │
        │ memcpy 拼接为连续内存
        ▼
float[] buf  (50000 × 128 = 6,400,000 个 float)
        │
        │ GenDataset() 零拷贝包装
        ▼
knowhere::DataSet {rows=50000, dim=128, data=buf}
        │
        │ Knowhere HNSW Build (逐条插入 + 建图)
        ▼
HNSW 图 (内存中的邻接表结构)
        │
        │ Serialize + Upload
        ▼
对象存储索引文件
  index/88001/1/5001/7001/abc123  ← HNSW 图的二进制序列化
```

代码：`internal/datacoord/index_inspector.go`，`internal/datacoord/task_index.go`，`internal/datanode/index/task_index.go`，`internal/core/src/index/VectorMemIndex.cpp`

### 4.8 第⑦步：段可查询

**LoadCollection 和 CreateIndex 类似，都是"调用一次，后续自动生效"的 API。**
不调用 LoadCollection，即使数据已插入、索引已构建，也完全无法搜索。

#### 4.8.1 LoadCollection 触发的初始化

用户调用 `LoadCollection` 后，QueryCoord 启动对该 collection 的持续监听：

```
用户调用 LoadCollection(collectionID=100)
        │
        ▼
QueryCoord:
  ├── 记录 collection 100 为"已加载"状态
  ├── 启动 TargetObserver 持续监听该 collection
  │     定期调 DataCoord.GetRecoveryInfoV2(collectionID)
  │     获取最新的 segment 列表 + channel 列表
  │     → 更新 NextTarget（期望状态）
  │
  └── SegmentChecker 定期检查:
        对比 NextTarget（期望）vs 当前分布（实际）
        → 发现缺失的 segment → 生成 Load 任务
        → 发现多余的 segment → 生成 Release 任务
```

代码：`internal/querycoordv2/services.go:197` `LoadCollection()`

#### 4.8.2 TargetObserver — 感知新 segment

`TargetObserver` 是 QueryCoord 内的后台组件，持续从 DataCoord 拉取最新的 segment 信息：

```
TargetObserver.updateNextTarget(collectionID)
  │
  │  调用 DataCoord.GetRecoveryInfoV2()
  │  返回: 所有 Flushed Segment 信息 + VChannel 信息
  │
  ▼
NextTarget = {
  segments: {
    7001: {segmentID=7001, state=Flushed, binlogs=[...], indexInfos=[...]},
    7002: {segmentID=7002, state=Flushed, binlogs=[...], indexInfos=[...]},
    ...
  },
  channels: {
    "ch0": {channelName="ch0", seekPosition=...},
    ...
  }
}
```

每个 segment 的信息包含 binlog 路径和索引文件路径（如果已建完索引）。

当所有 NextTarget 中的 segment 都被加载到 QueryNode 后，NextTarget 提升为 CurrentTarget，
然后 TargetObserver 再从 DataCoord 拉取新一轮 NextTarget，如此循环。

代码：`internal/querycoordv2/observers/target_observer.go:384`，`internal/querycoordv2/meta/target_manager.go:139`

#### 4.8.3 SegmentChecker — 分配 segment 到 QueryNode

`SegmentChecker` 定期检查每个 collection 的每个 replica，对比期望与实际：

```
SegmentChecker.Check()
  │
  │  遍历所有已加载的 collection
  │  对每个 replica:
  │
  ├── getSealedSegmentDiff()
  │     对比 NextTarget 中的 segment 列表
  │     vs   当前已分布在 QueryNode 上的 segment
  │     → lacks: 需要加载的 segment 列表
  │     → redundancies: 需要释放的 segment 列表
  │
  ├── lacks → createSegmentLoadTasks()
  │     为每个缺失的 segment 生成 LoadSegments 任务
  │     通过 AssignPolicy 选择目标 QueryNode（默认 RoundRobin）
  │
  ├── redundancies → createSegmentReduceTasks()
  │     为多余的 segment 生成 Release 任务
  │
  └── getGrowingSegmentDiff()
        检查 Growing Segment 是否已被 Sealed Segment 替代
        → 已替代的 Growing Segment → 生成 Release 任务
```

代码：`internal/querycoordv2/checkers/segment_checker.go:103` `Check()`

#### 4.8.4 QueryNode 加载 Sealed Segment

QueryNode 收到 `LoadSegments` 请求后，执行实际加载：

```
QueryNode.LoadSegments(req)
  │
  │  req 包含:
  │    SegmentLoadInfo {
  │      SegmentID   = 7001
  │      BinlogPaths = [insert_log/.../7001/101/..., ...]  ← 各字段的 binlog
  │      IndexInfos  = [{IndexID=9001, IndexFilePaths=["abc123"], ...}]
  │      Deltalogs   = [delta_log/.../7001/...]
  │      NumOfRows   = 50000
  │    }
  │
  ▼
segmentLoader.Load()
  │
  ├── 1. NewSegment() — 创建 C++ Sealed Segment 对象
  │
  ├── 2. segment.Load(ctx) — 加载 binlog 数据
  │     C++ segcore 从对象存储读取各字段的 binlog 文件
  │     加载标量字段数据（用于过滤）
  │     加载向量索引文件（HNSW 图反序列化到内存）
  │
  │     数据变化:
  │     对象存储 binlog 文件 → C++ 内存中的列式数据
  │     对象存储 index 文件 → C++ 内存中的 HNSW 图
  │
  ├── 3. loadDeltalogs() — 加载删除日志
  │     应用删除标记，搜索时跳过已删除的行
  │
  └── 4. 注册到 SegmentManager
        segment 变为可查询状态
        后续搜索请求可以命中该 segment
```

代码：`internal/querynodev2/segments/segment_loader.go:244` `Load()`

#### 4.8.5 Growing Segment 释放

当 Sealed Segment 加载完成后，之前从 WAL 回放产生的 Growing Segment 数据与 Sealed Segment 重复。
`SegmentChecker.getGrowingSegmentDiff()` 检测到这些 Growing Segment 不再存在于 target 中，
生成 Release 任务将其释放，避免内存浪费和搜索结果重复。

```
加载前:
  QueryNode 内存:
    Growing Segment 7001 (WAL 回放, brute force 搜索)  ← 实时数据

加载后:
  QueryNode 内存:
    Sealed Segment 7001 (binlog + HNSW 索引)           ← 替代 Growing
    Growing Segment 7001                               ← 被释放
```

#### 4.8.6 全流程总结

```
LoadCollection (用户调用一次)
      │
      ▼
QueryCoord 开始持续监听
      │
      ├─── TargetObserver ───────────────────────────┐
      │    定期从 DataCoord 获取最新 segment 列表      │
      │    更新 NextTarget                            │
      │                                              │
      ├─── SegmentChecker ──────────────────────────┐│
      │    对比 NextTarget vs 当前分布               ││
      │    发现缺失 → 生成 Load 任务                 ││
      │    发现多余 → 生成 Release 任务              ││
      │                                              ││
      ▼                                              ││
QueryNode                                            ││
  ├── 下载 binlog + index 文件                       ││
  ├── 构建 Sealed Segment (C++)                      ││
  ├── 加载 HNSW 索引到内存                           ││
  ├── 段可查询 (搜索走 HNSW 索引)                    ││
  └── 释放对应的 Growing Segment                     ││
                                                     ││
      新 segment flush + 索引完成 ──────────────────►┘│
      TargetObserver 感知 → SegmentChecker 触发 ────►┘
      自动加载，无需用户再次调用
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
