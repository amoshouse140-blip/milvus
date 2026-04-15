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


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 980 460" width="980" height="460" role="img" aria-label="Segment 生命周期">
  <title>Segment 生命周期</title>
  <desc>Segment 生命周期</desc>
  <defs>
    <marker id="arrow-1" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-1" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-1);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="980" height="460" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">Segment 生命周期</text><text x="48" y="84" class="subtitle">从写入到可查询，状态逐步演进</text>
<g filter="url(#shadow-1)">
<rect x="120" y="150" width="180" height="190" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="120" y="150" width="180" height="10" rx="22" fill="#2762d8"/>
<text x="144" y="190" class="card-title" style="font-size:19px">Growing</text>
<text x="144" y="222" class="card-body">内存中接收新写入</text>
<text x="144" y="246" class="card-body">搜索走 brute force</text>
</g>
<path d="M300,245 L340,245" class="arrow"/>
<text x="320.0" y="235" text-anchor="middle" class="small">达到阈值</text>
<g filter="url(#shadow-1)">
<rect x="340" y="150" width="180" height="190" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="340" y="150" width="180" height="10" rx="22" fill="#6f3dc1"/>
<text x="364" y="190" class="card-title" style="font-size:19px">Sealed</text>
<text x="364" y="222" class="card-body">达到阈值后封存</text>
<text x="364" y="246" class="card-body">不再接受新写入</text>
</g>
<path d="M520,245 L560,245" class="arrow"/>
<text x="540.0" y="235" text-anchor="middle" class="small">Flush</text>
<g filter="url(#shadow-1)">
<rect x="560" y="150" width="180" height="190" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="560" y="150" width="180" height="10" rx="22" fill="#b96f00"/>
<text x="584" y="190" class="card-title" style="font-size:19px">Flushed</text>
<text x="584" y="222" class="card-body">binlog 已写入对象存储</text>
<text x="584" y="246" class="card-body">可以触发索引构建</text>
</g>
<path d="M740,245 L780,245" class="arrow"/>
<text x="760.0" y="235" text-anchor="middle" class="small">构建完成</text>
<g filter="url(#shadow-1)">
<rect x="780" y="150" width="180" height="190" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="780" y="150" width="180" height="10" rx="22" fill="#157f52"/>
<text x="804" y="190" class="card-title" style="font-size:19px">Search On Index</text>
<text x="804" y="222" class="card-body">索引构建完成并被 QueryNode 加载</text>
<text x="804" y="246" class="card-body">后续搜索优先走向量索引</text>
</g>
<rect x="402" y="392" width="180" height="40" rx="20" fill="#157f52"/><text x="492.0" y="416.0" text-anchor="middle" class="chip" style="fill:#ffffff">QueryNode 加载索引后对外可见</text>
</svg>


</div>


### Segment 层级


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 940 560" width="940" height="560" role="img" aria-label="Segment 层级">
  <title>Segment 层级</title>
  <desc>Segment 层级</desc>
  <defs>
    <marker id="arrow-2" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-2" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-2);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="940" height="560" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">Segment 层级</text><text x="48" y="84" class="subtitle">L0 / L1 / L2 在职责上是分层的</text>
<rect x="110" y="150" width="720" height="100" rx="24" fill="#fff1ef" stroke="#b5483c" stroke-width="1.8"/>
<text x="134" y="190" class="card-title" style="fill:#b5483c">L0 · 删除层</text>
<text x="134" y="222" class="card-body">仅存删除记录（PK + Timestamp）</text>
<text x="134" y="246" class="card-body">常驻内存，搜索时用于过滤已删除行</text>
<rect x="110" y="270" width="720" height="100" rx="24" fill="#eef4ff" stroke="#2762d8" stroke-width="1.8"/>
<text x="134" y="310" class="card-title" style="fill:#2762d8">L1 · 标准 Flush 段</text>
<text x="134" y="342" class="card-body">由 Flush 直接产出</text>
<text x="134" y="366" class="card-body">包含 insert binlog、stats、delta</text>
<rect x="110" y="390" width="720" height="100" rx="24" fill="#edf9f3" stroke="#157f52" stroke-width="1.8"/>
<text x="134" y="430" class="card-title" style="fill:#157f52">L2 · Compaction 大段</text>
<text x="134" y="462" class="card-body">由 Compaction 合并生成</text>
<text x="134" y="486" class="card-body">数据更紧凑，删除已被吸收</text>
<path d="M470,252 L470,268" class="arrow"/>
<path d="M470,372 L470,388" class="arrow"/>
</svg>


</div>


---

## 二、系统架构

Milvus 采用**存算分离**架构：Go 负责分布式协调和业务编排，C++ 负责段内向量执行，Rust 用于部分索引集成（tantivy）。

### 2.1 组件总图


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1320 1030" width="1320" height="1030" role="img" aria-label="Milvus 组件总图">
  <title>Milvus 组件总图</title>
  <desc>Milvus 组件总图</desc>
  <defs>
    <marker id="arrow-3" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-3" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-3);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1320" height="1030" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">Milvus 组件总图</text><text x="48" y="84" class="subtitle">存算分离架构：控制面、执行面、存储面协同工作</text>
<g filter="url(#shadow-3)">
<rect x="500" y="110" width="320" height="96" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="500" y="110" width="320" height="10" rx="22" fill="#475467"/>
<text x="524" y="150" class="card-title" style="font-size:19px">Client SDK</text>
<text x="524" y="182" class="card-body">Python / Java / Go / REST</text>
</g>
<g filter="url(#shadow-3)">
<rect x="470" y="250" width="380" height="110" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="470" y="250" width="380" height="10" rx="22" fill="#2762d8"/>
<text x="494" y="290" class="card-title" style="font-size:19px">Proxy</text>
<text x="494" y="322" class="card-body">统一入口</text>
<text x="494" y="346" class="card-body">参数校验、路由、结果归并</text>
</g>
<rect x="60" y="430" width="360" height="360" rx="24" fill="#f3edff" stroke="#6f3dc1" stroke-width="1.6"/><text x="84" y="464" class="card-title" style="font-size:18px;fill:#6f3dc1">MixCoord · 控制平面</text>
<g filter="url(#shadow-3)">
<rect x="84" y="486" width="312" height="74" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="84" y="486" width="312" height="10" rx="22" fill="#6f3dc1"/>
<text x="108" y="526" class="card-title" style="font-size:19px">RootCoord</text>
<text x="108" y="558" class="card-body">DDL、Schema、ID、时间戳</text>
</g>
<g filter="url(#shadow-3)">
<rect x="84" y="576" width="312" height="74" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="84" y="576" width="312" height="10" rx="22" fill="#6f3dc1"/>
<text x="108" y="616" class="card-title" style="font-size:19px">DataCoord</text>
<text x="108" y="648" class="card-body">Segment 生命周期、Flush、索引调度</text>
</g>
<g filter="url(#shadow-3)">
<rect x="84" y="666" width="312" height="74" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="84" y="666" width="312" height="10" rx="22" fill="#6f3dc1"/>
<text x="108" y="706" class="card-title" style="font-size:19px">QueryCoord</text>
<text x="108" y="738" class="card-body">Load / Release、分配、均衡</text>
</g>
<g filter="url(#shadow-3)">
<rect x="84" y="756" width="312" height="74" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="84" y="756" width="312" height="10" rx="22" fill="#6f3dc1"/>
<text x="108" y="796" class="card-title" style="font-size:19px">StreamingCoord</text>
<text x="108" y="828" class="card-body">WAL 分配与协调</text>
</g>
<g filter="url(#shadow-3)">
<rect x="490" y="460" width="310" height="170" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="490" y="460" width="310" height="10" rx="22" fill="#157f52"/>
<text x="514" y="500" class="card-title" style="font-size:19px">QueryNode</text>
<text x="514" y="532" class="card-body">Go 编排 + C++ segcore</text>
<text x="514" y="556" class="card-body">执行向量搜索与标量过滤</text>
<text x="514" y="580" class="card-body">管理 Growing / Sealed 段</text>
</g>
<g filter="url(#shadow-3)">
<rect x="870" y="460" width="310" height="170" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="870" y="460" width="310" height="10" rx="22" fill="#b96f00"/>
<text x="894" y="500" class="card-title" style="font-size:19px">StreamingNode</text>
<text x="894" y="532" class="card-body">接收 Insert</text>
<text x="894" y="556" class="card-body">持有 WAL、维护写缓冲</text>
<text x="894" y="580" class="card-body">负责 Flush 触发</text>
</g>
<g filter="url(#shadow-3)">
<rect x="680" y="680" width="310" height="130" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="680" y="680" width="310" height="10" rx="22" fill="#b5483c"/>
<text x="704" y="720" class="card-title" style="font-size:19px">DataNode</text>
<text x="704" y="752" class="card-body">执行索引构建、Compaction、Import</text>
</g>
<rect x="360" y="860" width="560" height="120" rx="24" fill="#f4f6f8" stroke="#475467" stroke-width="1.6"/><text x="384" y="894" class="card-title" style="font-size:18px;fill:#475467">外部依赖层</text>
<g filter="url(#shadow-3)">
<rect x="388" y="900" width="156" height="56" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="388" y="900" width="156" height="10" rx="22" fill="#475467"/>
<text x="412" y="940" class="card-title" style="font-size:17px">etcd / TiKV</text>
<text x="412" y="972" class="card-body">元数据</text>
</g>
<g filter="url(#shadow-3)">
<rect x="562" y="900" width="156" height="56" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="562" y="900" width="156" height="10" rx="22" fill="#475467"/>
<text x="586" y="940" class="card-title" style="font-size:17px">MinIO / S3</text>
<text x="586" y="972" class="card-body">binlog / 索引</text>
</g>
<g filter="url(#shadow-3)">
<rect x="736" y="900" width="156" height="56" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="736" y="900" width="156" height="10" rx="22" fill="#475467"/>
<text x="760" y="940" class="card-title" style="font-size:17px">WAL Backend</text>
<text x="760" y="972" class="card-body">消息持久化</text>
</g>
<path d="M660,206 L660,250" class="arrow"/>
<text x="674" y="220.0" class="small">gRPC / HTTP</text>
<path d="M470,320 C470,375.0 240,375.0 240,430" class="arrow"/>
<path d="M610,360 C610,410.0 645,410.0 645,460" class="arrow"/>
<path d="M700,360 C700,410.0 1025,410.0 1025,460" class="arrow"/>
<path d="M720,360 C720,520.0 835,520.0 835,680" class="arrow"/>
<path d="M835,630 L835,680" class="arrow"/>
<path d="M650,630 C650,745.0 640,745.0 640,860" class="arrow"/>
<path d="M1025,630 C1025,745.0 800,745.0 800,860" class="arrow"/>
<path d="M835,810 C835,835.0 700,835.0 700,860" class="arrow"/>
<path d="M420,620 L490,620" class="arrow"/>
<text x="455.0" y="610" text-anchor="middle" class="small">控制面驱动</text>
<path d="M800,545 L870,545" class="arrow"/>
<text x="835.0" y="535" text-anchor="middle" class="small">写入</text>
</svg>


