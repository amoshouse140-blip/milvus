# Milvus 插入链路深挖

这份文档只回答一件事：

> 一条数据从 `Insert()` 进入 Milvus 之后，到底经过哪些组件、什么时候分配 `segmentID`、什么时候 flush、为什么刚插入就能搜索到、C++ 里到底怎么写进去。

它不展开索引构建算法细节。  
`HNSW / IVF / DiskANN` 这条索引构建链路建议单独写一篇，否则会把“插入数据”和“构建索引”两条路径混掉。

## 0. 先给结论

Milvus 的“插入”实际上分成两条并行但职责不同的链路：

1. **持久化主链路**

```text
Client
  → Proxy
  → WAL
  → StreamingNode
  → WriteBuffer
  → Flush
  → 对象存储 binlog
```

这条链路回答的是：

- 数据什么时候被接收
- 数据什么时候落到某个 `segment`
- 数据什么时候刷成对象存储里的 `insert_log/...`

2. **实时可见链路**

```text
同一批 InsertMsg
  → QueryNode 消费同一条 vchannel
  → Growing Segment
  → Go -> C++ -> SegmentGrowingImpl::Insert()
```

这条链路回答的是：

- 为什么刚 `insert` 完还没 flush 也能 `search`
- C++ `segcore` 的真正插入点在哪里

最容易混的地方有三个：

1. **Proxy 不决定最终 `segmentID`**
2. **StreamingNode 的 WriteBuffer 不调用 `segcore.Insert()`**
3. **QueryNode 的 Growing Segment 会调用 `segcore.Insert()`，但它负责“实时可查”，不是“持久化写盘”**

## 1. 术语先统一

### 1.1 DML Channel 是什么

`DML` = `Data Manipulation Language`，在这里指：

- `Insert`
- `Delete`

`DML channel` 在这条链路里，基本可以理解为：

- **承载插入/删除增量消息的 channel**
- 对插入流程来说，通常就是 **某个 vchannel**

所以：

> “QueryNode 也在消费同一条 DML channel”

翻成大白话就是：

> Proxy 把这批数据 hash 到哪个 `vchannel`，StreamingNode 和 QueryNode 后面都会消费这个 `vchannel` 上的增量消息。

### 1.2 vchannel / pchannel / segment 的关系

可以先用这三个层次理解：

```text
Collection
  └── vchannel (逻辑分片)
        └── segment (该 vchannel 下的一段数据)
```

其中：

- `vchannel`：逻辑分片，插入按主键 hash 到这里
- `pchannel`：底层物理 channel，一个或多个 vchannel 复用它
- `segment`：真正承载一批数据的段；一个段属于某个 partition，也属于某个 vchannel

### 1.3 三种“插入完成”

“插入完成”在 Milvus 里有三个不同层次：

1. **Proxy 返回成功**
   说明消息已成功 append 到 WAL，并拿到可见时间戳

2. **QueryNode 可见**
   说明这批消息已经回放进 QueryNode 的 growing segment

3. **Flush 完成**
   说明这批数据已经变成对象存储里的 binlog 文件

用户平时最常看到的是第 1 个。  
真正落盘是第 3 个。  
“刚写完就能查到”依赖的是第 2 个。

## 2. 用一个最小例子贯穿整条链路

假设用户写 3 行数据：

```text
row0: id=101, price=19.8, embedding=vec0
row1: id=102, price=29.9, embedding=vec1
row2: id=103, price= 9.9, embedding=vec2
```

假设这个 collection 有两个 `vchannel`：

- `ch0`
- `ch1`

后面整篇文档都用这个例子。

## 3. Proxy 入口：请求先变成列式

入口：

- `internal/proxy/impl.go`
- `internal/proxy/task_insert.go`
- `internal/proxy/task_insert_streaming.go`

用户从 SDK 看，通常是“按行”的：

```text
[
  {"id":101, "price":19.8, "embedding":vec0},
  {"id":102, "price":29.9, "embedding":vec1},
  {"id":103, "price": 9.9, "embedding":vec2},
]
```

但进入 Proxy 之后，很快会变成“按列”的 `InsertRequest`：

