# Milvus 当前项目架构速览

这份文档不是重复 `docs/milvus_architecture_deep_dive.md` 的细节版，而是基于当前仓库代码结构整理的“快速建图”版本，目标是回答三个问题：

1. 这个仓库里有哪些核心进程和模块
2. 一条写入链路和一条查询链路分别怎么走
3. 看代码时应该先从哪些目录和文件入手

如果你要深挖 Segment、Binlog、Compaction、Search Reduce 等细节，再回头看 `docs/milvus_architecture_deep_dive.md`。

## 1. 一句话理解项目

Milvus 当前代码可以理解成：

- Go 负责分布式协调、RPC、调度、元数据、流式链路和大部分业务编排
- C++ `internal/core/` 负责向量检索、段内执行、索引/表达式相关核心能力
- Rust 主要用于部分索引/检索生态集成（例如 tantivy 相关能力）
- 外部依赖负责元数据、对象存储和 WAL / 消息流

从系统形态上看，它仍然是“存算分离 + 流式写入 + 查询节点执行”的分布式向量数据库。

## 2. 启动模型和进程角色

### 2.1 启动入口

当前启动入口主要看这几个文件：

- `cmd/main.go`
- `cmd/milvus/run.go`
- `cmd/roles/roles.go`
- `cmd/components/*.go`

启动过程大致是：

1. `cmd/main.go` 进入 CLI
2. `cmd/milvus/run.go` 处理 `run` 命令、打印版本和硬件信息、准备运行时目录
3. `cmd/roles/roles.go` 根据配置决定本进程启哪些角色
4. `cmd/components/*` 构造具体组件
5. 组件内部再进入 `internal/*` 的真实业务实现

### 2.2 当前主要角色

`cmd/roles/roles.go` 里现在的主角色包括：

- `Proxy`
- `MixCoord`
- `QueryNode`
- `DataNode`
- `StreamingNode`
- `CDC`（可选）

其中最需要先记住的是：

- `Proxy` 是用户请求入口
- `MixCoord` 是“协调器集合体”
- `QueryNode` 负责查
- `StreamingNode` 负责 WAL 和流式写路径
- `DataNode` 负责数据侧后台任务，例如 flush / import / compaction / index worker 能力

### 2.3 MixCoord 不是单一协调器

当前代码里，协调层已经不是简单的“每个 coord 一个独立进程模型”来理解最方便了。更准确的说法是：

- `MixCoord` 是统一协调器容器
- 它内部组合了：
  - `RootCoord`
  - `DataCoord`
  - `QueryCoordV2`
  - `StreamingCoord`

核心文件：

- `internal/coordinator/mix_coord.go`

从这个文件可以直接看到：

- `rootcoordServer`
- `datacoordServer`
- `queryCoordServer`
- `streamingCoord`

也能看到它的启动顺序：

1. 先初始化 session / kv / streaming coord
2. `RootCoord` 初始化并启动
3. `DataCoord` 和 `QueryCoordV2` 并行启动
4. 整体状态从 standby 切到 active / healthy

## 3. 系统总图

可以先用下面这张图记住大关系：

```text
Client SDK / REST
        |
        v
     Proxy
   /   |    \
  /    |     \
 v     v      v
MixCoord   QueryNode   Streaming WAL
  |                      |
  |                      v
  |                 StreamingNode
  |                      |
  v                      v
etcd/TiKV          Object Storage
  ^
  |
DataCoord / RootCoord / QueryCoord
        |
        v
     DataNode
```

更贴近代码的理解是：

- 接入层：`internal/proxy`
- 协调层：`internal/rootcoord`、`internal/datacoord`、`internal/querycoordv2`、`internal/streamingcoord`
- 执行层：`internal/querynodev2`、`internal/datanode`、`internal/streamingnode`
- 复用基础设施：`internal/flushcommon`、`internal/metastore`、`pkg/v2/*`