</div>


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


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1280 730" width="1280" height="730" role="img" aria-label="数据流概览">
  <title>数据流概览</title>
  <desc>数据流概览</desc>
  <defs>
    <marker id="arrow-4" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-4" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-4);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1280" height="730" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">数据流概览</text><text x="48" y="84" class="subtitle">写入流与查询流是两条并行但共享元数据的链路</text>
<rect x="50" y="130" width="1180" height="270" rx="24" fill="#eef4ff" stroke="#2762d8" stroke-width="1.6"/><text x="74" y="164" class="card-title" style="font-size:18px;fill:#2762d8">写入流</text>
<g filter="url(#shadow-4)">
<rect x="110" y="220" width="150" height="110" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="110" y="220" width="150" height="10" rx="22" fill="#2762d8"/>
<text x="134" y="260" class="card-title" style="font-size:18px">Client</text>
<text x="134" y="292" class="card-body">Insert 请求</text>
</g>
<path d="M260,275 L320,275" class="arrow"/>
<text x="290.0" y="265" text-anchor="middle" class="small">Hash 分片</text>
<g filter="url(#shadow-4)">
<rect x="320" y="220" width="150" height="110" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="320" y="220" width="150" height="10" rx="22" fill="#2762d8"/>
<text x="344" y="260" class="card-title" style="font-size:18px">Proxy</text>
<text x="344" y="292" class="card-body">校验、分片、发往 WAL</text>
</g>
<path d="M470,275 L550,275" class="arrow"/>
<text x="510.0" y="265" text-anchor="middle" class="small">Append</text>
<g filter="url(#shadow-4)">
<rect x="550" y="220" width="150" height="110" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="550" y="220" width="150" height="10" rx="22" fill="#2762d8"/>
<text x="574" y="260" class="card-title" style="font-size:18px">StreamingNode</text>
<text x="574" y="292" class="card-body">持久化消息</text>
<text x="574" y="316" class="card-body">维护 Growing / WriteBuffer</text>
</g>
<path d="M700,275 L810,275" class="arrow"/>
<text x="755.0" y="265" text-anchor="middle" class="small">Flush</text>
<g filter="url(#shadow-4)">
<rect x="810" y="220" width="150" height="110" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="810" y="220" width="150" height="10" rx="22" fill="#2762d8"/>
<text x="834" y="260" class="card-title" style="font-size:18px">对象存储</text>
<text x="834" y="292" class="card-body">binlog 落盘</text>
</g>
<path d="M960,275 L1040,275" class="arrow"/>
<text x="1000.0" y="265" text-anchor="middle" class="small">Build + Load</text>
<g filter="url(#shadow-4)">
<rect x="1040" y="220" width="150" height="110" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="1040" y="220" width="150" height="10" rx="22" fill="#2762d8"/>
<text x="1064" y="260" class="card-title" style="font-size:18px">DataNode / QueryNode</text>
<text x="1064" y="292" class="card-body">建索引并加载</text>
<text x="1064" y="316" class="card-body">变成高速可查</text>
</g>
<text x="760" y="360" text-anchor="start" class="small">QueryNode 同时消费 WAL</text>
<text x="760" y="382" text-anchor="start" class="small">把实时数据回放到 Growing 段</text>
<rect x="50" y="450" width="1180" height="230" rx="24" fill="#edf9f3" stroke="#157f52" stroke-width="1.6"/><text x="74" y="484" class="card-title" style="font-size:18px;fill:#157f52">查询流</text>
<g filter="url(#shadow-4)">
<rect x="120" y="525" width="170" height="106" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="120" y="525" width="170" height="10" rx="22" fill="#157f52"/>
<text x="144" y="565" class="card-title" style="font-size:18px">Client</text>
<text x="144" y="597" class="card-body">Search 请求</text>
</g>
<path d="M290,578 L350,578" class="arrow"/>
<text x="320.0" y="568" text-anchor="middle" class="small">Plan + Placeholder</text>
<g filter="url(#shadow-4)">
<rect x="350" y="525" width="170" height="106" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="350" y="525" width="170" height="10" rx="22" fill="#157f52"/>
<text x="374" y="565" class="card-title" style="font-size:18px">Proxy</text>
<text x="374" y="597" class="card-body">生成 Plan</text>
<text x="374" y="621" class="card-body">广播到所有 Shard</text>
</g>
<path d="M520,578 L640,578" class="arrow"/>
<text x="580.0" y="568" text-anchor="middle" class="small">广播</text>
<g filter="url(#shadow-4)">
<rect x="640" y="525" width="170" height="106" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="640" y="525" width="170" height="10" rx="22" fill="#157f52"/>
<text x="664" y="565" class="card-title" style="font-size:18px">QueryNode</text>
<text x="664" y="597" class="card-body">各段并行搜索</text>
<text x="664" y="621" class="card-body">ANN + 标量过滤</text>
</g>
<path d="M810,578 L930,578" class="arrow"/>
<text x="870.0" y="568" text-anchor="middle" class="small">跨分片 Reduce</text>
<g filter="url(#shadow-4)">
<rect x="930" y="525" width="170" height="106" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="930" y="525" width="170" height="10" rx="22" fill="#157f52"/>
<text x="954" y="565" class="card-title" style="font-size:18px">Proxy</text>
<text x="954" y="597" class="card-body">归并 TopK</text>
<text x="954" y="621" class="card-body">返回结果</text>
</g>
</svg>


