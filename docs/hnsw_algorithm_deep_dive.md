# HNSW 算法原理与 Milvus 当前实现

这份文档只回答 `HNSW` 本身：

1. 它要解决什么问题
2. 它的核心原理是什么
3. 构图和搜索各自怎么做
4. 在 Milvus 里怎么使用
5. 当前 Milvus 代码实际上是怎么落到 C++/Knowhere/Faiss 的

## 1. HNSW 是干什么的

`HNSW` 全名是 `Hierarchical Navigable Small World`。

它要解决的问题是：

> 在高维向量空间里，**快速找到近似最近邻**，同时保持尽可能高的召回率。

如果对所有向量做 brute force：

- 复杂度接近 `O(N * dim)`
- 数据量一大，延迟很高

HNSW 的思路不是“把所有距离都算一遍”，而是：

- 先把数据组织成一个**多层图**
- 查询时从高层开始导航
- 逐层逼近目标区域
- 最后在底层附近收敛到一批近邻

所以它本质上是：

- **图索引**
- **近似搜索**
- **以内存和构图成本换搜索速度**

## 2. HNSW 适合什么场景

HNSW 最典型的特点是：

- 搜索延迟低
- 召回率高
- 构图成本比 FLAT 大很多
- 内存开销明显高于 IVF/FLAT

可以粗略这么理解：

| 维度 | HNSW 的倾向 |
|------|-------------|
| 搜索速度 | 很快 |
| 召回率 | 很高 |
| 构建成本 | 较高 |
| 内存占用 | 较高 |
| 更新方式 | 支持增量插入，但大规模动态更新不是它最舒服的场景 |

在 Milvus 里，HNSW 常用于：

- 向量规模中大
- 查询延迟敏感
- 机器内存相对充足
- 希望用较少参数就拿到不错 recall

## 3. HNSW 的核心原理

如果只保留算法骨架，HNSW 的构图逻辑可以压成 5 句话：

1. 给每个点随机分配一个最高层
2. 从最高层入口点往下做 greedy 导航
3. 在每一层找到一批候选邻居
4. 用启发式从候选里剪到 `M` 个
5. 给新点和旧点建立双向边

这 5 句基本就是整套算法。

### 3.1 “分层”是什么意思

HNSW 不是一张单层图，而是多层图：

```text
Level 3: 少量节点，负责全局导航
Level 2: 比上层多一些
Level 1: 更多
Level 0: 所有节点都在这一层
```

特点是：

- 层越高，点越少
- 越高层越像“全局路由层”
- 最底层负责最终近邻收敛

这套结构的目标是：

- 高层先快速跳到“正确的大区域”
- 底层再做更细的局部搜索

### 3.2 为什么要随机分层

每个新点插入时会随机得到一个 level。

直觉上：

- 大多数点只在底层
- 少数点会进入更高层
- 极少数点进入最顶层

这样就会自然形成一个“稀疏的高层导航图 + 稠密的底层局部图”。

它不是聚类，不需要先把全局数据分区，而是靠随机层级制造多尺度导航结构。

### 3.3 为什么不是直接存 top-K 最近邻

这是 HNSW 最核心的地方。

如果一个点只连“最近的 M 个点”，图很容易变成：

- 局部特别密
- 跨区域跳转能力差
- 搜索时容易困在某个小团里

所以 HNSW 不做简单 top-M，而是做一种**多样性剪枝**：

- 候选点里，先按离 query/新点的距离排序
- 依次尝试保留
- 如果某个候选点离“已选邻居”更近，而不是离 query 更近，就把它淘汰

这样保留下来的边不会全挤在一个局部小团里，图更利于导航。

## 4. HNSW 的构图过程

下面按“插入一个新点”来讲。

### 4.1 随机决定层级

假设新点 `P` 被随机分到最高层 `L=2`。

那它会出现在：

- Level 2
- Level 1
- Level 0

不会出现在更高层。

### 4.2 从当前入口点往下 greedy 下降

图里会维护一个当前全局入口点 `entry_point`，通常是某个最高层节点。

插入新点 `P` 时：

1. 从最高层的 `entry_point` 出发
2. 在当前层不断看邻居
3. 如果发现某个邻居比当前节点更接近 `P`，就跳过去
4. 直到这一层找不到更近的点
5. 再下降到下一层继续

这一步是典型的 `greedy routing`。

它的目标不是“一次找到所有邻居”，而是先把搜索入口逐层带到离 `P` 更近的区域。

### 4.3 在每一层做候选扩展

当下降到某一层后，不是只看一个最近点就结束，而是会扩展出一个候选集合。

候选集合大小由 `efConstruction` 控制。

你可以把它理解成：

- `efConstruction` 小：看得窄，构图快，但边质量可能差
- `efConstruction` 大：看得广，构图慢，但边质量通常更好

所以：

- `M` 决定最终留下多少边
- `efConstruction` 决定在留下这些边之前，先看多大一圈

### 4.4 用启发式把候选剪到 M 个

假设当前层找到了这些候选：

```text
A, B, C, D, E
```

它们都离新点 `P` 不远。