## 4. 核心组件职责

### 4.1 Proxy

关键目录：

- `internal/proxy`
- `internal/distributed/proxy`

职责：

- 对外暴露 `MilvusService`
- 做鉴权、限流、参数校验、集合元数据缓存访问
- 构造 insert/search/query 等 task
- 写请求走分片和 WAL 追加
- 读请求走 QueryNode 路由和结果归并

看代码时可以这么分：

- `internal/distributed/proxy`: gRPC/HTTP 服务包装层
- `internal/proxy/impl.go`: 各类 API 入口
- `internal/proxy/task_*.go`: 具体请求处理逻辑
- `internal/proxy/shardclient`: 到 QueryNode 的 leader / replica 路由与负载均衡

### 4.2 RootCoord

关键目录：

- `internal/rootcoord`

职责：

- DDL 和元数据主入口
- 数据库、集合、分区、别名、函数等元数据管理
- 时间戳分配、ID 分配
- 对外提供 collection / partition 级元数据能力

如果你在查：

- 建表、删表、建分区
- schema 变更
- alloc timestamp / alloc id

优先看 `internal/rootcoord`。

### 4.3 DataCoord

关键目录：

- `internal/datacoord`

职责：

- Segment 生命周期管理
- flush / seal / compaction 调度
- 数据节点和段分配管理
- 数据侧元数据维护
- 索引构建任务的组织与推进

如果你在查：

- 一个 segment 什么时候 seal
- flush 后元数据怎么更新
- compaction 为什么触发
- index task 怎么建

优先看 `internal/datacoord`。

### 4.4 QueryCoordV2

关键目录：

- `internal/querycoordv2`

职责：

- collection load / release
- segment 和 channel 在 QueryNode 上的分配
- replica / balance / observer / checker
- 为查询链路提供路由层面的分布信息

这个模块偏“查询控制平面”，不是实际执行搜索的地方。

### 4.5 QueryNodeV2

关键目录：

- `internal/querynodev2`
- `internal/querynodev2/segments`
- `internal/querynodev2/delegator`

职责：

- 持有并管理 growing / sealed segment
- 执行 search / query / get
- 调用 C++ segcore / knowhere 做向量检索和执行
- 维护段加载、缓存、订阅和本地调度

这层要特别记住：

- Go 负责 orchestration
- 真正的底层向量执行经常会下沉到 `internal/core/`

### 4.6 StreamingNode

关键目录：

- `internal/streamingnode`
- `internal/streamingnode/server/wal`
- `internal/distributed/streaming`

职责：

- 持有和管理 WAL
- 接收 Proxy 追加的写消息
- 驱动流式消费、Growing Segment 写缓冲和后续 flush 链路

它是当前写入链路里非常重要的一层，不能再简单理解成“只有 msgstream”。

### 4.7 DataNode

关键目录：

- `internal/datanode`
- `internal/datanode/compactor`
- `internal/datanode/importv2`
- `internal/datanode/index`

职责：

- 数据持久化相关后台任务
- import 任务执行
- compaction 执行
- index / stats 相关 worker 能力
- 配合数据侧调度把对象存储上的数据组织好

一个容易误解的点是：`DataNode` 不只是“简单写盘”；当前代码里它还承担了 compaction、import、index worker 等职责。

## 5. 三类外部依赖

### 5.1 元数据存储

主要是：

- `etcd`
- 或 `TiKV`

用途：

- 服务注册与发现
- active / standby
- 元数据持久化
- 各类 coord 的 catalog / session 信息

相关目录：

- `internal/metastore`
- `internal/metastore/kv`
- `internal/util/sessionutil`

### 5.2 对象存储

主要是：

- `MinIO`
- `S3`
- 或本地存储兼容实现

用途：

- 持久化 binlog / deltalog / statslog
- 持久化 segment 数据和索引产物

### 5.3 WAL / 消息流

主要是：