</div>


**写入 vs 查询的关键区别**：

| 维度 | Insert | Search |
|------|--------|--------|
| 路由 | 一行只进一个 shard (Hash PK) | 一条查询广播到所有 shard |
| 数据方向 | Client → Proxy → WAL → Segment → 对象存储 | Client → Proxy → QueryNode → C++ → 归并返回 |
| 返回时机 | WAL 写入成功即返回（异步持久化） | 所有 shard 搜索完成并归并后返回 |

---

## 三、数据与存储

### 3.1 三层存储全景


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1060 720" width="1060" height="720" role="img" aria-label="三层存储全景">
  <title>三层存储全景</title>
  <desc>三层存储全景</desc>
  <defs>
    <marker id="arrow-5" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-5" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-5);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1060" height="720" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">三层存储全景</text><text x="48" y="84" class="subtitle">元数据、对象存储、WAL 各司其职</text>
<rect x="110" y="150" width="840" height="150" rx="24" fill="#f3edff" stroke="#6f3dc1" stroke-width="1.8"/>
<text x="134" y="190" class="card-title" style="fill:#6f3dc1">元数据层 · etcd / TiKV</text>
<text x="134" y="222" class="card-body">Collection Schema、Index 定义、Segment Meta、SegmentIndex</text>
<text x="134" y="246" class="card-body">Session / RBAC 与服务注册</text>
<text x="134" y="270" class="card-body">保存状态和路径，不存向量实体文件</text>
<rect x="110" y="330" width="840" height="170" rx="24" fill="#fff5e8" stroke="#b96f00" stroke-width="1.8"/>
<text x="134" y="370" class="card-title" style="fill:#b96f00">对象存储层 · MinIO / S3</text>
<text x="134" y="402" class="card-body">insert_log: 每字段一组列式 binlog</text>
<text x="134" y="426" class="card-body">stats_log: min/max PK、rowCount</text>
<text x="134" y="450" class="card-body">delta_log: 删除记录</text>
<text x="134" y="474" class="card-body">index: HNSW / IVF / DiskANN 等索引文件</text>
<rect x="110" y="530" width="840" height="120" rx="24" fill="#eef4ff" stroke="#2762d8" stroke-width="1.8"/>
<text x="134" y="570" class="card-title" style="fill:#2762d8">WAL 层 · StreamingNode / Pulsar / Kafka</text>
<text x="134" y="602" class="card-body">Insert / Delete / Flush / CreateSegment 等消息</text>
<text x="134" y="626" class="card-body">按 VChannel 分区并保证顺序</text>
<path d="M530,300 L530,330" class="arrow"/>
<text x="544" y="307.0" class="small">路径、状态写回</text>
<path d="M530,500 L530,530" class="arrow"/>
<text x="544" y="507.0" class="small">消费与回放</text>
</svg>


</div>


**关键点**：元数据（Schema、索引定义、段状态）在 etcd，实际数据文件（binlog、索引文件）在对象存储。两者分开存放。

### 3.2 元数据：存了什么

四类核心元数据，以及它们之间的依赖关系：


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1180 640" width="1180" height="640" role="img" aria-label="元数据依赖关系">
  <title>元数据依赖关系</title>
  <desc>元数据依赖关系</desc>
  <defs>
    <marker id="arrow-6" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-6" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-6);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1180" height="640" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">元数据依赖关系</text><text x="48" y="84" class="subtitle">Collection、Segment、Index 定义与 SegmentIndex 串起完整生命周期</text>
<g filter="url(#shadow-6)">
<rect x="80" y="220" width="220" height="110" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="80" y="220" width="220" height="10" rx="22" fill="#6f3dc1"/>
<text x="104" y="260" class="card-title" style="font-size:19px">Collection Schema</text>
<text x="104" y="292" class="card-body">字段定义、类型、维度</text>
</g>
<g filter="url(#shadow-6)">
<rect x="360" y="220" width="220" height="130" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="360" y="220" width="220" height="10" rx="22" fill="#2762d8"/>
<text x="384" y="260" class="card-title" style="font-size:19px">Segment Meta</text>
<text x="384" y="292" class="card-body">段 ID、状态、行数</text>
<text x="384" y="316" class="card-body">binlog 路径</text>
</g>
<g filter="url(#shadow-6)">
<rect x="640" y="220" width="220" height="110" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="640" y="220" width="220" height="10" rx="22" fill="#157f52"/>
<text x="664" y="260" class="card-title" style="font-size:19px">Index 定义</text>
<text x="664" y="292" class="card-body">FieldID、IndexID</text>
<text x="664" y="316" class="card-body">索引类型与参数</text>
</g>
<g filter="url(#shadow-6)">
<rect x="920" y="220" width="220" height="130" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="920" y="220" width="220" height="10" rx="22" fill="#b5483c"/>
<text x="944" y="260" class="card-title" style="font-size:19px">SegmentIndex</text>
<text x="944" y="292" class="card-body">BuildID、状态</text>
<text x="944" y="316" class="card-body">索引文件路径</text>
</g>
<rect x="108" y="160" width="164" height="40" rx="20" fill="#6f3dc1"/><text x="190.0" y="184.0" text-anchor="middle" class="chip" style="fill:#ffffff">CreateCollection</text>
<rect x="388" y="160" width="164" height="40" rx="20" fill="#2762d8"/><text x="470.0" y="184.0" text-anchor="middle" class="chip" style="fill:#ffffff">Insert / Flush</text>
<rect x="668" y="160" width="164" height="40" rx="20" fill="#157f52"/><text x="750.0" y="184.0" text-anchor="middle" class="chip" style="fill:#ffffff">CreateIndex</text>
<rect x="948" y="160" width="164" height="40" rx="20" fill="#b5483c"/><text x="1030.0" y="184.0" text-anchor="middle" class="chip" style="fill:#ffffff">Build 完成</text>
<path d="M300,275 L360,275" class="arrow"/>
<text x="330.0" y="265" text-anchor="middle" class="small">Insert 触发</text>
<path d="M580,285 L640,285" class="arrow"/>
<text x="610.0" y="275" text-anchor="middle" class="small">定义索引</text>
<path d="M860,285 L920,285" class="arrow"/>
<text x="890.0" y="275" text-anchor="middle" class="small">组合生成</text>
<path d="M1030,350 L1030,470" class="arrow"/>
<text x="1044" y="402.0" class="small">索引文件写入对象存储</text>
<g filter="url(#shadow-6)">
<rect x="910" y="480" width="230" height="90" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="910" y="480" width="230" height="10" rx="22" fill="#b96f00"/>
<text x="934" y="520" class="card-title" style="font-size:19px">对象存储索引文件</text>
<text x="934" y="552" class="card-body">QueryNode 根据路径加载</text>
</g>
<path d="M490,390 L1030,390" class="arrow"/>
<text x="760.0" y="380" text-anchor="middle" class="small">Flushed Segment + Index 定义</text>
<text x="360" y="540" text-anchor="start" class="card-body">核心区别：</text>
<text x="360" y="562" text-anchor="start" class="card-body">IndexID 是 collection 级定义；</text>
<text x="360" y="584" text-anchor="start" class="card-body">BuildID 是 segment 级一次构建任务。</text>
</svg>


</div>


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


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 860 700" width="860" height="700" role="img" aria-label="Binlog 文件内部结构">
  <title>Binlog 文件内部结构</title>
  <desc>Binlog 文件内部结构</desc>
  <defs>
    <marker id="arrow-7" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-7" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-7);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="860" height="700" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">Binlog 文件内部结构</text><text x="48" y="84" class="subtitle">事件式封装，payload 为列式编码数据</text>
