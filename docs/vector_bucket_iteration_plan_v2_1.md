# Vector Bucket 方案与迭代建议 V2.1

## 0. 本版相对 V2 的修正

V2 的产品定位是对的，但核心技术路径里有两个前提过于乐观：

- 把 Milvus 当成了“**不需要 load，也能直接从 JuiceFS 做冷查询**”的引擎
- 把 `partition_key = bucket_id` 当成了“**bucket 级物理隔离**”机制

这两个前提在当前 Milvus 能力边界下都不成立，至少不能直接当作 Phase 1 的既定事实。

因此，V2.1 做如下修正：

- 保留 V2 的产品定位：这是**对象存储的向量扩展**，不是通用向量数据库
- 保留 V2 的接口目标：用户只看到 bucket 语义，不感知底层索引
- 放弃 V2 Phase 1 里“`IVF_SQ8` + 不 load + JuiceFS 直查”这一假设
- 放弃 V2 Phase 1 里“多桶共表 + partition_key 就能天然支撑万级桶冷查询”这一假设
- 把第一阶段收敛为：**基于当前 Milvus 能力的可用版**

V2.1 的核心思想是：

- 产品定位按 V2 走
- 实现路线按当前 Milvus 现实能力走

## 1. 产品定位

产品仍然定义为：

**私有云 HCI 平台上，对象存储服务的向量扩展功能**

而不是：

- Pinecone 这类托管向量数据库
- 通用在线毫秒级高性能向量检索服务

对外接口保持简洁：

- `CreateBucket / DeleteBucket`
- `PutVectors / UpsertVectors / DeleteVectors`
- `QueryVectors(topK, filter)`

设计原则：

- 用户按 bucket 理解资源
- 用户不应该感知底层使用 `HNSW / IVF_SQ8 / IVF_PQ / DISKANN`
- 产品可以按档位和后端演进，但接口不变

## 2. 当前资源与约束

当前已知条件：

- HCI 平台上一台虚拟机：`8 vCPU / 16 GB RAM`
- 对象存储服务与本产品混部
- JuiceFS 是既有存储底座
- 有一个非 JuiceFS 的高速本地目录可用
- 查询目标 `topK` 小，按 `1-30` 规划
- 第一阶段不做 Milvus 内核深改

这意味着：

- RAM 很紧
- page cache、对象存储服务、Milvus 会竞争资源
- 第一阶段不能把复杂度建立在“修改 Milvus 查询模型”之上

## 3. 当前 Milvus 的能力边界

### 3.1 查询前提：需要 `load`

当前 Milvus 不是一个“对象存储上索引文件按需读就能查”的系统。

在当前架构里：

- collection 不 `LoadCollection`
- 即使数据已经存在、索引已经建好
- 也不能直接搜索

因此，下面这个假设在当前阶段不能成立：

- “默认 `IVF_SQ8`，不 load，查询时直接从 JuiceFS 按需扫倒排桶”

这不是简单调参能得到的能力，而是查询执行模型层面的变化。

### 3.2 `partition_key` 不是 bucket 级物理隔离

`partition_key = bucket_id` 有价值，但它的价值主要是：

- 写入路由
- 查询表达式裁剪的潜力
- 后续结合 clustering compaction 时做 segment prune 优化

它并不等于：

- 每个 bucket 都有独立物理索引
- 每个 bucket 查询时只读自己的小块文件
- 多桶共表后冷桶天然零内存成本

尤其是在当前默认行为下：

- partition key 默认分区数有限
- bucket 之间仍可能共享 segment
- 如果没有额外 compaction / prune 策略，filter 不一定能把 I/O 压到足够小

因此，`partition_key` 在 V2.1 里应被视为**优化手段**，而不是 Phase 1 的核心承诺。

### 3.3 JuiceFS 是存储底座，不是现成冷查询引擎

JuiceFS 对当前方案当然有价值：

- 提供统一对象存储底座
- 提供本地 cache
- 便于统一存储和计费体系

但它不能自动把 Milvus 变成：

- 不 load 可查
- 真正冷桶 0 RAM 成本
- 随机按桶级小文件快速查询

