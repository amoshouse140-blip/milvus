# RAID 卡与 NVMe SSD 故障率、数据不可访问概率、数据完全丢失概率

整理日期：2026-04-13（UTC）

## 1. 结论先看

如果只看公开可核验的“官方数据”，`RAID 卡`与`企业级 NVMe SSD`能拿到的口径并不对称：

- `RAID 卡`公开资料里，最有价值的公开统计分两类：
  - 厂商规格书给 `MTBF`
  - 微软公开 field study 给 `RAID controller AFR`
- `企业级 NVMe SSD`公开资料里，厂商普遍给：
  - `MTTF/MTBF`
  - `UBER`（不可校正读错率）
  - `PLP / PLI / End-to-End Data Protection`
- 但无论是 RAID 卡还是 NVMe SSD，`“故障后导致全部数据永久丢失”的官方统一概率` 基本都没有公开值。公开文档更多是在说明：
  - 设备本体的失效率
  - 读错概率
  - 掉电保护和缓存保护是否存在

因此，本文把问题拆成三层：

1. `设备本体故障率`
2. `故障后导致数据不可访问的概率`
3. `故障后导致数据完全丢失的概率`

其中第 1 层大多可以量化，第 2 层部分可以量化，第 3 层通常只能给出`保守上界`或`无法从公开官方资料直接量化`。

## 2. 统计口径说明

### 2.1 本文采用的口径

- `AFR`：Annualized Failure Rate，年化故障率
- `MTBF/MTTF`：Mean Time Between Failures / Mean Time To Failure
- `UBER`：Uncorrectable Bit Error Rate，不可校正比特错误率
- `数据不可访问`：包括整阵列离线、整盘离线、局部扇区/块不可读
- `数据完全丢失`：假定原始数据无法从原设备直接恢复，且没有其他副本/备份可回放

### 2.2 MTBF/MTTF 换算 AFR 的公式

对厂商给出的 `MTBF/MTTF`，本文统一按指数分布近似换算：

```text
AFR ≈ 1 - exp(-8760 / MTBF)
```

对应示例：

- `2,000,000 小时` -> `0.4370%/年`
- `3,000,000 小时` -> `0.2916%/年`

注意：

- 这是`按官方指标换算`，不是厂商直接声明的 AFR
- 厂商 MTBF/MTTF 多来自实验室/加速测试，不等于真实生产环境 field AFR
- 微软公开 field study 明确指出：某些 SSD 型号在生产环境中的 AFR 可比规格值高 `最多约 70%`

来源：

- Microsoft Research, *SSD Failures in Datacenters: What? When? and Why?*  
  https://www.microsoft.com/en-us/research/publication/ssd-failures-in-datacenters-what-when-and-why/

## 3. RAID 卡公开故障率

### 3.1 公开官方数据汇总

