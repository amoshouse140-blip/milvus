package proxy

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/distributed/streaming"
	"github.com/milvus-io/milvus/internal/util/hookutil"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/mq/msgstream"
	"github.com/milvus-io/milvus/pkg/v2/streaming/util/message"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/timerecord"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

// Execute 是 insertTask 的核心执行方法，负责将数据写入流式 WAL。
// 整体流程：
//   1. 从 MetaCache 获取 CollectionID 和 VChannel 列表
//   2. 根据主键 Hash 值将数据路由到不同 VChannel（分片）
//   3. 按分区和 Channel 将数据打包为 InsertMessage
//   4. 通过 streaming.WAL().AppendMessages() 将消息写入 StreamingNode 的 WAL
//   5. WAL 写入成功后返回 MaxTimeTick，用于会话一致性保证
//
// 关于分片路由：
//   每个 Collection 有 N 个 VChannel，数据通过 PK Hash % N 分配到不同 Channel。
//   每个 VChannel 对应一个 StreamingNode 上的 WAL 实例。
//   这种设计保证相同主键的数据总是落在同一个 Channel 上，便于后续去重和一致性维护。
func (it *insertTask) Execute(ctx context.Context) error {
	ctx, sp := otel.Tracer(typeutil.ProxyRole).Start(ctx, "Proxy-Insert-Execute")
	defer sp.End()

	tr := timerecord.NewTimeRecorder(fmt.Sprintf("proxy execute insert streaming %d", it.ID()))

	collectionName := it.insertMsg.CollectionName
	collID, err := globalMetaCache.GetCollectionID(it.ctx, it.insertMsg.GetDbName(), collectionName)
	log := log.Ctx(ctx)
	if err != nil {
		log.Warn("fail to get collection id", zap.Error(err))
		return err
	}
	it.insertMsg.CollectionID = collID

	getCacheDur := tr.RecordSpan()
	// 【获取 VChannel 列表】VChannel (Virtual Channel) 是逻辑数据通道
	// 每个 Collection 在创建时被分配固定数量的 VChannel
	// VChannel 与 PChannel (Physical Channel) 是多对一映射
	channelNames, err := it.chMgr.getVChannels(collID)
	if err != nil {
		log.Warn("get vChannels failed", zap.Int64("collectionID", collID), zap.Error(err))
		it.result.Status = merr.Status(err)
		return err
	}

	log.Info("[TRACE-INSERT] Step4: Execute 获取 VChannel 完成, 准备分片路由",
		zap.String("partition", it.insertMsg.GetPartitionName()),
		zap.Int64("collectionID", collID),
		zap.Strings("virtual_channels", channelNames),
		zap.Int("channelCount", len(channelNames)),
		zap.Int64("task_id", it.ID()),
		zap.Uint64("numRows", it.insertMsg.NumRows),
		zap.Bool("is_partition_key", it.partitionKeys != nil),
		zap.Duration("get_cache_duration", getCacheDur))

	var ez *message.CipherConfig
	if hookutil.IsClusterEncryptionEnabled() {
		ez = hookutil.GetEzByCollProperties(it.schema.GetProperties(), it.collectionID).AsMessageConfig()
	}

	// 【数据分片打包】根据是否使用 Partition Key 选择不同的打包策略：
	// - 无 Partition Key: 按 PK Hash 路由到 Channel，所有数据写入同一个 Partition
	// - 有 Partition Key: 先按 PK Hash 路由到 Channel，再按 Partition Key Hash 二次路由到子 Partition
	// 打包结果是一组 MutableMessage，每个 Message 对应一个 (Channel, Partition) 组合
	var msgs []message.MutableMessage
	if it.partitionKeys == nil {
		msgs, err = repackInsertDataForStreamingService(it.TraceCtx(), channelNames, it.insertMsg, it.result, ez)
	} else {
		msgs, err = repackInsertDataWithPartitionKeyForStreamingService(it.TraceCtx(), channelNames, it.insertMsg, it.result, it.partitionKeys, ez)
	}
	if err != nil {
		log.Warn("assign segmentID and repack insert data failed", zap.Error(err))
		it.result.Status = merr.Status(err)
		return err
	}
	log.Info("[TRACE-INSERT] Step5: 数据分片打包完成, 准备写入 WAL",
		zap.Int("messageCount", len(msgs)),
		zap.Int64("collectionID", collID),
	)
	resp := streaming.WAL().AppendMessages(ctx, msgs...)
	if err := resp.UnwrapFirstError(); err != nil {
		log.Warn("[TRACE-INSERT] WAL 写入失败", zap.Error(err))
		it.result.Status = merr.Status(err)
	}
	log.Info("[TRACE-INSERT] Step6: WAL 写入成功",
		zap.Uint64("maxTimeTick", resp.MaxTimeTick()),
		zap.Int("messageCount", len(msgs)),
	)
	// Update result.Timestamp for session consistency.
	it.result.Timestamp = resp.MaxTimeTick()
	return nil
}