```text
NumRows = 3

FieldsData["id"]        = [101, 102, 103]
FieldsData["price"]     = [19.8, 29.9, 9.9]
FieldsData["embedding"] = [vec0, vec1, vec2]
```

### 3.1 PreExecute 做了什么

`insertTask.PreExecute()` 主要做这些事：

1. 校验 collection/schema/字段类型
2. 处理 `autoID`
3. 给每一行分配内部 `RowID`
4. 给每一行设置时间戳
5. 检查动态字段、nullable/default value 等

这一批 3 行数据经过 PreExecute 之后，大致会变成：

```text
PK(id)      = [101, 102, 103]
RowIDs      = [90001, 90002, 90003]
Timestamps  = [t, t, t]
FieldsData  = {
  id        = [101, 102, 103],
  price     = [19.8, 29.9, 9.9],
  embedding = [vec0, vec1, vec2],
}
```

这里要注意：

- `id` 是业务主键
- `RowID` 是系统内部行标识
- 这两个不是一回事

## 4. Proxy 分片：先按 PK hash 到 vchannel

相关代码：

- `internal/proxy/task_insert_streaming.go`
- `internal/proxy/msg_pack.go`
- `internal/proxy/util.go`

Proxy 不会“把一整批数据原样丢给下游”，而是先按主键 hash 到不同 `vchannel`。

在我们的例子里，假设 hash 结果是：

```text
101 -> ch0
102 -> ch1
103 -> ch0
```

于是会拆出两条逻辑上的 `InsertMessage`：

```text
msgA(channel=ch0):
  id        = [101, 103]
  price     = [19.8, 9.9]
  embedding = [vec0, vec2]
  rowIDs    = [90001, 90003]
  ts        = [t, t]

msgB(channel=ch1):
  id        = [102]
  price     = [29.9]
  embedding = [vec1]
  rowIDs    = [90002]
  ts        = [t]
```

注意这里是：

- **按行分 shard**
- **每个 shard 内仍然按列存**

也就是说：

- `row0` 和 `row2` 去 `ch0`
- `row1` 去 `ch1`
- 但 `msgA` / `msgB` 仍然是列式 `FieldsData`

## 5. 写 WAL 时，还没有最终 SegmentID

Proxy 最后调用：

```go
streaming.WAL().AppendMessages(ctx, msgs...)
```

这时写入的消息已经确定了：

- `CollectionId`
- `PartitionId`
- `VChannel`
- `Rows`
- `FieldsData`

但是还**没有最终 `segmentID`**。

### 5.1 Proxy 什么时候给客户端返回成功

对客户端来说，`Insert()` 返回成功的时刻是：

```text
Proxy 成功把 InsertMessage append 到 WAL
  + 拿到本次写入的可见 time tick
```

它**不等于**：

- 已经 flush 到对象存储
- 已经 build index
- 已经从对象存储重新 load 成 sealed segment

所以最准确的理解是：

> `Insert` 返回成功，表示“这批数据已经被 Milvus 接受并写入 WAL”；  
> 后面的 QueryNode 可见、Flush、IndexBuild 都是后续阶段。

这是理解整条链路最关键的一点：

> **Proxy 只决定“这批行去哪个 vchannel”，不决定“最终落到哪个 segment”。**

## 6. StreamingNode 才负责给 Insert 分配 segment

相关代码：

- `internal/streamingnode/server/wal/interceptors/shard/shard_interceptor.go`
- `internal/streamingnode/server/wal/interceptors/shard/shards/partition_manager.go`
- `internal/streamingnode/server/wal/interceptors/shard/shards/segment_alloc_worker.go`

### 6.1 真实分配发生在哪里

WAL interceptor 处理 insert 时会走：

```text
handleInsertMessage
  → shardManager.AssignSegment(req)
    → partitionManager.AssignSegment(req)
      → segmentAllocManager.AllocRows(req)
```

如果当前 `(collection, partition, vchannel)` 已经有一个还在写的 growing segment，并且容量够：

- 这条 insert 会直接被分配到那个 segment

如果没有可写 segment：

```text
AssignSegment
  → asyncAllocSegment()
  → append CreateSegmentMessage 到 WAL
  → 等待新段 ready
  → 再给 InsertMessage 回填真实 SegmentID
```

