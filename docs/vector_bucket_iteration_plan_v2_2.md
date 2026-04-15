# Vector Bucket 方案与迭代建议 V2.2

## 0. 本版相对 V2.1 的修正

V2.1 把产品定位和实现路径拆开是对的，但在 Phase 1 的默认索引选型上回退过头：直接选了 `HNSW`。

V2.2 做如下微调：

- 保留 V2.1 的分层思路：产品定位按 V2 走，实现路径按当前 Milvus 能力边界走
- 保留 V2.1 的阶段划分节奏
- 保留 V2.1 对 DISKANN 的处理（研究路线保留，近期不承诺）
- **修正 Phase 1 默认索引**：从 `HNSW` 改为 **`IVF_SQ8 + mmap + load/release + LRU`**
- **澄清一个关键技术事实**：Milvus 必须 `LoadCollection` 才能查询，但 "load" 的 RAM 成本取决于索引类型和是否开启 mmap，**不等于"整个索引载入 RAM"**

V2.2 的核心判断是：

- V2.1 担心的"IVF 不 load 可查"确实当前做不到
- 但 V2.1 因此跳过了中间档"**IVF + load + mmap + LRU**"这个组合
- 这个中间档在实现复杂度上和 HNSW 方案**完全一样**（都走 load/release/LRU），RAM 占用低一个量级，还能避免 Phase 2 再做一次索引迁移

## 1. 产品定位

和 V2.1 保持一致：

**私有云 HCI 平台上，对象存储服务的向量扩展功能**。

对外接口：

- `CreateBucket / DeleteBucket`
- `PutVectors / UpsertVectors / DeleteVectors`
- `QueryVectors(topK, filter)`

设计原则：

- 用户按 bucket 理解资源
- 用户不感知底层索引类型
- 产品可以按档位和后端演进，接口不变

## 2. 当前资源与约束

- HCI 平台上一台虚拟机：`8 vCPU / 16 GB RAM`
- 对象存储服务与本产品混部
- JuiceFS 是既有存储底座
- 有一块非 JuiceFS 的高速本地盘
- 查询目标 `topK` 小，按 `1-30` 规划
- 第一阶段不做 Milvus 内核深改
- 向量存储层 RAM 预算约 8 GB

## 3. 当前 Milvus 的能力边界（对 V2.1 的校正）

### 3.1 查询前提：需要 `LoadCollection`

当前 Milvus 不是"对象存储上索引文件按需读就能查"的系统。collection 必须先 `LoadCollection` 才能查询。

这条是硬约束。V2 里"IVF_SQ8 不 load 可直接从 JuiceFS 查询"的假设**不成立**。

### 3.2 但 "load" 的 RAM 成本取决于索引类型和 mmap 配置

这是 V2.1 遗漏、V2.2 要补上的关键事实：

- Milvus 支持 `queryNode.mmap.mmapEnabled = true`，索引文件通过 mmap + chunk cache 管理
- 在 mmap 模式下，load 的语义是"把索引文件映射到虚拟地址空间 + 建立 chunk cache 映射"，**不是把索引全量读进 RAM**
- 实际常驻 RAM 的部分由 OS page cache 按访问模式决定
- 不同索引类型在 mmap 模式下的 RAM 行为差异很大：
  - `HNSW`：图结构访问随机且密集，热点图节点很容易把 page cache 撑满，实际 RAM 占用接近全量
  - `IVF_SQ8`：查询只访问少量倒排桶，大部分桶文件可以不常驻，RAM 占用显著低于 HNSW
  - `IVF_PQ`：同上，压缩度更高

因此 V2.1 里"HNSW 最省事、IVF_SQ8 要等 Phase 2"的默认前提应被修正为：

- **都需要 load**
- **但 IVF_SQ8 + mmap 的 load 在 RAM 成本上显著优于 HNSW + mmap**

### 3.3 `partition_key` 的定位

和 V2.1 一致：`partition_key = bucket_id` 有价值，但它不是"bucket 级物理隔离"的银弹。

补充一点：Milvus 2.3+ 的 `partition_key` + `clustering compaction` 组合在启用后，segment 会按 partition_key 聚簇，filter 能 prune 到少量 segment。**这个能力当前版本就有，不是未来能力。**

V2.2 里 `partition_key` 的处理：

- Phase 1 不强依赖，保持一 bucket 一 collection 的简单模型
- Phase 2 作为"多桶共表"落地时才启用

### 3.4 JuiceFS 的定位

和 V2.1 一致：存储底座，不是现成的冷查询引擎。

## 4. V2.2 推荐方案

### 4.1 总体策略

- 产品定位按 V2 走
- Phase 1 实现按当前 Milvus 能力边界走
- **Phase 1 默认索引选 `IVF_SQ8 + mmap`，不选 HNSW**
- load/release/LRU 框架照搬 V2.1 的设计，不受索引类型影响

