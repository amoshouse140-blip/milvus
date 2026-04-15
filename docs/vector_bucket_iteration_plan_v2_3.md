# Vector Bucket 方案设计 V2.3

## 1. 产品定位

**私有云 HCI 平台上，对象存储服务的向量扩展功能。**

不对标 Pinecone 这类托管向量数据库，不对标通用毫秒级实时向量检索服务。

对外资源模型：

- **bucket**：资源容器、权限、配额、计费边界
- **logical collection**（在某些云厂商语义下也叫 index / index table）：bucket 下真正的写入和查询单元

对外接口：

- `CreateBucket / DeleteBucket`
- `CreateCollection(bucket, name, dim, metric)` / `DeleteCollection`
- `PutVectors(bucket, collection, ...)`
- `UpsertVectors(bucket, collection, ...)`
- `DeleteVectors(bucket, collection, ...)`
- `QueryVectors(bucket, collection, topK, filter)`

设计原则：

- 用户按 `bucket -> logical collection` 理解资源
- 用户不感知底层索引类型
- 产品可以演进后端和档位，接口不变

## 2. 资源与约束

- HCI 平台一台虚拟机：`8 vCPU / 16 GB RAM`
- 对象存储服务与本产品混部
- Milvus 底座对象存储由 JuiceFS 提供（挂载目录形态）
- 一块非 JuiceFS 的本地高速盘
- 查询目标 `topK` 小，按 `1-30` 规划
- 向量存储层可用 RAM 约 `8 GB`
- 不做 Milvus 内核深改

## 3. 当前 Milvus 的能力边界

### 3.1 查询前必须 `LoadCollection`

collection 未 load 即使数据和索引都存在，也不能搜索。Phase 1 必须围绕 load/release 设计。

### 3.2 `load` 不等于"索引全进 RAM"

Milvus 支持 mmap，开启后 `load` 更接近"建立映射 + 借助 page cache / chunk cache"，常驻 RAM 由访问模式决定。相关配置：

- `queryNode.mmap.mmapEnabled`（旧总开关）
- `queryNode.mmap.vectorField`（默认 `true`）
- `queryNode.mmap.vectorIndex`（默认 `false`，**必须显式打开**）
- collection / index 级 `mmap.enabled` 属性

不同索引类型在 mmap 模式下 RAM 行为差异很大：HNSW 图结构访问密集，常驻接近全量；IVF 系只访问 nprobe 个倒排桶，常驻显著偏低。

### 3.3 Milvus 底座对象存储的定位

"Milvus 底座对象存储（由 JuiceFS 提供）"逻辑角色是 Milvus 主持久化存储，部署形态是 JuiceFS 挂载目录。它不是冷查询执行引擎，不能让 Milvus 绕过 load 直接对它做检索。

## 4. 整体架构

```text
Client
  -> Vector Bucket Gateway (REST / gRPC)
  -> Metadata Service           (bucket / logical collection / 配额 / 访问统计)
  -> Namespace Router           (bucket + logical collection -> 物理 Milvus collection)
  -> Load/Release Controller    (LRU + TTL, 预算内 load/release)
  -> Milvus Adapter
  -> Milvus Standalone (嵌入同一 VM)
       底座对象存储 = JuiceFS 挂载目录
       本地高速盘   = mmap 文件 + chunk cache
```

索引档位规划：

| 档位 | 索引 | load 策略 | 用途 | 引入阶段 |
| --- | --- | --- | --- | --- |
| 标准档 | `IVF_SQ8 + mmap` | 按需 load + LRU/TTL | 默认，承载所有 logical collection | Phase 1 |
| 性能档 | `HNSW` | load 常驻 | 大 / 热 logical collection 毕业使用 | Phase 2 |

## 5. 部署形态

Phase 1 推荐的挂载目录约定（宿主 → 容器）：

| 宿主路径 | 容器路径 | 用途 |
| --- | --- | --- |
| `/mnt/jfs/milvus-root` | `/var/lib/milvus-data` | Milvus 底座对象存储（JuiceFS） |
| `/mnt/localssd/mmap` | `/var/lib/milvus-mmap` | vector index / field mmap 本地文件 |
| `/mnt/localssd/chunk-cache` | `/var/lib/milvus-cache` | chunk cache / 本地读缓存 |

