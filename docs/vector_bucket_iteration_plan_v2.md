# Vector Bucket 方案与迭代建议 V2

## 0. 本版相对 V1 的核心调整

V1 文档把产品当作"通用向量检索服务"来设计，默认后端选 `HNSW + LRU load/release`，并把 `DISKANN` 和冷热双层架构作为长期目标。

V2 基于对产品形态和基础设施的重新梳理，调整主架构。关键变化如下：

- 产品定位明确为 **对象存储的向量扩展功能**，对标 AWS S3 Vectors，而不是对标 Pinecone / Milvus 托管服务。
- 存储底座明确为 **JuiceFS**，不再把它当成"可选目录"。
- 默认索引从 `HNSW` 改为 **`IVF_SQ8`**，并明确**默认不 load**。
- 冷热分层的粒度从"collection 级 LRU"下沉到"**冷档共享表 + 热档独占表**"两档共存。
- 桶与物理存储的映射从"一桶一 collection"改为"**多桶共表 + `partition_key` 路由**"。
- 彻底放弃 `DISKANN` 路线，省掉原方案 V3/V4 的全部多后端复杂度。

调整原因在后续章节展开。

## 1. 产品定位

产品是 **私有云 HCI 平台上，对象存储服务的向量扩展功能**。

产品形态对标 AWS S3 Vectors：

- 按**桶**组织，一个租户可开大量 vector bucket
- 按存储量和请求量计费
- **不承诺 ms 级延迟**，承诺 sub-second
- **不承诺高 recall**，面向成本敏感、长尾访问的场景
- 与实时向量数据库是互补关系，不是替代关系

核心卖点：

- 客户已经在用对象存储，追加一个向量能力即可用
- 向量索引文件和业务文件躺在同一个底层存储，计费统一
- 冷桶成本趋近于 0

对外接口保持简单：

- `PutVectors / UpsertVectors / DeleteVectors`
- `QueryVectors(topK, filter)`

用户不应感知底层是 `HNSW / IVF_SQ8 / IVF_PQ`。

## 2. 基础设施与资源约束

- HCI 平台上一台虚拟机：`8 vCPU / 16 GB RAM`
- 该 VM 内已运行 JuiceFS，提供对象存储 bucket
- 有一块**高速盘**，作为独立文件存储路径
- 对象存储服务与本产品**混部**在同一台 VM
- 查询目标 `topK` 小，按 `1-30` 规划
- 向量存储层 **RAM 预算约 8 GB**（剩余给 JuiceFS、对象存储服务、OS page cache）
- 第一阶段不修改 Milvus 内核

这些约束决定了：

- 不能依赖"本地 SSD 随机读"型索引（`DISKANN`）
- 可以有"少量热桶常驻内存"的预算（8 GB 够 80-100 万向量 @768D HNSW）
- 但冷桶必须是"0 RAM 成本"，否则海量桶扛不住

## 3. 为什么默认索引是 IVF，而不是 HNSW

这是 V2 相对 V1 最核心的结论。

### 3.1 产品定位决定算法选型

向量桶产品和向量数据库的差别不是规模差别，是**工作集差别**：

- 向量数据库：工作集小，热数据占比高，值得整体常驻内存
- 向量桶：海量桶、长尾分布，绝大多数桶常年不访问，**冷数据必须真的冷**

在这个差别下，各类主流 ANN 算法的适配度如下：

| 特性 | HNSW | IVF (含 SQ8/PQ) | DISKANN |
| --- | --- | --- | --- |
| 索引文件能否完全放对象存储 | 勉强 | **能** | 能但要求低延迟随机读 |
| 查询是否需要全索引进内存 | 需要 | **不需要** | 需要快速随机读 |
| 冷启动成本 | 高（秒级到十几秒 load） | **极低**（按需读 nprobe 个桶） | 中 |
| 对混部 + JuiceFS 环境友好度 | 低 | **高** | 极低 |
| recall 上限 | 高 | 中 | 高 |
| 延迟 | ms 级 | 百 ms 级 | 十 ms 级 |

IVF 是**唯一一种在算法层面就对"对象存储 + 按需访问 + 海量冷桶"友好**的主流 ANN。查询路径是"算 query 落在哪几个倒排桶 → 从 JuiceFS 读这几个桶文件 → 桶内算距离"，每次查询只读几 MB。