如果简单取最近的 `M=2`，可能得到：

```text
A, B
```

但如果 `A` 和 `B` 自己也离得特别近，那这两条边其实很冗余。

HNSW 的做法是：

1. 先选最好的候选，比如 `A`
2. 再看 `B`
3. 如果 `B` 离 `A` 比离 `P` 还近，就认为它“太冗余”，不要
4. 接着看 `C`、`D`...

这样最终留下来的 2 个邻居可能变成：

```text
A, D
```

虽然 `D` 不一定比 `B` 更近，但它和 `A` 的方向差异更大，能让图更容易导航。

这一步就是 HNSW 的核心启发式。

### 4.5 建双向边

新点 `P` 最终选出邻居后，不是只建立：

```text
P -> A
P -> D
```

而是还要更新对方：

```text
A -> P
D -> P
```

如果旧点的邻接表满了，还会重新做一次剪枝，决定是否保留 `P`、踢掉谁。

所以构图不是“只追加新点”，而是一个持续的局部重排过程。

## 5. HNSW 的搜索过程

查询和构图有相似的精神，但目标不同。

### 5.1 高层：贪心导航

给定查询向量 `q`：

1. 从全局 `entry_point` 开始
2. 在最高层做 greedy 搜索
3. 找到这一层最接近 `q` 的入口
4. 下降到下一层

这一步主要负责“快速跳到大致正确的区域”。

### 5.2 底层：束搜索 / 扩展搜索

到了底层以后，搜索会维护一个候选集合，扩展宽度由 `ef` 控制。

这里：

- `ef` 是**搜索期参数**
- `efConstruction` 是**构建期参数**

搜索时的直觉：

- `ef` 越大，搜索看得越广
- recall 越高
- 延迟也越大

因此 HNSW 搜索本质上是：

- 高层贪心
- 底层 beam-like expansion

## 6. 三个最重要的参数

### 6.1 `M`

含义：

- 每个节点保留多少条邻边

影响：

- 越大，图越密
- recall 往往更高
- 内存更大
- 构建更慢

经验理解：

- `M` 控制图的“宽度”

### 6.2 `efConstruction`

含义：

- 构图时候选扩展宽度

影响：

- 越大，构图时看得越广
- 候选质量更高
- 最终边通常更好
- 构建更慢

经验理解：

- `efConstruction` 控制构图时的“视野”

### 6.3 `ef`

含义：

- 搜索时底层扩展宽度

影响：

- 越大，召回更高
- 搜索更慢

经验理解：

- `ef` 控制查询时的“视野”

## 7. 一个示意例子

下面的例子是**示意**，目的是帮助理解，不是某次真实运行的固定输出。

### 7.1 数据

已有 6 个点：

```text
A = (0.0, 0.0)
B = (0.0, 1.0)
C = (1.0, 0.0)
D = (5.0, 5.0)
E = (5.0, 6.0)
F = (6.0, 5.0)
```

新插入一个点：

```text
P = (0.2, 0.2)
```

参数设成：

```text
M = 2
efConstruction = 4
```

### 7.2 高层导航

假设当前最高层入口点是 `E`。

因为 `P` 在左下角，而 `E/D/F` 在右上角，所以 greedy 导航大致会这样：

```text
E -> D -> C
```

含义不是“C 一定是最终邻居”，而是：

- 从高层逐步把搜索入口带到 `(0,0)` 这一侧

### 7.3 当前层候选集合

到了更低层以后，候选扩展可能得到：

```text
A, B, C, D
```

它们到 `P` 的距离大致是：

```text
dist(P, A) = 0.28
dist(P, C) = 0.82
dist(P, B) = 0.82
dist(P, D) = 很远
```

如果只按最近邻取 top-2，会得到：

```text
A, B
```

或者：

```text
A, C
```

### 7.4 为什么还要做剪枝

如果 `A/B/C` 三个点都集中在非常相似的方向上，那么保留两个最接近的点可能会导致边很冗余。

HNSW 的启发式会做类似这样的判断：

- 先保留 `A`
- 再检查 `B`
- 如果 `B` 离 `A` 比离 `P` 还近，说明它更多是在重复 `A` 的局部结构
- 那就把 `B` 剪掉
- 再看 `C`

最终保留下来的 2 个边，未必是“纯距离最小”的 2 个，而是“距离够近且方向更分散”的 2 个。

这就是为什么 HNSW 图比普通 kNN 图更适合搜索导航。

## 8. 在 Milvus 里怎么用 HNSW

在 Milvus 里，HNSW 最常用的索引参数就是：

```json
{
  "index_type": "HNSW",
  "metric_type": "COSINE",
  "M": "32",
  "efConstruction": "200"
}
```

搜索时的参数是：

```json
{
  "ef": 64
}
```

### 8.1 Go Client 写法

Milvus Go client 里直接有 HNSW helper：

```go
idx := index.NewHNSWIndex(entity.COSINE, 32, 200)
ann := index.NewHNSWAnnParam(64)
```

对应代码：

- `client/index/hnsw.go`

### 8.2 参数怎么选

如果只是一个保守起点，可以从：