Milvus 关键配置（以实际版本为准）：

```
queryNode.mmap.vectorField = true
queryNode.mmap.vectorIndex = true   # 必须显式改
queryNode.mmap.mmapDir     = /var/lib/milvus-mmap
queryNode.cacheSize        = 根据可用 RAM 和 chunk cache 路径容量设置
localStorage.path          = /var/lib/milvus-cache
```

## 6. Phase 1：单 Collection 可用版

### 6.1 物理模型

- **一个 logical collection 映射到一个物理 Milvus collection**
- 物理 collection 命名：`vb_{bucket_id}_{logical_collection_id}`
- shards = 1（单节点场景）

Schema：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | VARCHAR(64) | 主键，用户侧 vector id |
| `vector` | FLOAT_VECTOR(dim) | 创建 logical collection 时指定 dim |
| `metadata` | JSON | 用户自定义，支持 filter |
| `created_at` | INT64 | 毫秒时间戳 |

索引参数（起步值，以 benchmark 结果收敛）：

```
index_type   = IVF_SQ8
metric_type  = COSINE | L2 (创建时指定)
nlist        = clamp(sqrt(N) * 4, 1024, 65536)
nprobe       = 16 (查询默认，可按请求 override)
mmap.enabled = true  (index 级)
```

### 6.2 生命周期

**创建 bucket**
- Metadata 写入 bucket 记录（id、owner、quota、status=READY）
- 不创建任何 Milvus collection

**创建 logical collection**
1. Metadata 写入记录（id、parent bucket、dim、metric、status=INIT）
2. Milvus Adapter：`CreateCollection(vb_{bucket}_{lc}, schema)`，不建索引
3. Metadata 更新 status=READY

**写入 / Upsert / 删除向量**
1. Gateway 鉴权 + 配额检查
2. Namespace Router 查 Metadata 得物理 collection 名
3. Milvus Adapter 调 `Insert / Upsert / Delete`
4. 向量累积首次达到阈值（默认 1 万）→ 触发异步建索引 job
5. 建索引完成后 Metadata 记 `index_built=true`

**查询向量**
1. Gateway 鉴权 + 限流
2. Namespace Router 查 Metadata
3. Load/Release Controller 检查是否 loaded：
   - 已 loaded：直接查
   - 未 loaded：取 load 锁，`LoadCollection` 后查（首查延迟含 load 成本）
4. Milvus Adapter 调 `Search(topK, nprobe, filter)`
5. Controller 更新 `last_access_at`

**删除 logical collection**
1. Metadata 状态 DELETING
2. 异步 `ReleaseCollection + DropCollection`
3. 状态 DELETED（保留记录供计费核对）

**删除 bucket**
- 拒绝删除非空 bucket；或级联删除所有 logical collection 后再删 bucket

### 6.3 Load/Release Controller

**数据结构**

```
loaded_set  : OrderedDict[collection_name -> LoadEntry]   # LRU, 尾部为最近访问
LoadEntry   : { loaded_at, last_access_at, est_mem_mb, in_flight_queries }
budget_mb   : 4096   # 活跃 load 总预算
ttl         : 30 min
```

**内存预估**（用于预算决策）

```
est_mem_mb(N, dim) = N * dim * 1B * overhead_ratio   # IVF_SQ8
overhead_ratio ≈ 1.2 (含 nlist 中心点、倒排索引开销)
```

**load 触发路径**
1. 命中未 loaded 的 collection
2. 取该 collection 的 load 互斥锁
3. 检查预算：`sum(loaded.est_mem) + est > budget_mb`
   - 是：按 LRU 顺序 release 最久未访问且 `in_flight == 0` 的 collection，直到够
   - 全部 loaded 都有 in-flight 查询：返回 503 + Retry-After
4. 调 `LoadCollection`（同步等待完成）
5. 登记 `LoadEntry`，释放锁

**release 触发路径**
- TTL：后台 job 每 60s 扫描，`now - last_access_at > ttl` 且 `in_flight == 0` → release
- LRU：load 需要腾预算时触发