<rect x="170" y="170" width="520" height="96" rx="24" fill="#f4f6f8" stroke="#475467" stroke-width="1.8"/>
<text x="194" y="210" class="card-title" style="fill:#475467">Magic Number</text>
<text x="194" y="242" class="card-body">0xfffabc</text>
<rect x="170" y="290" width="520" height="96" rx="24" fill="#eef4ff" stroke="#2762d8" stroke-width="1.8"/>
<text x="194" y="330" class="card-title" style="fill:#2762d8">Descriptor Event</text>
<text x="194" y="362" class="card-body">Timestamp</text>
<text x="194" y="386" class="card-body">TypeCode</text>
<rect x="170" y="410" width="520" height="118" rx="24" fill="#edf9f3" stroke="#157f52" stroke-width="1.8"/>
<text x="194" y="450" class="card-title" style="fill:#157f52">Data Event</text>
<text x="194" y="482" class="card-body">Insert / Delete</text>
<text x="194" y="506" class="card-body">payload = Arrow / Parquet</text>
<text x="194" y="530" class="card-body">存放列式字段数据</text>
<rect x="170" y="552" width="520" height="96" rx="24" fill="#fff5e8" stroke="#b96f00" stroke-width="1.8"/>
<text x="194" y="592" class="card-title" style="fill:#b96f00">More Events</text>
<text x="194" y="624" class="card-body">继续追加更多事件</text>
</svg>


</div>


实现：`internal/storage/binlog_writer.go`，数据序列化使用 Arrow/Parquet 格式。

### 3.5 向量在内存中的形态


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1360 740" width="1360" height="740" role="img" aria-label="向量在内存中的形态">
  <title>向量在内存中的形态</title>
  <desc>向量在内存中的形态</desc>
  <defs>
    <marker id="arrow-8" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-8" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-8);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1360" height="740" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">向量在内存中的形态</text><text x="48" y="84" class="subtitle">Growing 段重在实时写入，Sealed 段重在高效搜索</text>
<rect x="50" y="130" width="600" height="560" rx="24" fill="#eef4ff" stroke="#2762d8" stroke-width="1.6"/><text x="74" y="164" class="card-title" style="font-size:18px;fill:#2762d8">Growing Segment</text>
<g filter="url(#shadow-8)">
<rect x="90" y="200" width="520" height="270" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="90" y="200" width="520" height="10" rx="22" fill="#2762d8"/>
<text x="114" y="240" class="card-title" style="font-size:19px">InsertRecord / SegmentGrowingImpl</text>
<text x="114" y="272" class="card-body">id: [101, 103]</text>
<text x="114" y="296" class="card-body">title: [&quot;red mug&quot;, &quot;green tea&quot;]</text>
<text x="114" y="320" class="card-body">price: [19.8, 9.9]</text>
<text x="114" y="344" class="card-body">embedding: [vec0, vec1] 连续存放</text>
<text x="114" y="368" class="card-body">RowIDs / Timestamps 额外维护</text>
</g>
<g filter="url(#shadow-8)">
<rect x="90" y="500" width="520" height="130" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="90" y="500" width="520" height="10" rx="22" fill="#2762d8"/>
<text x="114" y="540" class="card-title" style="font-size:19px">运行特征</text>
<text x="114" y="572" class="card-body">无索引，搜索走 brute force</text>
<text x="114" y="596" class="card-body">由 C++ segcore 直接管理</text>
</g>
<rect x="710" y="130" width="600" height="560" rx="24" fill="#edf9f3" stroke="#157f52" stroke-width="1.6"/><text x="734" y="164" class="card-title" style="font-size:18px;fill:#157f52">Sealed Segment</text>
<g filter="url(#shadow-8)">
<rect x="750" y="200" width="520" height="170" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="750" y="200" width="520" height="10" rx="22" fill="#157f52"/>
<text x="774" y="240" class="card-title" style="font-size:19px">字段与索引共存</text>
<text x="774" y="272" class="card-body">标量字段可在内存或 mmap 中</text>
<text x="774" y="296" class="card-body">向量字段对应 HNSW / IVF 等索引</text>
<text x="774" y="320" class="card-body">搜索优先走 SearchOnIndex</text>
</g>
<g filter="url(#shadow-8)">
<rect x="790" y="410" width="440" height="160" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="790" y="410" width="440" height="10" rx="22" fill="#b5483c"/>
<text x="814" y="450" class="card-title" style="font-size:19px">HNSW 图</text>
<text x="814" y="482" class="card-body">Layer 2: 少量导航点</text>
<text x="814" y="506" class="card-body">Layer 1: 中间层连接</text>
<text x="814" y="530" class="card-body">Layer 0: 全量向量节点</text>
</g>
<g filter="url(#shadow-8)">
<rect x="790" y="590" width="440" height="70" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="790" y="590" width="440" height="10" rx="22" fill="#b96f00"/>
<text x="814" y="630" class="card-title" style="font-size:18px">删除位图</text>
<text x="814" y="662" class="card-body">来自 L0 delta，搜索时过滤</text>
</g>
<path d="M650,410 L710,410" class="arrow"/>
<text x="680.0" y="400" text-anchor="middle" class="small">Flush + Load 后切换</text>
</svg>


</div>


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


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1380 700" width="1380" height="700" role="img" aria-label="插入端到端全景">
  <title>插入端到端全景</title>
  <desc>插入端到端全景</desc>
  <defs>
    <marker id="arrow-9" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-9" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-9);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1380" height="700" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">插入端到端全景</text><text x="48" y="84" class="subtitle">从 Proxy 到 WAL，再到 Flush、建索引、可查询</text>
<g filter="url(#shadow-9)">
<rect x="70" y="200" width="170" height="130" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="70" y="200" width="170" height="10" rx="22" fill="#2762d8"/>
<text x="94" y="240" class="card-title" style="font-size:18px">① Proxy</text>
<text x="94" y="272" class="card-body">校验 Schema</text>
<text x="94" y="296" class="card-body">分配 RowID</text>
<text x="94" y="320" class="card-body">Hash 到 VChannel</text>
</g>
<g filter="url(#shadow-9)">
<rect x="290" y="200" width="150" height="100" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="290" y="200" width="150" height="10" rx="22" fill="#6f3dc1"/>
<text x="314" y="240" class="card-title" style="font-size:18px">② WAL</text>
<text x="314" y="272" class="card-body">按 channel 持久化消息</text>
</g>
<g filter="url(#shadow-9)">
<rect x="490" y="180" width="190" height="170" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="490" y="180" width="190" height="10" rx="22" fill="#b96f00"/>
<text x="514" y="220" class="card-title" style="font-size:18px">③ StreamingNode</text>
<text x="514" y="252" class="card-body">分配 SegmentID</text>
<text x="514" y="276" class="card-body">WriteBuffer 缓冲</text>
</g>
<g filter="url(#shadow-9)">
<rect x="740" y="180" width="190" height="170" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="740" y="180" width="190" height="10" rx="22" fill="#157f52"/>
<text x="764" y="220" class="card-title" style="font-size:18px">④ QueryNode</text>
<text x="764" y="252" class="card-body">同步消费 WAL</text>
<text x="764" y="276" class="card-body">回放到 Growing 段</text>
</g>
<g filter="url(#shadow-9)">
<rect x="980" y="200" width="180" height="110" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="980" y="200" width="180" height="10" rx="22" fill="#475467"/>
<text x="1004" y="240" class="card-title" style="font-size:18px">⑤ Object Storage</text>
<text x="1004" y="272" class="card-body">Flush 成 binlog</text>
</g>
<g filter="url(#shadow-9)">
<rect x="980" y="390" width="180" height="120" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="980" y="390" width="180" height="10" rx="22" fill="#b5483c"/>
<text x="1004" y="430" class="card-title" style="font-size:18px">⑥ DataNode</text>
<text x="1004" y="462" class="card-body">读取 binlog</text>
<text x="1004" y="486" class="card-body">构建 HNSW</text>
</g>
<g filter="url(#shadow-9)">
<rect x="1210" y="280" width="140" height="130" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="1210" y="280" width="140" height="10" rx="22" fill="#157f52"/>
<text x="1234" y="320" class="card-title" style="font-size:18px">⑦ QueryNode</text>
<text x="1234" y="352" class="card-body">加载索引与 sealed 段</text>
<text x="1234" y="376" class="card-body">对外可查</text>
</g>
<path d="M240,250 L290,250" class="arrow"/>
<text x="265.0" y="240" text-anchor="middle" class="small">Append</text>
<path d="M440,250 L490,250" class="arrow"/>
<text x="465.0" y="240" text-anchor="middle" class="small">路由</text>
<path d="M680,250 L740,250" class="arrow"/>
<text x="710.0" y="240" text-anchor="middle" class="small">同批回放</text>
<path d="M930,250 L980,250" class="arrow"/>
<text x="955.0" y="240" text-anchor="middle" class="small">Flush</text>
<path d="M1070,310 L1070,390" class="arrow"/>
<text x="1084" y="342.0" class="small">发现 Flushed + 有索引定义</text>
<path d="M1160,450 L1210,450" class="arrow"/>
<text x="1185.0" y="440" text-anchor="middle" class="small">加载</text>
<text x="1040" y="550" text-anchor="start" class="card-body">“插入成功”发生在 WAL 持久化成功时，</text>
<text x="1040" y="572" text-anchor="start" class="card-body">不等于已经 Flush 到对象存储。</text>
</svg>