- `Pulsar`
- `Kafka`
- `RocksMQ`
- 以及 streaming WAL 抽象层

相关目录：

- `internal/streamingnode/server/wal`
- `pkg/v2/mq/msgstream`
- `internal/distributed/streaming`

## 6. 一条写入链路怎么走

如果你想理解 insert / upsert，目前最推荐的读法是：

### 6.1 Proxy 接收请求

入口通常从：

- `internal/proxy/impl.go`

开始。

以 insert 为例：

1. 检查 Proxy 健康状态
2. 检查外部 collection 是否允许写入
3. 构造 `insertTask`
4. 进入 `PreExecute / Execute / PostExecute`

### 6.2 PreExecute 做请求准备

重点文件：

- `internal/proxy/task_insert.go`

这里主要做：

- 集合名与请求校验
- 从 MetaCache 获取 collection info / schema
- 主键或 RowID 分配
- 动态字段处理
- partition key 模式判断
- 字段合法性校验

### 6.3 Execute 做分片和 WAL 追加

重点文件：

- `internal/proxy/task_insert_streaming.go`
- `internal/proxy/msg_pack.go`
- `internal/proxy/util.go`

这里主要做：

- 获取 collection 对应的 VChannel
- 依据主键 hash 做 channel 路由
- 必要时再做 partition key 路由
- 把数据打包成 `InsertMsg`
- 调 `streaming.WAL().AppendMessages()` 写入 WAL

所以从代码视角看，写入不是“Proxy 直接写对象存储”，而是：

`Proxy -> WAL -> StreamingNode/下游写路径 -> flush -> object storage`

### 6.4 Streaming / Flush / Segment 生命周期

涉及目录：

- `internal/streamingnode`
- `internal/flushcommon`
- `internal/datacoord`

这里会发生：

- 写入进入 growing segment
- 写缓冲累积
- 触发 seal / flush
- flush 产物落对象存储
- DataCoord 更新 segment 生命周期
- 后续触发 index / compaction 等后台流程

`internal/flushcommon` 是这条链上的复用层，很值得单独记住。

## 7. 一条查询链路怎么走

如果你想理解 search / query，推荐这样看：

### 7.1 Proxy 解析请求和构造查询任务

重点文件：

- `internal/proxy/task_search.go`
- `internal/proxy/impl.go`

这里主要做：

- 请求校验
- schema / collection 元数据获取
- 查询表达式解析
- 生成 plan
- 选择分区裁剪策略

### 7.2 Proxy 通过 shardclient 找到 QueryNode

重点目录：

- `internal/proxy/shardclient`

这里负责：

- shard leader 缓存
- QueryNode client 池
- replica failover
- round robin / look aside 负载均衡

这层非常关键，因为很多“为什么请求打到某个 QueryNode”都在这里决定。

### 7.3 QueryNode 执行真正搜索

重点目录：

- `internal/querynodev2/services.go`
- `internal/querynodev2/delegator`
- `internal/querynodev2/segments`

这里主要做：

- 选择 growing / sealed segment
- 做 segment 裁剪
- 调 C++ core 执行向量检索或标量过滤
- 汇总子结果

### 7.4 Proxy 做最终归并

搜索结果回到 Proxy 后，会继续做：

- reduce
- rerank / function pipeline
- 最终结果整理和返回

所以查询路径可以粗略记成：

`Proxy plan + route -> QueryNode execute -> Proxy reduce`

## 8. 代码目录怎么分层看

这是当前仓库最值得先形成的目录认知：

### 8.1 `cmd/`

作用：

- 进程入口
- 角色启动
- 组件装配

### 8.2 `internal/distributed/`

作用：

- 各角色的分布式服务包装层
- gRPC / HTTP server
- client 连接封装

经验上，这里更多是“服务外壳”，不是主要业务逻辑。

### 8.3 `internal/*coord`, `internal/proxy`, `internal/*node`

作用：

- 真正的业务实现层