**并发约束**
- 同 collection 的 load 全局互斥
- release 前检查 `in_flight_queries`
- 查询进入时 `in_flight++`，返回时 `in_flight--`

### 6.4 API 定义（HTTP 示意）

```
POST   /v1/buckets                                    {name}
DELETE /v1/buckets/{bucket}
GET    /v1/buckets/{bucket}

POST   /v1/buckets/{bucket}/collections               {name, dim, metric}
DELETE /v1/buckets/{bucket}/collections/{collection}

POST   /v1/buckets/{bucket}/collections/{collection}/vectors            [{id, vector, metadata}, ...]
POST   /v1/buckets/{bucket}/collections/{collection}/vectors:upsert     同上
POST   /v1/buckets/{bucket}/collections/{collection}/vectors:delete     {ids:[...]} | {filter:"..."}

POST   /v1/buckets/{bucket}/collections/{collection}/query
       {vector, topK, filter?, nprobe?}
       -> [{id, score, metadata}, ...]
```

错误语义：

| 情况 | 状态码 |
| --- | --- |
| bucket / collection 不存在 | 404 |
| 向量数不足最小索引阈值，查询退化为暴力扫描 | 200 |
| 超配额 | 429 |
| load 超时或预算耗尽 | 503 + `Retry-After` |
| 维度 / metric 不匹配 | 400 |

### 6.5 配额与硬限

| 项 | 限制 |
| --- | --- |
| 单租户 bucket 数 | 100 |
| 全局 bucket 数 | 1000 |
| 单 bucket 下 logical collection 数 | 50 |
| 单 logical collection 向量数 | 100 万 |
| 维度 | ≤ 1536 |
| 活跃 loaded collection 数 | 动态按预算，硬上限 50 |
| 写入 QPS（单 collection） | 500/s |
| 查询 QPS（单 collection） | 50/s |

### 6.6 监控指标

- `vb_bucket_count`, `vb_logical_collection_count{status=*}`
- `vb_loaded_collection_count`
- `vb_load_duration_seconds`（直方图）
- `vb_query_duration_seconds{phase=load|search}`
- `vb_release_evictions_total{reason=ttl|lru}`
- `vb_collection_mem_estimate_mb`
- `vb_query_recall`（离线 benchmark 任务采集）
- JuiceFS cache 命中率、chunk cache 命中率、底座对象存储 IO 吞吐

### 6.7 验收标准

- recall@10 ≥ 0.9（标准 benchmark 数据集）
- 已 loaded 查询 p99 ≤ 500 ms
- 冷查询 p99 ≤ 5 s（含 load，50 万向量 bucket）
- `LoadCollection` p95 ≤ 3 s
- 4 GB 预算下稳定 load ≥ 30 个活跃 collection（每个 10 万向量 @768D）

未达标触发兜底：回退 HNSW 或下调产品承诺。

## 7. Phase 2：性能档分层版

### 7.1 要解决的问题

标准档 IVF_SQ8 延迟 sub-second 够用，但少数大 / 热 logical collection 需要 ms 级延迟和更高 recall。做法是让这些 logical collection **毕业到性能档（HNSW + load 常驻）**。

### 7.2 物理模型扩展

新增性能档 collection：

| 档位 | collection 命名 | 索引 | load |
| --- | --- | --- | --- |
| 标准档 | `vb_{bucket}_{lc}` | `IVF_SQ8 + mmap` | 按需 load + LRU/TTL |
| 性能档 | `vbh_{bucket}_{lc}` | `HNSW(M=16, efConstruction=200)` | **常驻**，不受 LRU 回收 |

**RAM 预算再切**（8 GB 总预算）：

- Milvus 基础 + 查询 working set：2 GB
- 标准档活跃 load 预算：3 GB
- 性能档 HNSW 常驻：3 GB

### 7.3 访问统计

Metadata 新增：

- `tier ∈ {standard, performance}`
- `qps_1h_avg`, `qps_7d_avg`, `qps_1d_peak`
- `vector_count`, `last_query_at`
- `last_tier_change_at`