// repackInsertDataForStreamingService 将插入数据按 PK Hash 分片到不同 VChannel。
// 流程：
//   1. assignChannelsByPK: 对每行的主键计算 Hash，映射到 Channel 索引
//   2. 按 Channel 分组后，调用 genInsertMsgsByPartition 打包为 InsertMsg
//   3. 每个 InsertMsg 封装为 streaming Message，附加 VChannel 和 Partition 元信息
//   4. 如果单个 Message 过大（超过 MaxMessageSize），会被拆分成多个小 Message
func repackInsertDataForStreamingService(
	ctx context.Context,
	channelNames []string,
	insertMsg *msgstream.InsertMsg,
	result *milvuspb.MutationResult,
	ez *message.CipherConfig,
) ([]message.MutableMessage, error) {
	messages := make([]message.MutableMessage, 0)

	// 【PK Hash 分片】将主键 Hash 到 Channel，返回 map[channelName][]rowOffset
	channel2RowOffsets := assignChannelsByPK(result.IDs, channelNames, insertMsg)
	partitionName := insertMsg.PartitionName
	partitionID, err := globalMetaCache.GetPartitionID(ctx, insertMsg.GetDbName(), insertMsg.CollectionName, partitionName)
	if err != nil {
		return nil, err
	}

	for channel, rowOffsets := range channel2RowOffsets {
		log.Ctx(ctx).Info("[TRACE-INSERT] 分片路由详情: PK Hash 分配到 Channel",
			zap.String("channel", channel),
			zap.Int("rowCount", len(rowOffsets)),
			zap.Int64("partitionID", partitionID),
			zap.String("partitionName", partitionName),
		)
		// segment id is assigned at streaming node.
		msgs, err := genInsertMsgsByPartition(ctx, 0, partitionID, partitionName, rowOffsets, channel, insertMsg)
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			insertRequest := msg.(*msgstream.InsertMsg).InsertRequest
			newMsg, err := message.NewInsertMessageBuilderV1().
				WithVChannel(channel).
				WithHeader(&message.InsertMessageHeader{
					CollectionId: insertMsg.CollectionID,
					Partitions: []*message.PartitionSegmentAssignment{
						{
							PartitionId: partitionID,
							Rows:        insertRequest.GetNumRows(),
							BinarySize:  0, // TODO: current not used, message estimate size is used.
						},
					},
				}).
				WithBody(insertRequest).
				WithCipher(ez).
				BuildMutable()
			if err != nil {
				return nil, err
			}
			messages = append(messages, newMsg)
		}
	}
	return messages, nil
}

// repackInsertDataWithPartitionKeyForStreamingService 在 Partition Key 模式下的分片打包。
// 与普通模式的区别：数据先按 PK Hash 路由到 Channel，再按 Partition Key Hash 二次路由到子 Partition。
// 这使得相同 Partition Key 的数据总是落在同一个 Partition 中，优化按 Partition Key 过滤查询的性能。
func repackInsertDataWithPartitionKeyForStreamingService(
	ctx context.Context,
	channelNames []string,
	insertMsg *msgstream.InsertMsg,
	result *milvuspb.MutationResult,
	partitionKeys *schemapb.FieldData,
	ez *message.CipherConfig,
) ([]message.MutableMessage, error) {
	messages := make([]message.MutableMessage, 0)

	channel2RowOffsets := assignChannelsByPK(result.IDs, channelNames, insertMsg)
	partitionNames, err := getDefaultPartitionsInPartitionKeyMode(ctx, insertMsg.GetDbName(), insertMsg.CollectionName)
	if err != nil {
		log.Ctx(ctx).Warn("get default partition names failed in partition key mode",
			zap.String("collectionName", insertMsg.CollectionName),
			zap.Error(err))
		return nil, err
	}

	// Get partition ids
	partitionIDs := make(map[string]int64, 0)
	for _, partitionName := range partitionNames {
		partitionID, err := globalMetaCache.GetPartitionID(ctx, insertMsg.GetDbName(), insertMsg.CollectionName, partitionName)
		if err != nil {
			log.Ctx(ctx).Warn("get partition id failed",
				zap.String("collectionName", insertMsg.CollectionName),
				zap.String("partitionName", partitionName),
				zap.Error(err))
			return nil, err
		}
		partitionIDs[partitionName] = partitionID
	}

	hashValues, err := typeutil.HashKey2Partitions(partitionKeys, partitionNames)
	if err != nil {
		log.Ctx(ctx).Warn("has partition keys to partitions failed",
			zap.String("collectionName", insertMsg.CollectionName),
			zap.Error(err))
		return nil, err
	}
	for channel, rowOffsets := range channel2RowOffsets {
		partition2RowOffsets := make(map[string][]int)
		for _, idx := range rowOffsets {
			partitionName := partitionNames[hashValues[idx]]
			if _, ok := partition2RowOffsets[partitionName]; !ok {
				partition2RowOffsets[partitionName] = []int{}
			}
			partition2RowOffsets[partitionName] = append(partition2RowOffsets[partitionName], idx)
		}

		for partitionName, rowOffsets := range partition2RowOffsets {
			msgs, err := genInsertMsgsByPartition(ctx, 0, partitionIDs[partitionName], partitionName, rowOffsets, channel, insertMsg)
			if err != nil {
				return nil, err
			}
			for _, msg := range msgs {
				insertRequest := msg.(*msgstream.InsertMsg).InsertRequest
				newMsg, err := message.NewInsertMessageBuilderV1().
					WithVChannel(channel).
					WithHeader(&message.InsertMessageHeader{
						CollectionId: insertMsg.CollectionID,
						Partitions: []*message.PartitionSegmentAssignment{
							{
								PartitionId: partitionIDs[partitionName],
								Rows:        insertRequest.GetNumRows(),
								BinarySize:  0, // TODO: current not used, message estimate size is used.
							},
						},
					}).
					WithBody(insertRequest).
					WithCipher(ez).
					BuildMutable()
				if err != nil {
					return nil, err
				}
				messages = append(messages, newMsg)
			}
		}
	}
	return messages, nil
}