</div>


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


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 980 600" width="980" height="600" role="img" aria-label="VChannel 到 StreamingNode 路由">
  <title>VChannel 到 StreamingNode 路由</title>
  <desc>VChannel 到 StreamingNode 路由</desc>
  <defs>
    <marker id="arrow-10" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-10" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-10);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="980" height="600" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">VChannel 到 StreamingNode 路由</text><text x="48" y="84" class="subtitle">逻辑通道先映射到物理通道，再查 Assignment 找到目标节点</text>
<g filter="url(#shadow-10)">
<rect x="60" y="220" width="240" height="150" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="60" y="220" width="240" height="10" rx="22" fill="#2762d8"/>
<text x="84" y="260" class="card-title" style="font-size:19px">VChannel</text>
<text x="84" y="292" class="card-body">by0_3001v0</text>
<text x="84" y="316" class="card-body">by0_3001v1</text>
</g>
<g filter="url(#shadow-10)">
<rect x="370" y="220" width="240" height="150" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="370" y="220" width="240" height="10" rx="22" fill="#6f3dc1"/>
<text x="394" y="260" class="card-title" style="font-size:19px">PChannel</text>
<text x="394" y="292" class="card-body">funcutil.ToPhysicalChannel()</text>
<text x="394" y="316" class="card-body">得到 by0</text>
</g>
<g filter="url(#shadow-10)">
<rect x="680" y="220" width="240" height="150" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="680" y="220" width="240" height="10" rx="22" fill="#b96f00"/>
<text x="704" y="260" class="card-title" style="font-size:19px">StreamingNode</text>
<text x="704" y="292" class="card-body">Watcher.Get(by0)</text>
<text x="704" y="316" class="card-body">拿到 Assignment 后建立 gRPC 连接</text>
</g>
<path d="M300,295 L370,295" class="arrow"/>
<text x="335.0" y="285" text-anchor="middle" class="small">截掉 _3001v0 后缀</text>
<path d="M610,295 L680,295" class="arrow"/>
<text x="645.0" y="285" text-anchor="middle" class="small">按 Assignment 路由</text>
<text x="60" y="470" text-anchor="start" class="card-body">同一 PChannel 的多个 VChannel 可以复用同一个 Producer。</text>
<text x="60" y="492" text-anchor="start" class="card-body">PChannel 迁移时，ResumableProducer 会自动重连。</text>
</svg>


</div>


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


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 920 560" width="920" height="560" role="img" aria-label="Seal 与 Flush 的状态转换">
  <title>Seal 与 Flush 的状态转换</title>
  <desc>Seal 与 Flush 的状态转换</desc>
  <defs>
    <marker id="arrow-11" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-11" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-11);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="920" height="560" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">Seal 与 Flush 的状态转换</text><text x="48" y="84" class="subtitle">封存和刷盘是两个动作，先封存再刷盘</text>
<g filter="url(#shadow-11)">
<rect x="70" y="220" width="160" height="130" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="70" y="220" width="160" height="10" rx="22" fill="#2762d8"/>
<text x="94" y="260" class="card-title" style="font-size:19px">Growing</text>
<text x="94" y="292" class="card-body">持续接收写入</text>
</g>
<path d="M230,285 L275,285" class="arrow"/>
<g filter="url(#shadow-11)">
<rect x="275" y="220" width="160" height="130" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="275" y="220" width="160" height="10" rx="22" fill="#6f3dc1"/>
<text x="299" y="260" class="card-title" style="font-size:19px">Sealed</text>
<text x="299" y="292" class="card-body">DataCoord 决定不再写入</text>
</g>
<path d="M435,285 L480,285" class="arrow"/>
<g filter="url(#shadow-11)">
<rect x="480" y="220" width="160" height="130" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="480" y="220" width="160" height="10" rx="22" fill="#b96f00"/>
<text x="504" y="260" class="card-title" style="font-size:19px">Flushing</text>
<text x="504" y="292" class="card-body">WriteBuffer 正在序列化并上传</text>
</g>
<path d="M640,285 L685,285" class="arrow"/>
<g filter="url(#shadow-11)">
<rect x="685" y="220" width="160" height="130" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="685" y="220" width="160" height="10" rx="22" fill="#157f52"/>
<text x="709" y="260" class="card-title" style="font-size:19px">Flushed</text>
<text x="709" y="292" class="card-body">binlog 已落对象存储</text>
</g>
<text x="90" y="440" text-anchor="start" class="card-body">Seal 触发条件：行数上限、存活时间、binlog 文件数、空闲超时。</text>
<text x="90" y="462" text-anchor="start" class="card-body">Flush 触发条件：Buffer 满、手动 Flush、Stale、Segment Sealed 等策略。</text>
</svg>


</div>


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


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1120 820" width="1120" height="820" role="img" aria-label="索引构建触发点">
  <title>索引构建触发点</title>
  <desc>索引构建触发点</desc>
  <defs>
    <marker id="arrow-12" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-12" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-12);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1120" height="820" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">索引构建触发点</text><text x="48" y="84" class="subtitle">Insert 链路和 CreateIndex 链路在 Flushed Segment 处汇合</text>
<rect x="60" y="180" width="420" height="320" rx="24" fill="#eef4ff" stroke="#2762d8" stroke-width="1.6"/><text x="84" y="214" class="card-title" style="font-size:18px;fill:#2762d8">Insert 链路</text>
<g filter="url(#shadow-12)">
<rect x="90" y="240" width="360" height="70" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="90" y="240" width="360" height="10" rx="22" fill="#2762d8"/>
<text x="114" y="280" class="card-title" style="font-size:18px">Proxy → WAL → StreamingNode</text>
<text x="114" y="312" class="card-body">实时写入路径</text>
</g>
<g filter="url(#shadow-12)">
<rect x="90" y="330" width="360" height="70" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="90" y="330" width="360" height="10" rx="22" fill="#2762d8"/>
<text x="114" y="370" class="card-title" style="font-size:18px">Flush → binlog</text>
<text x="114" y="402" class="card-body">Segment 进入 Flushed 状态</text>
</g>
<rect x="640" y="180" width="420" height="320" rx="24" fill="#edf9f3" stroke="#157f52" stroke-width="1.6"/><text x="664" y="214" class="card-title" style="font-size:18px;fill:#157f52">CreateIndex 链路</text>
<g filter="url(#shadow-12)">
<rect x="670" y="240" width="360" height="70" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="670" y="240" width="360" height="10" rx="22" fill="#157f52"/>
<text x="694" y="280" class="card-title" style="font-size:18px">Proxy → DataCoord</text>
<text x="694" y="312" class="card-body">保存 Index 定义</text>
</g>
<g filter="url(#shadow-12)">
<rect x="670" y="330" width="360" height="70" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="670" y="330" width="360" height="10" rx="22" fill="#157f52"/>
<text x="694" y="370" class="card-title" style="font-size:18px">indexInspector</text>
<text x="694" y="402" class="card-body">扫描 Flushed Segment ∩ Index 定义</text>
</g>
<g filter="url(#shadow-12)">
<rect x="370" y="560" width="380" height="90" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="370" y="560" width="380" height="10" rx="22" fill="#b5483c"/>
<text x="394" y="600" class="card-title" style="font-size:19px">DataNode 构建索引</text>
<text x="394" y="632" class="card-body">读取向量 binlog，调用 Knowhere 构建 HNSW，上传 index 文件</text>
</g>
<path d="M270,400 L270,560" class="arrow"/>
<text x="284" y="472.0" class="small">Flushed</text>
<path d="M850,400 L850,560" class="arrow"/>
<text x="864" y="472.0" class="small">发现缺失索引</text>
<text x="120" y="720" text-anchor="start" class="card-body">无论先 Insert 还是先 CreateIndex，最终都会在 indexInspector 处补齐段级构建任务。</text>
</svg>