```text
M = 16 ~ 32
efConstruction = 100 ~ 300
ef = 32 ~ 128
```

开始。

粗略调参方向：

- 召回不够：先增大 `ef`
- 构图质量不够：增大 `efConstruction`
- 图太稀、召回始终上不去：增大 `M`
- 内存吃不消：减小 `M`

### 8.3 当前实现里的默认行为

在当前 Knowhere 配置里：

- `M` 默认值是 `30`
- `efConstruction` 默认值是 `360`
- 如果搜索时没有显式传 `ef`，会自动补成至少 `max(k, 16)`

所以：

- 想要可控、可复现的行为，最好显式给出 `M`、`efConstruction`、`ef`
- 不要完全依赖默认值，尤其是在不同环境或自动索引配置下

## 9. 当前 Milvus 里的真实实现路径

这里讲的是**当前仓库配置下的实际代码路径**。

### 9.1 Go 入口

DataNode 构建索引任务在：

- `internal/datanode/index/task_index.go`

核心流程是：

1. 读取原始向量列的 binlog 路径
2. 组装 `BuildIndexInfo`
3. 调 `indexcgowrapper.CreateIndex()`

### 9.2 C++ 入口

Go -> C 的桥在：

- `internal/util/indexcgowrapper/index.go`

它会调：

- `C.CreateIndex(...)`

真正的 C++ 入口在：

- `internal/core/src/indexbuilder/index_c.cpp`

这个函数负责：

1. 反序列化 `BuildIndexInfo`
2. 组装 config 和 file manager
3. 创建索引对象
4. 调 `index->Build()`

### 9.3 Milvus C++ 内部索引对象

向量 HNSW 会走到：

- `internal/core/src/indexbuilder/VecIndexCreator.cpp`
- `internal/core/src/index/IndexFactory.cpp`
- `internal/core/src/index/VectorMemIndex.cpp`

`VectorMemIndex::Build()` 的关键动作不是图构建，而是：

1. 从对象存储读回原始向量列 binlog
2. 拼成一块连续内存
3. 生成 `dataset`
4. 把这个 `dataset` 交给 Knowhere

### 9.4 当前 Knowhere 路径

Milvus 当前通过：

- `internal/core/thirdparty/knowhere/CMakeLists.txt`

拉取 Knowhere。

在当前配置的 Knowhere 提交上，`INDEX_HNSW` 对应的实现是：

- `knowhere/src/index/hnsw/faiss_hnsw.cc`

不是老 `hnswlib` wrapper 直通路径。

也就是说，当前 `HNSW` 更准确地说是：

> **Milvus + Knowhere 包装 + Faiss HNSW 实现**

### 9.5 真正的图构建落点

真正的图构建骨架在：

- `knowhere/thirdparty/faiss/faiss/IndexHNSW.cpp`
- `knowhere/thirdparty/faiss/faiss/impl/HNSW.cpp`
- `knowhere/thirdparty/faiss/faiss/impl/HNSW.h`

最关键的函数是：

- `IndexHNSW::add()`
- `hnsw_add_vertices()`
- `HNSW::add_with_locks()`
- `search_neighbors_to_add()`
- `shrink_neighbor_list()`

注意一个很关键的点：

> 对 HNSW 来说，真正的图构建主要发生在 `add()`，不是 `train()`。

## 10. 当前代码里几个值得特别记住的事实

### 10.1 `train()` 不等于“图已经建好”

在 Faiss HNSW 里，`train()` 主要是让 storage ready，并设置 `is_trained=true`。

真正把边建出来的是后面的：

- `index->add(...)`

### 10.2 Level 0 的邻居容量通常是 `2*M`

而更高层通常是：

- `M`

所以底层更稠密，高层更稀疏。

### 10.3 HNSW 的核心不是“取 top-M 最近邻”

它的关键是：

- 先找候选
- 再做多样性剪枝

因此：

- 图结构更分散
- 更适合导航
- 也更符合 ANN 搜索需求

## 11. 一句话总结

HNSW 的本质可以概括为：

> 用“随机分层 + 上层 greedy 导航 + 下层候选扩展 + 多样性剪枝”构建一张适合近似最近邻搜索的分层小世界图。

在 Milvus 当前实现里：

- Go 负责组装索引任务与存储路径
- Milvus C++ 负责把原始 binlog 还原成连续 dataset
- Knowhere 负责 HNSW 节点封装
- Faiss HNSW 负责真正的分层图构建与搜索

## 12. 如果继续往下看，建议先读哪几个函数

如果你已经接受“HNSW 本来就不简单”，继续读源码时最值得先盯住这几个：

1. `knowhere/thirdparty/faiss/faiss/impl/HNSW.cpp`  
   `shrink_neighbor_list()`

2. `knowhere/thirdparty/faiss/faiss/impl/HNSW.cpp`  
   `add_with_locks()`

3. `knowhere/thirdparty/faiss/faiss/IndexHNSW.cpp`  
   `hnsw_add_vertices()`

4. `internal/core/src/index/VectorMemIndex.cpp`  
   `Build()`

按这个顺序看，理解成本最低。