| 类型 | 来源 | 官方原始指标 | 换算/直接值 | 备注 |
|---|---|---:|---:|---|
| RAID 控制器 field study | [Microsoft Research 2010](https://www.microsoft.com/en-us/research/publication/characterizing-cloud-computing-hardware-reliability/) | `0.7% AFR` | `0.7%/年` | 大型云数据中心实测，属于 field AFR，不是厂商实验室值 |
| 新上架 RAID 控制器 field study | [Microsoft Research 2010](https://www.microsoft.com/en-us/research/publication/characterizing-cloud-computing-hardware-reliability/) | `~0.3% AFR (< 3 months)` | `0.3%/年` | 仅针对“新控制器”子样本 |
| Broadcom MegaRAID 9500 | [Broadcom 产品简报](https://docs.broadcom.com/doc/MegaRAID-9500-Tri-Mode-Storage-Adapters) | `MTBF > 3,000,000h @ 40°C` | `< 0.2916%/年` | 厂商规格值，非 field AFR |
| Microchip SmartRAID 3200（部分型号） | [Microchip 产品简报](https://ww1.microchip.com/downloads/aemDocuments/documents/DCS/ProductDocuments/Brochures/Adaptec-SmartRAID-3200-Sell-Sheet-00003270.pdf.pdf) | `MTBF 3.0 million hours @ 40°C` | `0.2916%/年` | 3254U-16e / 3204-8i 等 |
| Microchip SmartRAID 3200（部分型号） | [Microchip 产品简报](https://ww1.microchip.com/downloads/aemDocuments/documents/DCS/ProductDocuments/Brochures/Adaptec-SmartRAID-3200-Sell-Sheet-00003270.pdf.pdf) | `MTBF 2.0 million hours @ 40°C` | `0.4370%/年` | 3258/3254/3252 多个型号 |

### 3.2 对 RAID 卡故障率的合理解读

公开资料里，`RAID 卡最有参考意义的“官方统计值”`其实是微软的大规模 field study：

- 整体 `RAID controller AFR = 0.7%`
- 新控制器（上架不足 3 个月）约 `0.3%`

这比单纯看厂商 `MTBF 2M~3M 小时` 更接近真实运维风险，因为它是生产环境中的实际更换/故障事件统计。

但要注意两点：

- 这份微软研究发表于 `2010-06`，早于 NVMe RAID 普及期，硬件代际较老
- 论文明确说明这些值更接近`replacement/failure event`统计，且对部分组件是`上界近似`

也就是说：

- 如果你想估算现代 RAID 卡的`设计指标`，看厂商 MTBF 更合适
- 如果你想估算`运维层面一年遇到多少张坏卡/换卡`，微软 field AFR 更有现实意义

### 3.3 RAID 卡故障后，“数据不可访问”与“数据完全丢失”的概率

#### 3.3.1 数据不可访问

如果是`单 RAID 卡、单控制器路径`的硬 RAID 架构，控制器故障最常见的后果是：

- 阵列离线
- 服务器无法识别虚拟盘
- 业务数据暂时不可挂载

因此，对“`RAID 卡故障导致数据不可访问`”做粗粒度估算时，可用下面两个口径：

- `field 口径`：约 `0.3% ~ 0.7%/年/卡`
- `厂商 MTBF 口径`：约 `0.29% ~ 0.44%/年/卡`

更偏实战的建议是优先采用 `0.3% ~ 0.7%/年/卡` 作为不可访问风险代理值。

微软同一篇研究还给出一个辅助指标：`6% 的首次服务器硬件故障`是由 RAID controller 触发。这说明控制器故障并不只是“卡坏了要换件”，而是足以成为服务不可用的显著来源。

来源：

- Microsoft Research, *Characterizing Cloud Computing Hardware Reliability*  
  https://www.microsoft.com/en-us/research/publication/characterizing-cloud-computing-hardware-reliability/

#### 3.3.2 数据完全丢失

这一项`无法从公开官方资料直接给出统一数值`。原因是 RAID 卡故障是否演化成“全部数据永久丢失”，取决于下面几个条件：

- 阵列元数据是否落盘
- 是否支持控制器更换后导入配置
- 是否启用了 write-back cache
- write-back cache 是否有掉电保护
- 是否还有备份、副本、快照

官方资料能明确支持的，是下面两件事：

1. `阵列元数据不只在卡上`

Broadcom MegaRAID 9500 官方产品简报明确列出：

- `DDF-compliant Configuration on Disk (COD)`

这说明阵列配置元数据是写在磁盘上的，而不是只保存在控制器板卡易失性状态中。它不能直接等价成“100% 可恢复”，但意味着`控制器损坏并不天然等于磁盘数据被物理抹掉`。

来源：

- Broadcom MegaRAID 9500 产品简报  
  https://docs.broadcom.com/doc/MegaRAID-9500-Tri-Mode-Storage-Adapters

2. `write-back cache 在无缓存保护时确实存在确认写丢失风险`

Broadcom 的 CacheVault 官方资料明确说明：

- 写回缓存把数据先写到控制器 DRAM，再向应用确认完成
- 如果此时掉电，DRAM 中尚未落盘的写入可能丢失
- CacheVault 的作用是把这些缓存内容在掉电时自动转存到 NAND

Microchip SmartRAID 3200 官方资料也写明：

- 多数型号集成基于闪存的缓存备份
- ASCM 电容模块会在掉电时把缓存安全备份到板载 flash

这意味着：

- `无缓存保护 + write-back cache + 突然掉电` 时，存在`已确认写丢失`风险
- `有 CacheVault/ASCM` 时，这条“从卡缓存丢数据”的路径被显式降低

来源：

- Broadcom CacheVault 产品简报  
  https://docs.broadcom.com/doc/BC-0497EN
- Microchip SmartRAID 3200 产品简报  
  https://ww1.microchip.com/downloads/aemDocuments/documents/DCS/ProductDocuments/Brochures/Adaptec-SmartRAID-3200-Sell-Sheet-00003270.pdf.pdf

#### 3.3.3 RAID 卡部分的小结

| 问题 | 是否能用公开官方数据直接量化 | 建议写法 |
|---|---|---|
| RAID 卡本体故障率 | 可以 | `0.3%~0.7%/年`（field AFR），或 `0.29%~0.44%/年`（按 MTBF 换算） |
| RAID 卡故障导致阵列不可访问概率 | 可以粗估 | 单控制器架构下，可近似视为与 RAID 卡年故障率同量级 |
| RAID 卡故障导致全部数据永久丢失概率 | 不可以 | 公开官方资料没有统一数值；通常取决于 COD、缓存保护、备份与恢复流程 |

## 4. 企业级 NVMe SSD 公开故障率

### 4.1 公开官方数据汇总

| 产品 | 来源 | 官方原始指标 | 换算/直接值 | 备注 |
|---|---|---:|---:|---|
| Micron 7450 NVMe SSD | [Micron 规格书](https://www.micron.com/content/dam/micron/global/public/documents/products/technical-marketing-brief/7450-nvme-ssd-tech-prod-spec.pdf) | `MTTF = 2,000,000h` | `0.4370%/年` | 同时给出 `UBER < 1 sector / 10^17 bits read`、`Full power-loss protection` |
| Samsung PM9A3 NVMe SSD | [Samsung 产品简报](https://download.semiconductor.samsung.com/resources/brochure/Samsung%20PM9A3%20NVMe%20PCIe%20SSD.pdf) | `MTBF = 2,000,000h` | `0.4370%/年` | 同时给出 `UBER = 1 sector / 10^17 bits read`、`PLP` |
| Intel / Solidigm D7-P5510 | [Solidigm 托管的 Intel 产品简报](https://www.solidigm.com/content/dam/solidigm/en/site/products/data-center/product-briefs/d7-p5510-product-brief/documents/d7-p5510-series-product-brief.pdf) | `MTBF = 2,000,000h` | `0.4370%/年` | 同时给出 `UBER = 1 sector / 10^17 bits read`、`PLI`、`E2E protection` |

### 4.2 对 NVMe SSD 故障率的合理解读

对于企业级 NVMe SSD，公开官方资料里最常见的是：

- `2,000,000h MTTF/MTBF`
- `10^17 bits` 级别的 UBER
- `PLP/PLI` 与 `End-to-End Data Protection`

因此，如果只问“`单盘一年大概多大概率坏到不可用`”，最常见的公开规格答案就是：

- `约 0.437%/年/盘`

但这仍然是`规格换算值`，不是 field AFR。

### 4.3 公开 field study 对 SSD 的补充提醒

虽然公开官方资料里很难找到“`只针对企业 NVMe SSD`”的大样本 field AFR，但微软在 `2016-08` 发布的 SSD 生产环境研究非常值得参考：

- 样本规模：`超过 50 万块 SSD`
- 时间跨度：`接近 3 年`
- 结论之一：某些型号的生产环境 `AFR` 可比规格值高 `最多约 70%`
- 另一个结论：SSD 相关故障单导致更换的比例高达 `79%`

这份研究并不限定 NVMe，也不是按公开产品型号给出逐盘 AFR，因此不能直接拿来充当“某块 NVMe 盘的官方故障率”。但它非常重要，因为它说明：

- `规格书中的 2M 小时 MTTF 不应直接当作真实生产环境故障率`
- `现场运维事件通常比实验室数字更复杂`

来源：

- Microsoft Research, *SSD Failures in Datacenters: What? When? and Why?*  
  https://www.microsoft.com/en-us/research/publication/ssd-failures-in-datacenters-what-when-and-why/

## 5. NVMe SSD 故障后，“数据不可访问”与“数据完全丢失”的概率

### 5.1 整盘不可访问概率

如果是单块企业级 NVMe SSD，不考虑 RAID/副本，仅从盘本体看：

- `整盘失效导致整盘不可访问` 的年概率，最容易从官方资料量化为  
  `约 0.437%/年/盘`

这是最接近“盘坏了整盘挂掉”的公开量化口径。

### 5.2 局部数据不可访问概率：UBER 视角

UBER 不表示“盘坏了”，而是表示：

- 读了足够多的数据后，遇到至少一次`不可校正读错/不可恢复读`的概率

按厂商常见口径 `1 sector / 10^17 bits read`，可用下面的近似公式：

```text
P(至少一次不可校正读错) ≈ 1 - exp(-已读取比特数 / 10^17)
```

按十进制容量（`1 TB = 10^12 bytes`）计算：

| 累计读取量 | 近似概率 |
|---|---:|
| 1 TB | `0.0080%` |
| 10 TB | `0.0800%` |
| 100 TB | `0.7968%` |
| 1 PB | `7.6884%` |
| 10 PB | `55.0671%` |

这组数字的含义是：

- `UBER 描述的是“读过程中遇到至少一次不可校正错误”的风险`
- 它更接近`局部数据不可访问/读失败`，不是“整盘全部丢失”

### 5.3 数据完全丢失概率

和 RAID 卡一样，这一项`公开官方资料通常不给统一数值`。

原因是“整盘失效”并不总等于“全部数据永久丢失”：

- 有的失效可以通过厂商实验室恢复
- 有的失效只是局部不可读
- 有的系统本来就有 RAID / 镜像 / 副本 / 备份

因此应分场景讨论：

#### 场景 A：单盘、单副本、无备份

这种情况下，若业务数据只有这一份，`整盘 fail-stop` 在业务层面就可能等价于“全部数据永久丢失”。

所以在非常保守的风险建模里，可以把：

- `数据完全丢失的上界`

近似看成与单盘年失效率同量级，也就是：

- `约 0.44%/年/盘`

但要强调：

- 这`不是厂商公开声明的“永久丢失率”`
- 这只是`单副本系统`下非常保守的业务上界

#### 场景 B：有 RAID / 镜像 / 副本 / 备份

这时“全部数据永久丢失”的概率就不再由单盘 `MTTF/MTBF` 决定，而主要由系统层设计决定，例如：

- RAID 级别
- 副本数
- 重建窗口
- 备份频率
- 快照与校验机制

也就是说：

- `单盘 0.437%/年` 只能用来建模“单块盘会不会坏”
- 不能直接用来等同“系统数据永久丢失概率”

### 5.4 PLP / PLI / E2E Protection 对“完全丢失”的意义

公开官方文档能明确说明的是，这些设计会降低某些失效路径：

- `PLP / PLI`：降低突然掉电时，DRAM 缓存或飞行中数据丢失的风险
- `End-to-End Data Protection`：降低链路、控制器、内存、闪存路径上的静默损坏风险

例如：

- Samsung PM9A3 官方文档明确写到，其 `PLP` 架构在意外断电时会用电容能量把 DRAM 中缓存数据转存到 flash
- Micron 7450 明确写 `Full power-loss protection`
- Intel D7-P5510 明确写 `PLI` 和 `industry-leading end-to-end data path protection`

这些信息能支持的结论是：

- `掉电导致已确认写丢失`这条路径被显式压低
- 但它们并不等价于“SSD 永远不会发生全部数据丢失”

## 6. 一页式结论

### 6.1 RAID 卡

| 项目 | 建议结论 |
|---|---|
| 公开官方故障率 | `0.7%/年`（微软 field AFR）；新控制器约 `0.3%/年` |
| 厂商设计指标 | `0.29%~0.44%/年/卡`（按 2M~3M 小时 MTBF 换算） |
| 导致数据不可访问概率 | 单控制器架构下，可近似按 `0.3%~0.7%/年/卡` 估算 |
| 导致数据完全丢失概率 | `无法从公开官方资料直接量化`；通常显著依赖 COD、缓存保护和备份 |

### 6.2 企业级 NVMe SSD

| 项目 | 建议结论 |
|---|---|
| 公开官方故障率 | 主流企业盘规格值通常为 `MTTF/MTBF = 2,000,000h`，换算约 `0.437%/年/盘` |
| 导致整盘不可访问概率 | 可近似按 `0.437%/年/盘` 看待 |
| 导致局部数据不可访问概率 | 由 `UBER` 决定；`100TB` 累计读取量下约 `0.7968%` 遇到至少一次不可校正读错 |
| 导致数据完全丢失概率 | 公开官方资料通常不给直接数值；单盘单副本时，业务保守上界可近似视作与整盘失效同量级；有 RAID/副本/备份时主要由系统级冗余决定 |

## 7. 建议如何在方案评审里引用这些数字

如果你要把这些数字写进内部方案、SLA、预算或风险评审，推荐这样表述：

### 7.1 RAID 卡

```text
根据 Microsoft Research 在大型云数据中心公开的 field study，
RAID controller 的年化故障率约为 0.7%，新控制器约 0.3%。
根据 Broadcom/Microchip 当代硬 RAID 控制器产品简报，设计级
MTBF 多在 200 万到 300 万小时，对应换算 AFR 约 0.29%~0.44%。
在单控制器硬 RAID 架构下，RAID 卡故障更应被建模为“阵列暂时不可访问”
风险，而不是“数据必然永久丢失”风险；永久丢失概率公开资料无统一数值，
需结合缓存保护、阵列元数据落盘方式及备份体系单独评估。
```

### 7.2 NVMe SSD

```text
根据 Micron/Samsung/Solidigm(原 Intel) 等企业级 NVMe SSD 官方规格书，
主流产品 MTTF/MTBF 通常为 200 万小时，对应年化故障率约 0.437%/盘；
同时典型 UBER 为 1 sector per 10^17 bits read。该指标更适合拆成
“整盘不可访问概率”和“局部读错概率”分别建模。对于“数据永久丢失概率”，
公开官方资料通常不给单一值，单盘单副本系统可按与整盘失效同量级的保守上界处理，
而有 RAID/副本/备份的系统应按系统级 MTTDL 或恢复链路评估。
```

## 8. 原始来源清单

1. Microsoft Research, *Characterizing Cloud Computing Hardware Reliability*  
   https://www.microsoft.com/en-us/research/publication/characterizing-cloud-computing-hardware-reliability/

2. Broadcom, *MegaRAID 9500 PCIe Gen 4.0 Tri-Mode Storage Adapters Product Brief*  
   https://docs.broadcom.com/doc/MegaRAID-9500-Tri-Mode-Storage-Adapters

3. Broadcom, *CacheVault Technology Product Brief*  
   https://docs.broadcom.com/doc/BC-0497EN

4. Microchip, *Adaptec SmartRAID 3200 Series Sell Sheet*  
   https://ww1.microchip.com/downloads/aemDocuments/documents/DCS/ProductDocuments/Brochures/Adaptec-SmartRAID-3200-Sell-Sheet-00003270.pdf.pdf

5. Micron, *Micron 7450 SSD Series Technical Product Specification*  
   https://www.micron.com/content/dam/micron/global/public/documents/products/technical-marketing-brief/7450-nvme-ssd-tech-prod-spec.pdf

6. Samsung, *PM9A3 NVMe PCIe SSD Product Brief*  
   https://download.semiconductor.samsung.com/resources/brochure/Samsung%20PM9A3%20NVMe%20PCIe%20SSD.pdf

7. Solidigm, *Intel SSD D7-P5510 Product Brief*  
   https://www.solidigm.com/content/dam/solidigm/en/site/products/data-center/product-briefs/d7-p5510-product-brief/documents/d7-p5510-series-product-brief.pdf

8. Microsoft Research, *SSD Failures in Datacenters: What? When? and Why?*  
   https://www.microsoft.com/en-us/research/publication/ssd-failures-in-datacenters-what-when-and-why/

## 9. 这份文档没有直接给出的内容

下列数字`公开官方资料通常没有单一答案`，所以本文没有伪造精确值：

- “RAID 卡坏了以后，100% 有多大概率会把整个阵列数据永久搞没”
- “企业级 NVMe SSD 单盘永久数据丢失的官方统一概率”
- “某个 RAID 级别在某个盘数、某个重建时间下的系统级永久丢失概率”

如果需要下一步继续细化，建议在此文档基础上再算一层系统级模型：

- `RAID1 / RAID5 / RAID6 / RAID10`
- `盘数`
- `单盘 AFR`
- `UBER`
- `重建时间`
- `是否有热备盘`
- `是否有副本或备份`

这样才能得到真正接近生产环境的：

- `系统不可访问概率`
- `系统永久数据丢失概率`