### 6.2 套到例子里

假设：

- `msgA` 最终被分到 `segment=7001`
- `msgB` 最终被分到 `segment=7002`

那么从语义上看，WAL 里发生的是：

```text
CreateSegment(7001, channel=ch0)
Insert(segment=7001, rows=[101,103])

CreateSegment(7002, channel=ch1)
Insert(segment=7002, rows=[102])
```

所以到这一步为止：

- `vchannel` 已经在 Proxy 阶段决定了
- `segmentID` 是在 StreamingNode 阶段决定的

## 7. StreamingNode 持久化主链路：按 segment 缓冲

相关代码：

- `internal/streamingnode/server/flusher/flusherimpl/msg_handler_impl.go`
- `internal/flushcommon/pipeline/flow_graph_write_node.go`
- `internal/flushcommon/writebuffer/write_buffer.go`

StreamingNode 后面会消费 WAL，把 insert 消息继续送进 FlowGraph / WriteBuffer。

关键路径可以简化成：

```text
WAL Scanner
  → writeNode.Operate()
    → PrepareInsert(...)
      → 按 SegmentID 分组
      → InsertMsg -> storage.InsertData
    → WriteBufferManager.BufferData(...)
```

这里的关键点是：

> `PrepareInsert()` 的分组键就是 `msg.SegmentID`

所以它已经不是“按 vchannel 模糊缓冲”，而是“按具体 segment 缓冲”。

套到例子里，WriteBuffer 里会是：

```text
WriteBuffer[7001]:
  id        = [101, 103]
  price     = [19.8, 9.9]
  embedding = [vec0, vec2]

WriteBuffer[7002]:
  id        = [102]
  price     = [29.9]
  embedding = [vec1]
```

到这里仍然是：

- Go 侧缓冲
- 还没有调用 QueryNode 的 `segcore.Insert()`

## 8. 什么时候会 flush

这个问题不能只说“segment 满了就 flush”，因为 Milvus 里至少有两层动作：

1. **Seal**
   这个 segment 不再继续接收新写入

2. **Sync / Flush**
   把当前缓冲的数据写成 binlog / delta log 到对象存储

先 seal，不一定立刻对用户等价于“全量 flush 完成”；但后面通常会跟着真正的 flush/sync。

### 8.1 什么时候会先 seal segment

相关代码：

- `internal/streamingnode/server/wal/interceptors/shard/stats/stats_manager.go`
- `internal/streamingnode/server/wal/interceptors/shard/stats/stats_seal_worker.go`
- `internal/streamingnode/server/wal/interceptors/shard/policy/seal_policy.go`

当前代码里，常见 seal 触发包括：

| 触发原因 | 代码路径 | 含义 |
|----------|----------|------|
| 容量打满 | `StatsManager.AllocRows()` -> `PolicyCapacity()` | 当前段写到容量上限 |
| 生命周期过长 | `selectSegmentsWithTimePolicy()` -> `PolicyLifetime()` | 段存在太久，定时扫描触发 |
| 长时间空闲 | `selectSegmentsWithTimePolicy()` -> `PolicyIdle()` | 有数据但长期没新写入 |
| binlog 文件数过多 | `UpdateOnSync()` -> `PolicyBinlogNumber()` | sync 后 binlog 数超阈值 |
| 全局 growing bytes 超高水位 | `NotifyGrowingBytes()` | 通过 seal 一批段把内存压回低水位 |
| 节点内存压力过高 | `PolicyNodeMemory()` | 系统内存 used ratio 太高 |
| 手动 flush / schema 变更 / truncate | `FlushAndFenceSegmentAllocUntil()` | 把某个时间点之前的旧段全部封住 |

可以把它理解成：

> **segment seal 是“停止继续往这个段写”**

### 8.2 什么时候真正 sync / flush 到对象存储

相关代码：

- `internal/flushcommon/writebuffer/options.go`
- `internal/flushcommon/writebuffer/sync_policy.go`
- `internal/flushcommon/writebuffer/write_buffer.go`
- `internal/flushcommon/writebuffer/manager.go`