### 4.2 Phase 1 推荐架构

```text
Client
  -> Bucket Gateway / API
  -> Metadata Service
  -> Bucket Router           (Phase 1: bucket -> 独立 collection)
  -> Milvus Adapter
  -> IVF_SQ8 Backend (mmap)
  -> Load/Release Controller
  -> LRU / TTL Cache Manager

存储根   = JuiceFS
chunk cache = 高速本地盘 (固定预算, e.g. 20 GB)
mmap    = 开启
```

实现原则：

- 用户操作 bucket
- 每个 bucket 映射到一个独立 collection（Phase 1 保持简单）
- 默认后端索引 `IVF_SQ8`，开启 mmap
- 查询前按需 `LoadCollection`
- 空闲 bucket 由 `TTL + LRU` 触发 `ReleaseCollection`

### 4.3 为什么 Phase 1 默认选 `IVF_SQ8 + mmap` 而不是 HNSW

| 对比项 | HNSW (V2.1) | IVF_SQ8 + mmap (V2.2) |
| --- | --- | --- |
| Phase 1 是否可跑 | ✅ | ✅（Milvus 原生） |
| load/release/LRU 复杂度 | 需要 | **完全相同** |
| 60 万 @768D 常驻 RAM | `~2-3 GB` | `~500 MB - 1 GB` |
| 冷查询延迟 (load 后) | `10-50 ms` | `100-500 ms` |
| recall | 高 | 中（对 topK ≤ 30 够用） |
| 和产品定位匹配度 | 偏实时向量库 | 偏向量桶（sub-second OK） |
| Phase 2 是否需要重建索引 | **需要**（迁到 IVF_SQ8） | 不需要 |

关键点：

- V2.1 选 HNSW 的潜台词是"要么做完整 S3 Vectors（IVF 不 load），要么回最保守的 HNSW"
- 但"IVF_SQ8 + load + mmap + LRU"这个中间档**实现复杂度和 HNSW 方案完全相同**（换的只是 `index_type` 参数）
- RAM 占用低一个量级
- 延迟虽然比 HNSW 差，但产品定位本来就是 sub-second
- 避免 Phase 2 再做一次全量索引迁移

V2.1 担心的"IVF_SQ8 搞不定"主要是担心"不 load 可查"。一旦接受"IVF_SQ8 也走 load/release 语义"，这个担心就消失了。

### 4.4 HNSW 的位置调整

HNSW 不从路线里删除，而是**从 Phase 1 默认档调整为 Phase 3 性能档**：

- Phase 1/2：标准档 = `IVF_SQ8 + mmap`
- Phase 3：性能档 = `HNSW + load 常驻`，给大 bucket / 付费 bucket 毕业使用

这样 HNSW 的价值被保留（低延迟、高 recall），但不吃掉 Phase 1 的 RAM 预算。

## 5. 为什么 V2 的原始路线仍然不能做 Phase 1

和 V2.1 判断一致：

- V2 的"IVF_SQ8 + 不 load + JuiceFS 直查"依赖 Milvus 查询模型层面的能力，不是调参能得到
- V2 的"多桶共表 + partition_key 就天然支撑万级冷桶查询"过于乐观

V2.2 的修正是：

- 放弃"不 load"，接受 `LoadCollection` 是硬约束
- 但保留 IVF_SQ8 作为 Phase 1 默认，通过 mmap + load/release/LRU 实现"冷桶 RAM 成本低、热桶 load 后查询快"

## 6. V2.2 的阶段性路线

### Phase 1：可用版 Preview

交付内容：

- Bucket API
- Metadata Service
- bucket -> 独立 collection 映射
- **`IVF_SQ8 + mmap` 建索引**
- `LoadCollection / ReleaseCollection` 管理
- `LRU + TTL`
- 基础监控

明确不做：

- 多桶共表
- bucket 自动毕业
- HNSW 性能档
- 多后端联合查询
- Milvus 深改

阶段目标：

- 尽快做出可用版
- 用户能按 bucket 写入、查询、删除
- 热点 bucket 后续查询延迟稳定
- RAM 可控（受益于 IVF_SQ8 + mmap）

### Phase 2：多桶共表 + 路由优化

在 Phase 1 稳定后引入：

- 共享 collection 模型（`shared_small / medium`）
- `partition_key = bucket_id` 路由
- clustering compaction + segment prune 验证
- bucket group 粒度的 load/release
- 索引类型保持 `IVF_SQ8`（不切换）

这一阶段不是"引入新索引"，而是"**降低单位 bucket 的 collection 成本**"，让系统能支撑万级桶。

目标：

- 桶数量上限从千级提升到万级
- 单位 bucket 的元数据和调度成本显著下降

### Phase 3：性能档（HNSW 毕业）

再增加：