可以大致理解为：

- `internal/proxy`: 接入层
- `internal/rootcoord`, `internal/datacoord`, `internal/querycoordv2`, `internal/streamingcoord`: 控制平面
- `internal/querynodev2`, `internal/datanode`, `internal/streamingnode`: 执行平面

### 8.4 `internal/flushcommon`

作用：

- 写入 / flush 相关公共抽象
- metacache、pipeline、syncmgr、writebuffer 等

这是串起 StreamingNode、DataNode、DataCoord 的关键中间层。

### 8.5 `internal/metastore`

作用：

- 元数据存储抽象和 KV 实现

### 8.6 `internal/core`

作用：

- C++ 核心执行层
- 向量检索、segcore、表达式执行、底层存储与索引能力

很多“Go 看起来只是一层调度”的地方，真正执行会落到这里。

### 8.7 `pkg/`

这是单独的 Go module：`github.com/milvus-io/milvus/pkg/v2`。

作用：

- 公共库
- proto
- 日志、配置、错误、工具类
- MQ / stream / metrics / util 等通用能力

如果改这里的依赖，要在 `pkg/` 目录单独处理，不要默认在仓库根目录改。

## 9. 当前项目里容易混淆的几个点

### 9.1 `distributed` 目录不等于“核心逻辑目录”

很多新人会先钻 `internal/distributed/*`，但真正值得优先看的通常是对应的：

- `internal/proxy`
- `internal/rootcoord`
- `internal/datacoord`
- `internal/querycoordv2`
- `internal/querynodev2`
- `internal/datanode`
- `internal/streamingnode`

### 9.2 `MixCoord` 是组合协调器

不要把当前仓库理解成“RootCoord/DataCoord/QueryCoord 一定各自单独部署”的老模型；从代码结构和启动方式看，`MixCoord` 是更重要的抽象。

### 9.3 `QueryNode` 是 Go + C++ 混合执行

Go 层负责：

- 路由
- segment 管理
- 调度
- 结果组织

C++ 层负责：

- 真正的高性能向量执行

### 9.4 `DataNode` 和 `StreamingNode` 共同构成写路径

如果只盯着一个节点，很容易把写入链路看残。更准确的理解是：

- `StreamingNode` 偏实时写入与 WAL
- `DataNode` 偏后台物化、整理和维护任务
- `DataCoord` 决定什么时候应该 seal / flush / compact / build index

## 10. 推荐阅读顺序

### 10.1 先理解启动和角色

按这个顺序读：

1. `CLAUDE.md`
2. `cmd/milvus/run.go`
3. `cmd/roles/roles.go`
4. `internal/types/types.go`
5. `internal/coordinator/mix_coord.go`

### 10.2 想看写入链路

按这个顺序读：

1. `internal/proxy/impl.go`
2. `internal/proxy/task_insert.go`
3. `internal/proxy/task_insert_streaming.go`
4. `internal/proxy/msg_pack.go`
5. `internal/streamingnode`
6. `internal/flushcommon`
7. `internal/datacoord`

### 10.3 想看查询链路

按这个顺序读：

1. `internal/proxy/task_search.go`
2. `internal/proxy/shardclient`
3. `internal/querycoordv2`
4. `internal/querynodev2/services.go`
5. `internal/querynodev2/delegator`
6. `internal/querynodev2/segments`
7. `internal/core`

## 11. 和现有文档的关系

建议这样使用现有文档：

- `CLAUDE.md`: 仓库级速查，包括构建、测试、组件列表
- `docs/project_architecture_overview.md`: 当前代码结构的快速架构地图
- `docs/milvus_architecture_deep_dive.md`: 端到端链路和底层机制细节

如果后面继续补文档，最值得追加的是：

- 每个角色的“入口文件 -> 关键对象 -> 主时序”
- `flushcommon` 专项说明
- `QueryNodeV2` 的 segment / delegator 关系图