</div>


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


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1220 700" width="1220" height="700" role="img" aria-label="LoadCollection 后的自动加载循环">
  <title>LoadCollection 后的自动加载循环</title>
  <desc>LoadCollection 后的自动加载循环</desc>
  <defs>
    <marker id="arrow-13" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-13" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-13);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1220" height="700" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">LoadCollection 后的自动加载循环</text><text x="48" y="84" class="subtitle">用户只调用一次，后续新段由后台观察与调度自动接入</text>
<g filter="url(#shadow-13)">
<rect x="90" y="180" width="230" height="100" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="90" y="180" width="230" height="10" rx="22" fill="#2762d8"/>
<text x="114" y="220" class="card-title" style="font-size:19px">LoadCollection</text>
<text x="114" y="252" class="card-body">标记 collection 为已加载</text>
</g>
<g filter="url(#shadow-13)">
<rect x="400" y="150" width="320" height="160" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="400" y="150" width="320" height="10" rx="22" fill="#6f3dc1"/>
<text x="424" y="190" class="card-title" style="font-size:19px">TargetObserver</text>
<text x="424" y="222" class="card-body">周期性调用 DataCoord.GetRecoveryInfoV2()</text>
<text x="424" y="246" class="card-body">刷新 NextTarget：segment 列表 + channel 列表</text>
</g>
<g filter="url(#shadow-13)">
<rect x="820" y="150" width="320" height="160" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="820" y="150" width="320" height="10" rx="22" fill="#157f52"/>
<text x="844" y="190" class="card-title" style="font-size:19px">SegmentChecker</text>
<text x="844" y="222" class="card-body">对比 NextTarget 与实际分布</text>
<text x="844" y="246" class="card-body">生成 Load / Release 任务</text>
</g>
<g filter="url(#shadow-13)">
<rect x="400" y="420" width="320" height="170" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="400" y="420" width="320" height="10" rx="22" fill="#b96f00"/>
<text x="424" y="460" class="card-title" style="font-size:19px">QueryNode</text>
<text x="424" y="492" class="card-body">下载 binlog + index 文件</text>
<text x="424" y="516" class="card-body">构建 Sealed Segment</text>
<text x="424" y="540" class="card-body">加载 HNSW 并注册到 SegmentManager</text>
</g>
<g filter="url(#shadow-13)">
<rect x="820" y="430" width="320" height="120" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="820" y="430" width="320" height="10" rx="22" fill="#b5483c"/>
<text x="844" y="470" class="card-title" style="font-size:19px">释放 Growing Segment</text>
<text x="844" y="502" class="card-body">被对应的 Sealed Segment 替代后释放</text>
</g>
<path d="M320,230 L400,230" class="arrow"/>
<text x="360.0" y="220" text-anchor="middle" class="small">启动观察</text>
<path d="M720,230 L820,230" class="arrow"/>
<text x="770.0" y="220" text-anchor="middle" class="small">生成任务</text>
<path d="M980,310 C980,365.0 560,365.0 560,420" class="arrow"/>
<text x="770.0" y="355.0" text-anchor="middle" class="small">LoadSegments</text>
<path d="M720,505 L820,505" class="arrow"/>
<text x="770.0" y="495" text-anchor="middle" class="small">替代后释放</text>
<path d="M560,420 C560,365.0 560,365.0 560,310" class="arrow dashed"/>
<text x="560.0" y="355.0" text-anchor="middle" class="small">新 segment flush / 索引完成</text>
</svg>


</div>


### 4.9 数据形态变化总表


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1380 620" width="1380" height="620" role="img" aria-label="数据形态变化总表">
  <title>数据形态变化总表</title>
  <desc>数据形态变化总表</desc>
  <defs>
    <marker id="arrow-14" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-14" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-14);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1380" height="620" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">数据形态变化总表</text><text x="48" y="84" class="subtitle">同一批数据沿链路不断变形：行式 → 列式 → WAL → 内存段 → binlog → 索引</text>
<g filter="url(#shadow-14)">
<rect x="50" y="250" width="140" height="160" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="50" y="250" width="140" height="10" rx="22" fill="#475467"/>
<text x="74" y="290" class="card-title" style="font-size:17px">用户 JSON</text>
<text x="74" y="322" class="card-body">{&quot;id&quot;:101,&quot;embedding&quot;:[...]}</text>
</g>
<g filter="url(#shadow-14)">
<rect x="240" y="250" width="140" height="160" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="240" y="250" width="140" height="10" rx="22" fill="#2762d8"/>
<text x="264" y="290" class="card-title" style="font-size:17px">Proxy FieldData</text>
<text x="264" y="322" class="card-body">ids=[101,102,103]</text>
<text x="264" y="346" class="card-body">embeddings=[vec0,vec1,vec2]</text>
</g>
<g filter="url(#shadow-14)">
<rect x="430" y="250" width="140" height="160" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="430" y="250" width="140" height="10" rx="22" fill="#6f3dc1"/>
<text x="454" y="290" class="card-title" style="font-size:17px">InsertMsg</text>
<text x="454" y="322" class="card-body">按 channel 切分列式子集</text>
</g>
<g filter="url(#shadow-14)">
<rect x="620" y="250" width="140" height="160" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="620" y="250" width="140" height="10" rx="22" fill="#b96f00"/>
<text x="644" y="290" class="card-title" style="font-size:17px">WAL Message</text>
<text x="644" y="322" class="card-body">Header + Body</text>
<text x="644" y="346" class="card-body">仍是列式字段数据</text>
</g>
<g filter="url(#shadow-14)">
<rect x="810" y="250" width="140" height="160" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="810" y="250" width="140" height="10" rx="22" fill="#157f52"/>
<text x="834" y="290" class="card-title" style="font-size:17px">Growing / WriteBuffer</text>
<text x="834" y="322" class="card-body">一边进 C++ Growing 段</text>
<text x="834" y="346" class="card-body">一边进 Go WriteBuffer</text>
</g>
<g filter="url(#shadow-14)">
<rect x="1040" y="250" width="140" height="160" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="1040" y="250" width="140" height="10" rx="22" fill="#b5483c"/>
<text x="1064" y="290" class="card-title" style="font-size:17px">binlog / index</text>
<text x="1064" y="322" class="card-body">每字段一个 binlog 文件</text>
<text x="1064" y="346" class="card-body">后续再生成 HNSW 等索引文件</text>
</g>
<g filter="url(#shadow-14)">
<rect x="1210" y="250" width="140" height="160" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="1210" y="250" width="140" height="10" rx="22" fill="#2762d8"/>
<text x="1234" y="290" class="card-title" style="font-size:17px">Sealed Segment</text>
<text x="1234" y="322" class="card-body">QueryNode 加载后可高效搜索</text>
</g>
<path d="M190,330 L240,330" class="arrow"/>
<path d="M380,330 L430,330" class="arrow"/>
<path d="M570,330 L620,330" class="arrow"/>
<path d="M760,330 L810,330" class="arrow"/>
<path d="M950,330 L1040,330" class="arrow"/>
<path d="M1180,330 L1210,330" class="arrow"/>
<text x="150" y="520" text-anchor="start" class="card-body">核心分叉：</text>
<text x="150" y="542" text-anchor="start" class="card-body">QueryNode 回放 WAL 负责实时可查；WriteBuffer Flush 负责持久化；</text>
<text x="150" y="564" text-anchor="start" class="card-body">两条链路最终在 Sealed Segment 上汇合。</text>
</svg>


</div>


---

## 五、搜索端到端流程

### 5.1 全景图

用户搜索：`q=[0.11,0.19,0.31,0.41], filter="price>10", topK=2`


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1420 660" width="1420" height="660" role="img" aria-label="搜索端到端全景">
  <title>搜索端到端全景</title>
  <desc>搜索端到端全景</desc>
  <defs>
    <marker id="arrow-15" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-15" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-15);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1420" height="660" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">搜索端到端全景</text><text x="48" y="84" class="subtitle">Plan 生成、分片执行、两级归并构成一次完整搜索</text>