所以 V2.1 对 JuiceFS 的定位是：

- **权威存储和缓存底座**
- 不是当前 Phase 1 的核心查询创新点

## 4. V2.1 推荐方案

### 4.1 总体策略

V2.1 推荐采取“**产品定位按 V2，Phase 1 实现按 V1 的简化路线**”：

- 产品仍然是 bucket 语义
- 但第一阶段实现不要尝试一步做成“对象存储原生冷查询引擎”
- 先用当前 Milvus 能力做一个真正可用的版本

### 4.2 Phase 1 推荐架构

```text
Client
  -> Bucket Gateway / API
  -> Metadata Service
  -> Bucket Router
  -> Milvus Adapter
  -> HNSW Backend
  -> Load/Release Controller
  -> LRU / TTL Cache Manager
```

实现原则：

- 用户操作 bucket
- 每个 bucket 在 Phase 1 映射到一个独立 collection
- 默认后端索引使用 `HNSW`
- 查询前按需 `load`
- 空闲 bucket 由 `TTL + LRU` 控制 `release`

这是当前阶段最重要的取舍：

- **用更简单的物理模型，换更快交付**

### 4.3 为什么 Phase 1 仍然推荐 `HNSW`

不是因为 `HNSW` 最终最便宜，而是因为在当前 Milvus 约束下，它最符合以下条件：

- 能快速做出可用版
- 最贴近当前想要的“首查慢，后续快”
- 对小 `topK` 的用户体验更稳
- `load/release` 语义清晰
- 不需要先引入共享冷档和复杂迁移逻辑

如果在当前 Milvus 能力下强行把默认后端切成 `IVF_SQ8`，但又无法做到“不 load 可查”，那第一阶段并不会比 `HNSW` 更简单，反而更容易变成：

- recall 一般
- 查询仍要 load
- 结构还更复杂

## 5. 为什么 V2 里的 `IVF_SQ8` 路线不能直接做 Phase 1

V2 的方向不是完全错，而是**时机不对**。

如果未来要做：

- 冷档共享表
- 默认 `IVF_SQ8`
- Bucket 级别自动升降档
- 大量冷桶挂在 JuiceFS 上

那至少要先满足以下条件之一：

### 路线 A：Milvus 内部能力增强

例如：

- 支持不 load 的查询模型
- 更强的 segment 级 lazy read
- 更清晰的冷索引执行路径

### 路线 B：重新定义冷档能力边界

例如：

- 冷档不是“0 RAM”
- 而是“共享加载的标准档”
- 通过 bucket group 而不是单 bucket 控制加载粒度

### 路线 C：换冷层执行方案

例如：

- 使用另一套更适合对象存储冷查询的引擎
- 或者深改 Milvus 的查询路径

Phase 1 明显不适合走这几条。

## 6. V2.1 的阶段性路线

### Phase 1：可用版 Preview

交付内容：

- Bucket API
- Metadata Service
- Bucket 到 collection 的映射
- `HNSW` 建索引
- `load/release`
- `LRU + TTL`
- 基础监控

明确不做：

- `IVF_SQ8` 冷档共享表
- bucket 自动毕业
- bucket 自动降级
- 多后端联合查询
- 复杂计费档位
- Milvus 深改

阶段目标：

- 尽快做出可用版
- 用户能按 bucket 写入、查询、删除
- 热点 bucket 后续查询明显变快
- 内存可控

### Phase 2：标准档能力

在 Phase 1 稳定后，再考虑引入“标准档”：

- 共享 collection
- `IVF_SQ8`
- bucket group 路由
- 可选 `partition_key = bucket_id`
- 可选 clustering compaction 和 prune

但这一阶段要明确：

- 这仍然不是“0 RAM 冷档”
- 而是“**共享加载的低成本标准档**”

Phase 2 的目标是：

- 降低大规模 bucket 的单位成本
- 提高 bucket 数量承载能力
- 为后续冷热分层打基础

### Phase 3：性能档或冷热分层

在已有标准档之后，再增加：

- `HNSW` 性能档
- 大 bucket 自动毕业
- 热档常驻
- 标准档和性能档之间离线迁移

这时的系统形态才会逐渐接近：