WriteBuffer 的默认 sync policy 有这些：

| 触发原因 | 代码 | 含义 |
|----------|------|------|
| buffer full | `GetFullBufferPolicy()` | 内存 buffer 达到 `FlushInsertBufferSize` |
| buffer stale | `GetSyncStaleBufferPolicy()` | 缓冲在 `SyncPeriod` 后仍未刷，带一点 jitter |
| segment sealed | `GetSealedSegmentsPolicy()` | 已 seal 的 segment 会被选出来同步 |
| segment dropped | `GetDroppedSegmentPolicy()` | 被 drop 的 segment 会被同步/清理 |
| flush timestamp 到达 | `GetFlushTsPolicy()` | 手动 flush / flush-all 等设置了 flush ts |

另外还有一个独立的“强制内存回收”路径：

- `bufferManager` 会扫描所有 channel 的内存使用
- 如果总内存超过 `MemoryForceSyncWatermark`
- 就对占用最大的 writebuffer 触发 `EvictBuffer(GetOldestBufferPolicy(...))`

也就是说：

> **真正 flush 并不只靠“段满了”**

还可能因为：

- 太久没刷
- 已 seal
- 用户手动 flush
- schema 变更
- truncate
- 内存压力过高

### 8.3 手动 flush 的语义

相关代码：

- `internal/streamingnode/server/flusher/flusherimpl/msg_handler_impl.go`

手动 flush 本质上做两件事：

1. 先 seal 指定 segment 或整个 channel 上的旧 segment
2. 再设置 `flushTs`

这样 WriteBuffer 后续在 checkpoint 到达对应时间戳后，就会把这批 segment 真正刷出去。

## 9. Flush 完成之后对象存储里长什么样

Flush 后，每个字段会变成独立的 binlog 文件。

不是：

```text
一行一条 JSON
```

而是更接近：

```text
insert_log/{collection}/{partition}/{segment}/{field}/...
```

例如 `segment=7001`，向量字段 `fieldID=103`：

```text
insert_log/.../7001/103/...
```

这意味着：

- 标量列各有自己的 binlog
- 向量列也有自己的 binlog
- flush 是“列式写盘”，不是“行式写盘”

## 10. 为什么刚 insert 完就能 search

这就是第二条链路：**实时可见链路**。

相关代码：

- `internal/querynodev2/services.go`
- `internal/querynodev2/pipeline/pipeline.go`
- `internal/querynodev2/pipeline/insert_node.go`
- `internal/querynodev2/delegator/delegator_data.go`

QueryNode 在 `WatchDmChannels()` 之后，也会去消费同一个 `vchannel`。

注意，是：

- 同一条 `vchannel`
- 同一批 `InsertMsg`

但是它的目的不是写对象存储，而是把数据回放到本地 growing segment，让查询线程立刻可见。

## 11. QueryNode 的实时可见链路

整体链路可以简化成：

```text
QueryNode.WatchDmChannels
  → pipeline.ConsumeMsgStream(...)
  → filterNode
  → insertNode.addInsertData(...)
    → storage.TransferInsertMsgToInsertRecord(...)
  → delegator.ProcessInsert(...)
    → growing.Insert(...)
      → LocalSegment.Insert(...)
        → s.csegment.Insert(...)
          → cSegmentImpl.Insert(...)
            → PreInsert + marshal InsertRecord + C.Insert(...)
              → segment_c.cpp::Insert(...)
                → SegmentGrowingImpl::Insert(...)
```

### 11.1 这一步为什么也是“insert 逻辑”

因为它确实是在“把数据插入到 Growing Segment”。

但它不是持久化主链路，而是：

- **增量回放**
- **实时可见**

所以更准确的说法是：

> QueryNode 在执行的是“插入消息的回放插入”，不是“主写入链路的落盘插入”。

## 12. Go 到 C++ 的真正插入点

相关代码：

- `internal/querynodev2/segments/segment.go`
- `internal/util/segcore/segment.go`
- `internal/core/src/segcore/segment_c.cpp`
- `internal/core/src/segcore/SegmentGrowingImpl.cpp`

### 12.1 Go 侧

`LocalSegment.Insert()` 会调用：