<g filter="url(#shadow-15)">
<rect x="70" y="200" width="260" height="210" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="70" y="200" width="260" height="10" rx="22" fill="#2762d8"/>
<text x="94" y="240" class="card-title" style="font-size:19px">① Proxy</text>
<text x="94" y="272" class="card-body">解析 Schema</text>
<text x="94" y="296" class="card-body">生成 PlanNode</text>
<text x="94" y="320" class="card-body">编码 PlaceholderGroup</text>
<text x="94" y="344" class="card-body">广播到所有 shard</text>
</g>
<g filter="url(#shadow-15)">
<rect x="420" y="170" width="260" height="260" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="420" y="170" width="260" height="10" rx="22" fill="#157f52"/>
<text x="444" y="210" class="card-title" style="font-size:19px">② QueryNode-A</text>
<text x="444" y="242" class="card-body">Delegator 编排</text>
<text x="444" y="266" class="card-body">Sealed Segment: HNSW 搜索</text>
<text x="444" y="290" class="card-body">Growing Segment: brute force</text>
<text x="444" y="314" class="card-body">标量过滤 + L0 删除过滤</text>
</g>
<g filter="url(#shadow-15)">
<rect x="760" y="170" width="260" height="260" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="760" y="170" width="260" height="10" rx="22" fill="#157f52"/>
<text x="784" y="210" class="card-title" style="font-size:19px">② QueryNode-B</text>
<text x="784" y="242" class="card-body">与 A 相同，但负责另一组 shard</text>
<text x="784" y="266" class="card-body">输出节点内 TopK</text>
</g>
<g filter="url(#shadow-15)">
<rect x="1110" y="220" width="240" height="150" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="1110" y="220" width="240" height="10" rx="22" fill="#b5483c"/>
<text x="1134" y="260" class="card-title" style="font-size:19px">③ Proxy Reduce</text>
<text x="1134" y="292" class="card-body">跨分片 TopK 归并</text>
<text x="1134" y="316" class="card-body">返回全局最优结果</text>
</g>
<path d="M330,305 L420,305" class="arrow"/>
<text x="375.0" y="295" text-anchor="middle" class="small">广播</text>
<path d="M330,330 L760,330" class="arrow"/>
<text x="545.0" y="320" text-anchor="middle" class="small">广播</text>
<path d="M680,305 L1110,305" class="arrow"/>
<text x="895.0" y="295" text-anchor="middle" class="small">节点内 TopK</text>
<path d="M1020,330 L1110,330" class="arrow"/>
<text x="1065.0" y="320" text-anchor="middle" class="small">节点内 TopK</text>
<g filter="url(#shadow-15)">
<rect x="1090" y="460" width="280" height="120" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="1090" y="460" width="280" height="10" rx="22" fill="#475467"/>
<text x="1114" y="500" class="card-title" style="font-size:19px">结果示意</text>
<text x="1114" y="532" class="card-body">rank1: id=101 score=0.0004</text>
<text x="1114" y="556" class="card-body">rank2: id=102 score=0.162</text>
<text x="1114" y="580" class="card-body">L2 距离越小越相似</text>
</g>
</svg>


</div>


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


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1060 780" width="1060" height="780" role="img" aria-label="两级结果归并">
  <title>两级结果归并</title>
  <desc>两级结果归并</desc>
  <defs>
    <marker id="arrow-16" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-16" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-16);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1060" height="780" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">两级结果归并</text><text x="48" y="84" class="subtitle">先段内归并到节点，再由 Proxy 做全局 TopK</text>
<g filter="url(#shadow-16)">
<rect x="80" y="220" width="150" height="90" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="80" y="220" width="150" height="10" rx="22" fill="#2762d8"/>
<text x="104" y="260" class="card-title" style="font-size:18px">Seg 7001</text>
<text x="104" y="292" class="card-body">101 : 0.0004</text>
</g>
<g filter="url(#shadow-16)">
<rect x="80" y="340" width="150" height="90" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="80" y="340" width="150" height="10" rx="22" fill="#2762d8"/>
<text x="104" y="380" class="card-title" style="font-size:18px">Seg 700x</text>
<text x="104" y="412" class="card-body">...</text>
</g>
<g filter="url(#shadow-16)">
<rect x="430" y="260" width="200" height="120" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="430" y="260" width="200" height="10" rx="22" fill="#157f52"/>
<text x="454" y="300" class="card-title" style="font-size:18px">QN-A Reduce</text>
<text x="454" y="332" class="card-body">[(101, 0.0004)]</text>
</g>
<g filter="url(#shadow-16)">
<rect x="80" y="500" width="150" height="90" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="80" y="500" width="150" height="10" rx="22" fill="#6f3dc1"/>
<text x="104" y="540" class="card-title" style="font-size:18px">Seg 7002</text>
<text x="104" y="572" class="card-body">102 : 0.162</text>
</g>
<g filter="url(#shadow-16)">
<rect x="80" y="620" width="150" height="90" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="80" y="620" width="150" height="10" rx="22" fill="#6f3dc1"/>
<text x="104" y="660" class="card-title" style="font-size:18px">Seg 700y</text>
<text x="104" y="692" class="card-body">...</text>
</g>
<g filter="url(#shadow-16)">
<rect x="430" y="540" width="200" height="120" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="430" y="540" width="200" height="10" rx="22" fill="#157f52"/>
<text x="454" y="580" class="card-title" style="font-size:18px">QN-B Reduce</text>
<text x="454" y="612" class="card-body">[(102, 0.162)]</text>
</g>
<g filter="url(#shadow-16)">
<rect x="760" y="390" width="220" height="140" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="760" y="390" width="220" height="10" rx="22" fill="#b5483c"/>
<text x="784" y="430" class="card-title" style="font-size:18px">Proxy Reduce</text>
<text x="784" y="462" class="card-body">全局排序</text>
<text x="784" y="486" class="card-body">取 TopK</text>
<text x="784" y="510" class="card-body">[(101,0.0004),(102,0.162)]</text>
</g>
<path d="M230,265 L430,265" class="arrow"/>
<path d="M230,385 L430,385" class="arrow"/>
<path d="M230,545 L430,545" class="arrow"/>
<path d="M230,665 L430,665" class="arrow"/>
<path d="M630,320 L760,320" class="arrow"/>
<path d="M630,600 L760,600" class="arrow"/>
</svg>


</div>


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


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 960 740" width="960" height="740" role="img" aria-label="HNSW 的多层图结构">
  <title>HNSW 的多层图结构</title>
  <desc>HNSW 的多层图结构</desc>
  <defs>
    <marker id="arrow-17" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-17" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-17);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="960" height="740" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">HNSW 的多层图结构</text><text x="48" y="84" class="subtitle">上层稀疏跳跃，底层稠密保证精度</text>
<rect x="170" y="180" width="620" height="84" rx="24" fill="#f3edff" stroke="#6f3dc1" stroke-width="1.8"/>
<text x="194" y="220" class="card-title" style="fill:#6f3dc1">Layer 3</text>
<text x="194" y="252" class="card-body">极少节点，负责长距离导航</text>
<rect x="150" y="290" width="660" height="92" rx="24" fill="#eef4ff" stroke="#2762d8" stroke-width="1.8"/>
<text x="174" y="330" class="card-title" style="fill:#2762d8">Layer 2</text>
<text x="174" y="362" class="card-body">节点更多，开始收敛到目标区域</text>
<rect x="120" y="410" width="720" height="96" rx="24" fill="#edf9f3" stroke="#157f52" stroke-width="1.8"/>
<text x="144" y="450" class="card-title" style="fill:#157f52">Layer 1</text>
<text x="144" y="482" class="card-body">更密的近邻连接，进一步缩小搜索范围</text>
<rect x="90" y="540" width="780" height="110" rx="24" fill="#fff1ef" stroke="#b5483c" stroke-width="1.8"/>
<text x="114" y="580" class="card-title" style="fill:#b5483c">Layer 0</text>
<text x="114" y="612" class="card-body">包含全部向量节点</text>
<text x="114" y="636" class="card-body">短距离连接最密集，最终在这里拿到 TopK</text>
<path d="M480,264 L480,290" class="arrow"/>
<path d="M480,382 L480,410" class="arrow"/>
<path d="M480,506 L480,540" class="arrow"/>
</svg>