HNSW 做不到这一点：HNSW 是图，查询要随机跳转几百次，每跳走对象存储就是灾难，所以所有基于 HNSW 的产品都必须 load 进内存。

### 3.2 8 GB RAM 给 HNSW 不够用

按 `768D float32`，HNSW 实测内存 ≈ 原始向量 × 1.5：

- 一条 ≈ 4.5 KB
- 8 GB / 4.5 KB ≈ **180 万条**

这是整个产品所有桶加起来的上限。对一个面向"海量桶 + 长尾分布"的产品，180 万条分到几千个桶里每桶几百条，不够支撑一个能卖的产品。

IVF_SQ8 不需要 load 进内存，8 GB 全部用作 JuiceFS cache + 查询 working set，**可检索总量是几千万到上亿**，受存储带宽约束，不受 RAM 约束。

### 3.3 V1 对 IVF 的评估在新语境下被推翻

V1 第 4.3 节把 IVF_PQ 评为"不建议作为第一版默认"，理由是"recall 上限较低、参数敏感"。这个判断在"做通用向量数据库"的语境下成立，但在"做向量桶"的语境下**恰好反过来**——向量桶本来就不承诺高 recall 和低延迟，选 HNSW 反而是付了不该付的 RAM 成本。

## 4. 推荐架构

```text
Client (S3-ish Vector API)
  -> Bucket Gateway      (认证 / 配额 / 计费, 复用对象存储租户体系)
  -> Bucket Router       (bucket_id -> physical collection + partition_key)
  -> Metadata Service
  -> Milvus Standalone (嵌入同一台 VM)
       存储根    = JuiceFS
       chunk cache = 高速盘 (固定预算, e.g. 20 GB)
       ├── cold_shared_*    IVF_SQ8, 不 load      <- 默认, 承载绝大多数桶
       └── hot_dedicated_*  HNSW,    load 常驻    <- 少量大桶 / 付费桶
```

RAM 预算 8 GB 划分：

- Milvus 进程 + 查询 working set：~2 GB
- 热档 HNSW 常驻：~4 GB（≈ 80-100 万向量 @768D）
- 弹性 buffer：~2 GB

### 4.1 关键设计决策

**1) 默认多桶共表，而非一桶一 collection**

Milvus 的 collection 是重对象（meta、segment、channel、load 状态），collection 数量本身就是成本。海量桶必须共享 collection，用 `partition_key = bucket_id` 做路由和隔离。

- 冷桶的存在成本 = 几行 meta + JuiceFS 上的几个 parquet 文件
- 桶数量上限从 Milvus collection 配额（千级）提升到 JuiceFS 对象数量上限（万级以上）

**2) 冷热分层在"桶到 collection 的映射"这一层，不在 Milvus 内部**

- 冷档 `cold_shared_*`：IVF_SQ8，**不 load**，全部躺 JuiceFS，查询按需读 segment
- 热档 `hot_dedicated_*`：HNSW，load 常驻，给毕业的大桶/付费桶
- 两档在同一个 Milvus 实例共存，通过 collection 粒度区分

**3) JuiceFS 是底座，高速盘是 cache**

- Milvus 存储根挂 JuiceFS，冷数据天然躺对象存储，成本趋近 0
- 高速盘作为 **Milvus chunk cache / JuiceFS 本地 cache**，固定预算
- 热桶的 IVF 倒排桶被读过一次后留在高速盘，下次查询命中
- **不需要自己写向量数据的 LRU**——JuiceFS cache + OS page cache 已经是 LRU

**4) 单索引家族，必要时切量化档位**

- 冷档默认 IVF_SQ8；超冷档（更便宜）可选 IVF_PQ
- 热档 HNSW
- 不引入 DISKANN
- 档位切换走离线重建，对用户透明

## 5. 演进路径

### Phase 1 — 冷档端到端（3-5 周）

交付内容：

- S3 风格 Vector Bucket API（create/delete bucket、put/query/delete vectors）
- Metadata Service（桶、配额、路由规则）
- Bucket Gateway 与对象存储已有租户/计费体系打通
- Milvus standalone 嵌入部署，存储根 = JuiceFS，chunk cache = 高速盘
- 一张 `cold_shared_default` collection，IVF_SQ8，`partition_key = bucket_id`
- 所有桶默认路由到冷档，**不 load**
- 基础监控指标