- `hot_dedicated_*` collection 模板，**`HNSW + load 常驻`**
- 访问统计
- 大 bucket / 热 bucket 自动毕业
- 标准档 <-> 性能档离线迁移
- 热档总量硬控在 RAM 预算内

目标：

- 大桶查询延迟从 sub-second 降到 ms 级
- 产品有性能梯度

### Phase 4：研究路线

保留不承诺：

- 真正的对象存储冷查询执行模型（不 load 可查）
- DISKANN 基础层
- Milvus 原生多 profile 支持

`DISKANN` 保留为研究备选，不进入近期路线。

## 7. 资源与容量估算

### 7.1 Phase 1 预算

- Milvus 基础进程 + 查询 working set：约 `2 GB`
- 标准档 `IVF_SQ8 + mmap` 活跃 load 集合：约 `4 GB`
- buffer / page cache / 弹性余量：约 `2 GB`

### 7.2 `IVF_SQ8 + mmap` 4 GB 预算粗略规模

| 向量维度 | 粗略可承载活跃加载总量 |
| --- | --- |
| `768D float32` | `200 万 - 350 万` |
| `1024D float32` | `150 万 - 250 万` |
| `1536D float32` | `100 万 - 180 万` |

这是所有已加载 bucket 加起来的量级。受以下因素影响：

- `nlist`、`nprobe`
- mmap page cache 命中率
- 并发压力
- JuiceFS 本地 cache 容量和命中率

注意：这不是硬承诺，是 Phase 1 规划量级。

### 7.3 Phase 3 HNSW 性能档预算（参考）

如果 Phase 3 给 HNSW 性能档预留 4 GB：

| 向量维度 | 粗略热 bucket 容量 |
| --- | --- |
| `768D float32` | `60 万 - 90 万` |
| `1024D float32` | `45 万 - 70 万` |
| `1536D float32` | `30 万 - 50 万` |

这是热档所有 bucket 加起来的上限，毕业准入按此反推。

### 7.4 Phase 1 的产品边界建议

和 V2.1 一致，Phase 1 主动限制：

- bucket 总数配额
- 活跃加载 bucket 数硬控
- 单 bucket 向量数上限
- 不承诺所有冷桶首查 sub-second
- 定义为：**可用版 / 预览版 / 限额版**

## 8. 工作量评估

### 8.1 Phase 1

工作范围：

- bucket API
- metadata
- bucket -> collection 映射
- **`IVF_SQ8 + mmap` 索引管理**
- load/release 控制
- LRU/TTL
- 基础监控

工作量预估（与 V2.1 HNSW 方案**相同**，因为改的只是 `index_type` 和 mmap 配置）：

- PoC：`1-2 周`
- 可用版：`2-4 周`
- 带基本稳定性和观测：`4-6 周`

### 8.2 Phase 2

工作范围：

- 共享 collection 模型
- bucket group 路由
- `partition_key` 和 clustering compaction 验证
- 迁移流程

工作量预估：追加 `3-6 周`

### 8.3 Phase 3

工作范围：

- HNSW 性能档 collection 模板
- 自动毕业 / 降级
- 热度统计
- 后台重建

工作量预估：追加 `3-6 周`

### 8.4 研究路线

不进入近期承诺。

## 9. 风险

Phase 1 风险：

- 冷 bucket 首查延迟包含 `LoadCollection` 成本
- `IVF_SQ8` 的 recall 对异常数据集可能偏低，需要在 Phase 1 早期做 recall 基准测试
- mmap 行为与 JuiceFS 本地 cache 的交互需要实测
- collection 数量增长带来元数据和调度成本（Phase 2 解决）

Phase 2 风险：

- 共享 collection 的隔离性和 prune 效果需要基准测试
- `partition_key` 默认分区数和 clustering compaction 配置需要调优
- bucket group 粒度选错会导致 load 单位过粗

Phase 3 风险：

- 升降档迁移抖动
- 热档预算被少数大 bucket 吃光
- 毕业策略复杂度

## 10. 最终建议

- 产品定位继续按 V2 走（对象存储的向量扩展，不是通用向量数据库）
- Phase 1 实现按当前 Milvus 能力边界走，但**默认索引用 `IVF_SQ8 + mmap + load/release + LRU`**
- HNSW 不从路线里删除，调整为 Phase 3 的性能档
- `partition_key` + 多桶共表留到 Phase 2
- DISKANN 保留研究路线，不进近期承诺
- Phase 1 主动限制产品边界（限额版、预览版）

一句话总结：

**V2.1 的分层思路对，阶段节奏对，但 Phase 1 默认索引选错——V2.2 把默认索引换成 `IVF_SQ8 + mmap`，实现复杂度和 HNSW 方案相同，RAM 占用低一个量级，还省掉 Phase 2 的索引迁移。HNSW 的位置从"Phase 1 默认"调整为"Phase 3 性能档"。其余全部保留 V2.1 结构。**