```text
s.csegment.Insert(...)
  → cSegmentImpl.Insert(...)
    → preInsert(...)
    → proto.Marshal(InsertRecord)
    → C.Insert(...)
```

这里有三个输入：

1. `RowIDs[]`
2. `Timestamps[]`
3. `InsertRecord.FieldsData`

这里再强调一次：

- `InsertRecord.FieldsData` 是业务字段列
- `RowIDs[]` 和 `Timestamps[]` 是分开传的
- 它们不全都塞在同一个 `FieldsData` 结构里

### 12.2 C 入口

`segment_c.cpp::Insert()` 做的事情很直接：

1. 反序列化 `InsertRecordProto`
2. 调 `segment->Insert(...)`

也就是：

```cpp
segment->Insert(reserved_offset,
                size,
                row_ids,
                timestamps,
                insert_record_proto.get());
```

## 13. C++ 里到底怎么插

真正的逻辑在：

- `internal/core/src/segcore/SegmentGrowingImpl.cpp`

核心步骤可以概括成 7 步。

### 13.1 先申请一段连续 offset

Go 侧会先调 `PreInsert(n)`。

比如当前段是空的，这批 2 行写入：

```text
PreInsert(2) -> reserved_offset = 0
```

意思是：

- 这 2 行会占用 offset `[0, 2)`

如果这不是第一次写，可能是：

```text
PreInsert(2) -> reserved_offset = 128
```

说明这批行会落到：

- offset `128`
- offset `129`

### 13.2 校验字段集合

`SegmentGrowingImpl::Insert()` 先检查：

- `InsertRecordProto.num_rows == num_rows`
- 字段集合是否合法
- 是否有重复字段

如果 segment 的 schema 更新过，而这条 insert 还是旧 schema：

- C++ 会给缺失字段补空列

所以它不是“盲写”。

### 13.3 写 timestamp 列

`timestamps_raw` 会先写进内部的时间戳列：

```text
insert_record_.timestamps_
```

这一步之后，每个 offset 都有了自己的可见时间。

### 13.4 写每个业务字段的列数据

然后它会遍历 schema 里的每个用户字段，把 `InsertRecordProto.fields_data(...)` 里的列式数据灌进 growing segment 内部的列存结构。

可以把它理解成：

```text
offset 0:
  id        = 101
  price     = 19.8
  embedding = vec0

offset 1:
  id        = 103
  price     =  9.9
  embedding = vec2
```

但是内部不是“存两行 struct”，而是更接近：

```text
column[id]        = [101, 103]
column[price]     = [19.8, 9.9]
column[embedding] = [vec0, vec2]
```

这就是 Growing Segment 里的列式内存布局。

### 13.5 更新辅助结构

如果当前配置启用了额外能力，还会同步更新：

- interim segment index
- text index
- geometry cache
- array offset 等辅助结构

所以这一步不仅仅是“把原始值放进去”，还可能附带维护一些小索引或缓存。

### 13.6 建立 PK -> offset 映射

接着 C++ 会解析主键列，建立主键到 offset 的映射。

例如对 `segment=7001`：

```text
pk 101 -> offset 0
pk 103 -> offset 1
```

这一步对：

- 删除
- 去重
- 主键相关检索

都很重要。

### 13.7 更新资源与可见性

最后会更新：

- 内存统计
- 资源使用情况
- ack / 可见段范围

到这一步，这批数据才算真正进入了 C++ growing segment。

## 14. 套回我们的 3 行例子

### 14.1 Proxy 分完 shard

```text
msgA(channel=ch0):
  id        = [101, 103]
  price     = [19.8, 9.9]
  embedding = [vec0, vec2]
  rowIDs    = [90001, 90003]
  ts        = [t, t]

msgB(channel=ch1):
  id        = [102]
  price     = [29.9]
  embedding = [vec1]
  rowIDs    = [90002]
  ts        = [t]
```

### 14.2 StreamingNode 分完 segment

```text
msgA -> segment 7001
msgB -> segment 7002
```

### 14.3 WriteBuffer 缓冲