明确不交付：

- 热档 / HNSW
- 毕业机制
- 性能档位暴露
- DISKANN / IVF_PQ

阶段目标：

- 能卖：客户能创建桶、写入、查询、删除，计费正确
- 延迟 sub-second，recall 中等
- 桶数量扩展性到万级

### Phase 2 — 引入热档 + 自动毕业（+3-4 周）

交付内容：

- `hot_dedicated_*` collection 模板，HNSW + load
- 访问统计（QPS、向量数、最近访问时间）
- 后台 job：满足条件的桶离线重建索引 → 迁移到 `hot_*` → load
- 冷却的热桶降级回冷档
- **8 GB 预算硬控**：热档总量超阈值则拒绝新毕业
- 毕业对用户透明

阶段目标：

- 大桶 / 热桶查询从 sub-second 降到 ms 级
- 产品有性能梯度

### Phase 3 — 档位化计费（按客户反馈，可选）

交付内容：

- 显式暴露 `economy (IVF_PQ) / standard (IVF_SQ8) / performance (HNSW)` 档位
- 从"系统自动毕业"演进为"客户付费升档"
- 档位切换 = 离线重建 + collection 迁移

阶段目标：

- 差异化定价

### 明确不做

- ❌ DISKANN
- ❌ HNSW 作默认索引
- ❌ 一桶一 collection
- ❌ 自写向量数据 LRU load/release 编排
- ❌ 双后端联合查询 / 热层冷层在线 merge
- ❌ Milvus 内核深改

## 6. 容量粗略估算

### 6.1 冷档（IVF_SQ8, 不 load, JuiceFS）

受存储成本和查询带宽约束，不受 RAM 约束。

| 向量维度 | 粗略可承载向量总量 |
| --- | --- |
| `768D` | 千万级以上 |
| `1024D` | 千万级 |
| `1536D` | 百万到千万级 |

冷查询延迟主要取决于 JuiceFS 对 nprobe 个倒排桶文件的读取能力，预期 100 ms - 1 s 级。

### 6.2 热档（HNSW, load 常驻, 8 GB 预算）

| 向量维度 | 粗略热桶向量总量 |
| --- | --- |
| `768D` | 80 - 100 万 |
| `1024D` | 60 - 80 万 |
| `1536D` | 40 - 50 万 |

这是所有热桶加起来的上限，毕业准入要按此反推。

## 7. 风险

Phase 1 风险：

- JuiceFS 对 IVF 倒排桶的随机读 QPS 是产品延迟底线，需要早期压测
- 混部下对象存储服务与 Milvus 查询 IO 争抢尾延迟
- `partition_key` 路由在 Milvus 当前版本的查询计划稳定性需要验证
- 计费/配额与对象存储已有体系打通的工作量可能被低估

Phase 2 风险：

- 毕业抖动：桶在冷热之间反复迁移成本高，需要滞后阈值
- 热档 8 GB 预算被少数超大桶吃光，其他本应毕业的桶挤不进来
- HNSW load 耗时在请求路径上暴露，需要 warmup 语义

## 8. 最终建议

- 产品定位对齐 S3 Vectors：**对象存储的向量扩展，不是向量数据库**
- 架构让 **JuiceFS 做主角、IVF_SQ8 做默认索引、Milvus 只当一个不 load 的查询引擎**
- 高速盘是 JuiceFS / Milvus chunk cache，不是独立索引层
- 热档（HNSW + load）作为 Phase 2 引入的性能梯度，不是 Phase 1 的默认
- 完全不碰 DISKANN 和 Milvus 内核

这条路线在以下维度最均衡：

- 与产品形态（向量桶）自洽
- 与基础设施（HCI + JuiceFS + 8C16G + 混部）自洽
- 与交付节奏（先能卖，再加性能梯度）自洽
- 改动量最小：Milvus 零内核改动，用的全是原生能力

一句话总结：

- V1 是 "把 Milvus 当向量数据库用"
- V2 是 "把 Milvus 当向量桶的查询引擎用，让 JuiceFS 和 IVF 承担主要职责"