- 标准档 = `IVF_SQ8`
- 性能档 = `HNSW`

### Phase 4：研究路线

下面这些能力不应写入近期承诺，但可以保留研究方向：

- 真正的对象存储冷查询执行模型
- `DISKANN` 基础层
- 更深的 Milvus 原生多 profile 支持

V2 里把 `DISKANN` 彻底删掉，我不建议这么做。
更合理的处理是：

- 不进近期路线
- 但保留为研究备选项

## 7. 资源与容量估算

### 7.1 Phase 1 预算

当前节点：

- `8 vCPU / 16 GB RAM`
- 对象存储服务混部

建议按以下预算规划：

- Milvus 基础进程和查询开销：约 `2 GB`
- `HNSW` 热集合预算：约 `4 GB`
- buffer / page cache / 弹性余量：约 `2 GB`

### 7.2 `HNSW` 4 GB 热集合粗略规模

| 向量维度 | 粗略可承载热数据总量 |
| --- | --- |
| `768D float32` | `60 万 - 90 万` |
| `1024D float32` | `45 万 - 70 万` |
| `1536D float32` | `30 万 - 50 万` |

这是当前节点上**所有已加载 bucket 加起来**的大致量级，不是单 bucket。

### 7.3 Phase 1 的产品边界建议

Phase 1 应主动限制产品边界：

- bucket 总数做配额控制
- 活跃加载 bucket 数做硬控
- 单 bucket 向量数设上限
- 不承诺所有冷桶首次查询都在 sub-second 内完成

更适合把 Phase 1 定义为：

- 可用版
- 预览版
- 限额版

而不是一开始就承诺“海量冷桶 + 统一亚秒级”

## 8. 工作量评估

### 8.1 Phase 1

工作范围：

- bucket API
- metadata
- bucket -> collection 映射
- HNSW 索引管理
- load/release 控制
- LRU/TTL
- 基础监控

工作量预估：

- PoC：`1-2 周`
- 可用版：`2-4 周`
- 带基本稳定性和观测能力：`4-6 周`

### 8.2 Phase 2

工作范围：

- 标准档 collection 模型
- bucket group 路由
- `IVF_SQ8`
- clustering compaction / prune 验证
- 标准档迁移流程

工作量预估：

- 追加 `3-6 周`

### 8.3 Phase 3

工作范围：

- 自动毕业
- 标准档 <-> 性能档切换
- 热度统计
- 后台重建

工作量预估：

- 追加 `3-6 周`

### 8.4 研究路线

如果未来要追求：

- 真正冷查询
- 不 load 可查
- 更接近 S3 Vectors 的对象存储原生执行模型

那将是研究级工作，不应放进近期承诺。

## 9. 风险

Phase 1 风险：

- 冷 bucket 首查延迟高
- collection 数量增长会带来元数据和调度成本
- 混部下对象存储服务与 Milvus 会争抢资源

Phase 2 风险：

- 标准档共享 collection 的隔离性和 prune 效果未必足够好
- `partition_key` 和 clustering compaction 的收益需要基准测试验证
- bucket group 粒度一旦选错，可能导致 load 单位过粗

Phase 3 风险：

- 升降档会引入迁移抖动
- 热档预算容易被少数大 bucket 吃光
- 自动化策略可能比预想中更复杂

## 10. 最终建议

V2 的产品定位我认可：

- 它比 V1 更接近真正的产品方向
- 它更像“对象存储的向量扩展”，而不是“小型向量数据库”

但 V2 的技术路径不应直接照搬到 Phase 1。

V2.1 的建议是：

- 产品定位继续按 V2 走
- Phase 1 实现按当前 Milvus 能力边界走
- 第一版采用 `HNSW + load/release + LRU`
- `IVF_SQ8` 留到 Phase 2 作为标准档，而不是 Phase 1 默认冷档
- `DISKANN` 不进入近期路线，但不应该从研究路线里彻底删除

一句话总结：

**V2 的方向是对的，V2 的第一阶段实现路径不够落地；V2.1 应该保留产品视角，但把技术路线修正为“先做能跑的，再做更像 S3 Vectors 的”。**