由 Controller 和后台 job 维护。

### 7.4 毕业与降级规则

**自动毕业**（每小时评估）：

```
候选条件（任一）：
  - vector_count >= 50 万 AND qps_7d_avg >= 1
  - qps_1d_peak >= 10
  - 用户显式付费升档

准入门槛（全部）：
  - 性能档剩余预算 >= est_hnsw_mem(lc)
  - now - last_tier_change_at >= 24h（防抖）
```

**自动降级**（每小时评估）：

```
条件（全部）：
  - qps_7d_avg < 0.1
  - now - last_query_at > 3d
  - 性能档预算占用 >= 90%
  - now - last_tier_change_at >= 24h
```

**est_hnsw_mem**：`vector_count * dim * 4B * 1.5`（原始向量 + 图结构开销）。

### 7.5 毕业流程

1. Metadata 标记 `migrate_state = upgrading`
2. `CreateCollection(vbh_{b}_{lc}, schema)`
3. 从标准档抽取该 logical collection 数据（scan + batch）
4. 批量写入性能档 collection
5. 建 HNSW 索引（异步，耗时可达几分钟）
6. 索引完成后 `LoadCollection` 并常驻
7. 双写阶段：写入同时落标准档和性能档，查询仍走标准档
8. 数据校验（count、抽样 topK 对比）
9. 切换路由 `tier = performance`；查询走 HNSW
10. 双写保留 24h 后 `DropCollection` 标准档

降级是反向操作：先复制回标准档 → 建 IVF_SQ8 索引 → 切路由 → drop HNSW collection。

### 7.6 双写期一致性

- 写入：双写任一失败 → 整体失败（客户端重试）
- 查询：毕业期间优先读标准档（权威源）
- 删除：双写删除；若某一侧失败，后台对账 job 补偿

### 7.7 Phase 2 验收

- 性能档 logical collection 查询 p99 ≤ 20 ms
- 毕业 / 降级期间查询可用性 100%
- 毕业期间延迟劣化 ≤ 50%
- 性能档总 logical collection 数上限 10-20（按 3 GB 预算 + 典型规模反推）
- 24h 内同一 logical collection 不发生 ≥ 2 次 tier 变化

## 8. Phase 3：研究方向（不进入近期承诺）

- Milvus 原生"不 load 可查"执行模型（对象存储原生冷查询）
- DISKANN 基础层
- Milvus 原生多 profile（同 collection 多索引并存）

## 9. 容量估算（规划目标，待 benchmark 验证，不对外承诺）

### 9.1 标准档（`IVF_SQ8 + mmap`，4 GB 预算）

| 维度 | 活跃加载总量 |
| --- | --- |
| `768D` | 200 万 - 350 万 |
| `1024D` | 150 万 - 250 万 |
| `1536D` | 100 万 - 180 万 |

### 9.2 性能档（HNSW，3 GB 预算）

| 维度 | 性能档总向量数 |
| --- | --- |
| `768D` | 35 万 - 50 万 |
| `1024D` | 25 万 - 40 万 |
| `1536D` | 18 万 - 28 万 |

## 10. 工作量

| 阶段 | 范围 | 预估 |
| --- | --- | --- |
| Phase 1 PoC | bucket + logical collection API、metadata、Milvus 适配、基本 load/release | 1-2 周 |
| Phase 1 可用版 | + LRU/TTL + 配额 + 基础监控 | 3-5 周 |
| Phase 1 稳定版 | + benchmark + 参数调优 + 异常处理 | 4-6 周 |
| Phase 2 | 性能档 + 毕业 / 降级 + 双写一致性 | 4-6 周 |

## 11. 风险

**Phase 1**
- 冷 logical collection 首查延迟含 `LoadCollection`
- `IVF_SQ8` 对异常数据 recall 可能偏低
- mmap 与底座对象存储本地 cache 的交互需实测
- logical collection 数量增长导致 meta 开销（到千级需要重新评估）

**Phase 2**
- 毕业 / 降级在边界 logical collection 上抖动，需防抖窗口
- 性能档预算被少数超大 logical collection 吃光
- 双写期一致性实现错误会导致数据漂移