```text
WriteBuffer[7001]:
  id        = [101, 103]
  price     = [19.8, 9.9]
  embedding = [vec0, vec2]

WriteBuffer[7002]:
  id        = [102]
  price     = [29.9]
  embedding = [vec1]
```

### 14.4 QueryNode 回放到 C++

对于 `segment 7001`：

```text
RowIDs     = [90001, 90003]
Timestamps = [t, t]
FieldsData = {
  id        = [101, 103],
  price     = [19.8, 9.9],
  embedding = [vec0, vec2],
}
```

如果这是这个 segment 的第一批数据：

```text
PreInsert(2) -> reserved_offset = 0
```

那么 C++ 内部的逻辑效果是：

```text
offset 0:
  rowID     = 90001
  ts        = t
  pk        = 101
  price     = 19.8
  embedding = vec0

offset 1:
  rowID     = 90003
  ts        = t
  pk        = 103
  price     = 9.9
  embedding = vec2
```

对 `segment 7002` 同理，只是它只有一行。

## 15. 最后把这条链路压缩成一句话

Milvus 插入一条数据时：

1. Proxy 负责校验、分配 RowID、按主键 hash 到某个 `vchannel`
2. StreamingNode 负责给这条写入分配真实 `segmentID`
3. WriteBuffer 负责把这条写入暂存在某个 segment 的内存缓冲里，并在合适时机 flush 到对象存储
4. QueryNode 同时消费同一个 `vchannel`，把同一批 insert 回放到 C++ `Growing Segment`
5. 真正的 C++ 插入点是 `SegmentGrowingImpl::Insert()`

所以：

- **写入最终落盘** 看 StreamingNode / WriteBuffer / Flush
- **刚写完能不能查到** 看 QueryNode / Growing Segment / C++ segcore

## 16. 代码地图

| 主题 | 主要代码 |
|------|----------|
| Proxy Insert 入口 | `internal/proxy/impl.go` |
| Insert 任务 | `internal/proxy/task_insert.go` |
| 按 vchannel 分片并写 WAL | `internal/proxy/task_insert_streaming.go` |
| 行转列 / 分包 | `internal/proxy/msg_pack.go` |
| StreamingNode 分配 segment | `internal/streamingnode/server/wal/interceptors/shard/shard_interceptor.go` |
| segment 分配管理 | `internal/streamingnode/server/wal/interceptors/shard/shards/partition_manager.go` |
| 新建 growing segment | `internal/streamingnode/server/wal/interceptors/shard/shards/segment_alloc_worker.go` |
| segment seal/flush worker | `internal/streamingnode/server/wal/interceptors/shard/shards/segment_flush_worker.go` |
| seal 策略 | `internal/streamingnode/server/wal/interceptors/shard/stats/stats_manager.go` |
| WriteBuffer sync policy | `internal/flushcommon/writebuffer/sync_policy.go` |
| WriteBuffer 主逻辑 | `internal/flushcommon/writebuffer/write_buffer.go` |
| QueryNode watch channel | `internal/querynodev2/services.go` |
| QueryNode insert pipeline | `internal/querynodev2/pipeline/insert_node.go` |
| QueryNode 回放 insert | `internal/querynodev2/delegator/delegator_data.go` |
| InsertMsg -> InsertRecord | `internal/storage/utils.go` |
| Go -> C++ segment bridge | `internal/querynodev2/segments/segment.go` |
| CGO insert 封装 | `internal/util/segcore/segment.go` |
| C 入口 | `internal/core/src/segcore/segment_c.cpp` |
| C++ growing segment insert | `internal/core/src/segcore/SegmentGrowingImpl.cpp` |

## 17. 下一篇建议整理什么

如果继续往下整理，最值得单独写的是：

1. **DataNode/IndexBuilder 的 C++ 索引构建链路**
   重点看 `indexcgowrapper.CreateIndex()` -> `internal/core/src/indexbuilder/index_c.cpp` -> `IndexFactory::CreateIndex()` -> `index->Build()`

2. **HNSW 在 Knowhere 里的构建算法**
   重点回答：
   - 图是怎么建的
   - 参数 `M / efConstruction` 分别影响什么
   - build 阶段到底读的是什么输入

这一篇先把“插入”理顺。