</div>


### 构建过程

1. 每个新向量随机分配一个层级 L（概率指数衰减，大多数在 Layer 0）
2. 从最高层的入口点开始，**贪心搜索**找到距离新向量最近的节点
3. 逐层下降，在每一层都连接 M 个最近邻居
4. 参数 `M` 控制每个节点的连接数，`efConstruction` 控制构建时的搜索宽度

### 搜索过程


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1120 650" width="1120" height="650" role="img" aria-label="HNSW 搜索过程">
  <title>HNSW 搜索过程</title>
  <desc>HNSW 搜索过程</desc>
  <defs>
    <marker id="arrow-18" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-18" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-18);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1120" height="650" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">HNSW 搜索过程</text><text x="48" y="84" class="subtitle">从最高层入口点开始，逐层下探到 Layer 0 做精细搜索</text>
<g filter="url(#shadow-18)">
<rect x="70" y="230" width="190" height="170" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="70" y="230" width="190" height="10" rx="22" fill="#6f3dc1"/>
<text x="94" y="270" class="card-title" style="font-size:18px">Step 1</text>
<text x="94" y="302" class="card-body">Layer 3 入口点</text>
<text x="94" y="326" class="card-body">贪心跳到更近的节点</text>
</g>
<path d="M260,315 L325,315" class="arrow"/>
<text x="292.5" y="305" text-anchor="middle" class="small">下降</text>
<g filter="url(#shadow-18)">
<rect x="325" y="230" width="190" height="170" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="325" y="230" width="190" height="10" rx="22" fill="#2762d8"/>
<text x="349" y="270" class="card-title" style="font-size:18px">Step 2</text>
<text x="349" y="302" class="card-body">下降到 Layer 2</text>
<text x="349" y="326" class="card-body">扩大局部搜索范围</text>
</g>
<path d="M515,315 L580,315" class="arrow"/>
<text x="547.5" y="305" text-anchor="middle" class="small">下降</text>
<g filter="url(#shadow-18)">
<rect x="580" y="230" width="190" height="170" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="580" y="230" width="190" height="10" rx="22" fill="#157f52"/>
<text x="604" y="270" class="card-title" style="font-size:18px">Step 3</text>
<text x="604" y="302" class="card-body">下降到 Layer 1</text>
<text x="604" y="326" class="card-body">继续逼近目标区域</text>
</g>
<path d="M770,315 L835,315" class="arrow"/>
<text x="802.5" y="305" text-anchor="middle" class="small">下降</text>
<g filter="url(#shadow-18)">
<rect x="835" y="230" width="190" height="170" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="835" y="230" width="190" height="10" rx="22" fill="#b5483c"/>
<text x="859" y="270" class="card-title" style="font-size:18px">Step 4</text>
<text x="859" y="302" class="card-body">进入 Layer 0</text>
<text x="859" y="326" class="card-body">用 ef_search 维持候选集</text>
<text x="859" y="350" class="card-body">返回 TopK 最近邻</text>
</g>
<g filter="url(#shadow-18)">
<rect x="360" y="480" width="400" height="100" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="360" y="480" width="400" height="10" rx="22" fill="#475467"/>
<text x="384" y="520" class="card-title" style="font-size:19px">查询向量 q</text>
<text x="384" y="552" class="card-body">[0.11, 0.19, 0.31, 0.41]</text>
</g>
</svg>


</div>


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


<div align="center">

<svg style="max-width: 100%; height: auto;" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1220 620" width="1220" height="620" role="img" aria-label="HNSW 在 Milvus 中的位置">
  <title>HNSW 在 Milvus 中的位置</title>
  <desc>HNSW 在 Milvus 中的位置</desc>
  <defs>
    <marker id="arrow-19" markerWidth="12" markerHeight="12" refX="10" refY="6" orient="auto">
      <path d="M0,0 L12,6 L0,12 Z" fill="#475467"/>
    </marker>
    <filter id="shadow-19" x="-20%" y="-20%" width="140%" height="160%">
      <feDropShadow dx="0" dy="6" stdDeviation="8" flood-color="#b7ad96" flood-opacity="0.15"/>
    </filter>
    <style>
      text {
        font-family: "Avenir Next", "PingFang SC", "Microsoft YaHei", "Segoe UI", sans-serif;
        fill: #1f2937;
      }
      .title {
        font-size: 28px;
        font-weight: 700;
        letter-spacing: 0.2px;
      }
      .subtitle {
        font-size: 15px;
        fill: #667085;
      }
      .card-title {
        font-size: 19px;
        font-weight: 700;
      }
      .card-body {
        font-size: 14px;
      }
      .small {
        font-size: 12px;
        fill: #667085;
      }
      .label {
        font-size: 13px;
        font-weight: 600;
        fill: #667085;
      }
      .chip {
        font-size: 12px;
        font-weight: 700;
      }
      .step {
        font-size: 15px;
        font-weight: 700;
      }
      .mono {
        font-size: 13px;
        font-family: "JetBrains Mono", "SFMono-Regular", Consolas, monospace;
      }
      .arrow {
        stroke: #475467;
        stroke-width: 2.8;
        fill: none;
        marker-end: url(#arrow-19);
      }
      .dashed {
        stroke-dasharray: 8 6;
      }
    </style>
  </defs>
  <rect x="0" y="0" width="1220" height="620" rx="28" fill="#f7f4ed"/>
  <text x="48" y="56" class="title">HNSW 在 Milvus 中的位置</text><text x="48" y="84" class="subtitle">CreateIndex、构建、上传、加载、搜索是一条完整闭环</text>
<g filter="url(#shadow-19)">
<rect x="40" y="240" width="200" height="150" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="40" y="240" width="200" height="10" rx="22" fill="#2762d8"/>
<text x="64" y="280" class="card-title" style="font-size:18px">CreateIndex</text>
<text x="64" y="312" class="card-body">index_type=HNSW</text>
<text x="64" y="336" class="card-body">metric_type=L2</text>
</g>
<g filter="url(#shadow-19)">
<rect x="275" y="240" width="200" height="150" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="275" y="240" width="200" height="10" rx="22" fill="#6f3dc1"/>
<text x="299" y="280" class="card-title" style="font-size:18px">DataCoord</text>
<text x="299" y="312" class="card-body">保存 Index 定义</text>
<text x="299" y="336" class="card-body">indexInspector 扫描 Flushed Segment</text>
</g>
<g filter="url(#shadow-19)">
<rect x="510" y="240" width="200" height="150" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="510" y="240" width="200" height="10" rx="22" fill="#b96f00"/>
<text x="534" y="280" class="card-title" style="font-size:18px">DataNode</text>
<text x="534" y="312" class="card-body">读取向量 binlog</text>
<text x="534" y="336" class="card-body">调用 Knowhere Build</text>
</g>
<g filter="url(#shadow-19)">
<rect x="745" y="240" width="200" height="150" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="745" y="240" width="200" height="10" rx="22" fill="#b5483c"/>
<text x="769" y="280" class="card-title" style="font-size:18px">对象存储</text>
<text x="769" y="312" class="card-body">上传 hnsw_meta / hnsw_graph / hnsw_data</text>
</g>
<g filter="url(#shadow-19)">
<rect x="980" y="240" width="200" height="150" rx="22" fill="#ffffff" stroke="#d8d3c7" stroke-width="1.5"/>
<rect x="980" y="240" width="200" height="10" rx="22" fill="#157f52"/>
<text x="1004" y="280" class="card-title" style="font-size:18px">QueryNode</text>
<text x="1004" y="312" class="card-body">加载索引文件</text>
<text x="1004" y="336" class="card-body">SearchOnIndex 图遍历</text>
</g>
<path d="M240,315 L275,315" class="arrow"/>
<path d="M475,315 L510,315" class="arrow"/>
<path d="M710,315 L745,315" class="arrow"/>
<path d="M945,315 L980,315" class="arrow"/>
<text x="170" y="500" text-anchor="start" class="card-body">最终效果：QueryNode 搜索时走 SearchOnIndex，</text>
<text x="170" y="522" text-anchor="start" class="card-body">而不是对 Sealed Segment 做全量 brute force。</text>
</svg>


</div>

