// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proxy

import (
	"context"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/msgpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/mq/msgstream"
	"github.com/milvus-io/milvus/pkg/v2/util/commonpbutil"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

// genInsertMsgsByPartition 将属于同一个 (Channel, Partition) 的行数据打包为 InsertMsg。
// 核心逻辑：
//   1. 创建空的 InsertMsg（列式格式）
//   2. 遍历 rowOffsets，逐行追加字段数据到 InsertMsg
//   3. 每追加一行前检查当前消息大小，超过 MaxMessageSize 阈值时拆分为新消息
//   4. 返回一个或多个 InsertMsg（大批量插入可能产生多个消息）
//
// 消息拆分机制保证单条消息不超过消息队列的最大消息限制（默认 5MB）。
// 数据以列式 (ColumnBased) 格式组织，每个字段一列，便于后续存储和查询。
//
// 示例：
//   原始 batch:
//     id    = [101, 102, 103]
//     title = ["red mug", "blue bottle", "green tea"]
//     price = [19.8, 29.9, 9.9]
//
//   如果当前 channel 只拿到 rowOffsets = [0, 2]，那么打包后的 InsertMsg 会变成：
//     NumRows = 2
//     id      = [101, 103]
//     title   = ["red mug", "green tea"]
//     price   = [19.8, 9.9]
//
//   注意：
//   - rowOffsets 决定“挑哪几行”
//   - 但最终消息依旧是列式 FieldData，而不是重新拼成一行一行的 JSON
func genInsertMsgsByPartition(ctx context.Context,
	segmentID UniqueID,
	partitionID UniqueID,
	partitionName string,
	rowOffsets []int,
	channelName string,
	insertMsg *msgstream.InsertMsg,
) ([]msgstream.TsMsg, error) {
	threshold := Params.PulsarCfg.MaxMessageSize.GetAsInt()
	// 关键日志字段说明：
	// - segmentID: 当前消息要写入的 Segment ID；在 streaming 场景下这里可能先是占位值，后续由下游分配
	// - partitionID/partitionName: 本批消息对应的分区 ID 和分区名
	// - channelName: 这批数据即将写入的目标 VChannel
	// - rowOffsetCount: 从原始请求里抽出了多少行放进这批消息
	// - maxMessageSize: 单条消息允许的最大大小，超过它就要继续拆包
	log.Ctx(ctx).Info("[TRACE-INSERT] genInsertMsgsByPartition: 按分区打包消息",
		zap.Int64("segmentID", segmentID),
		zap.Int64("partitionID", partitionID),
		zap.String("partitionName", partitionName),
		zap.String("channelName", channelName),
		zap.Int("rowOffsetCount", len(rowOffsets)),
		zap.Int("maxMessageSize", threshold),
	)

	// 【创建空 InsertMsg 工厂函数】
	// 每个 InsertMsg 对应一个 (Segment, Partition, Channel) 的数据子集
	// FieldsData 预分配与原始消息相同数量的字段槽位（列式存储，每列一个 FieldData）
	createInsertMsg := func(segmentID UniqueID, channelName string) *msgstream.InsertMsg {
		insertReq := &msgpb.InsertRequest{
			Base: commonpbutil.NewMsgBase(
				commonpbutil.WithMsgType(commonpb.MsgType_Insert),
				commonpbutil.WithTimeStamp(insertMsg.BeginTimestamp), // entity's timestamp was set to equal it.BeginTimestamp in preExecute()
				commonpbutil.WithSourceID(insertMsg.Base.SourceID),
			),
			CollectionID:   insertMsg.CollectionID,
			PartitionID:    partitionID,
			DbName:         insertMsg.DbName,
			CollectionName: insertMsg.CollectionName,
			PartitionName:  partitionName,
			SegmentID:      segmentID,
			ShardName:      channelName,
			Version:        msgpb.InsertDataVersion_ColumnBased,
			FieldsData:     make([]*schemapb.FieldData, len(insertMsg.GetFieldsData())),
		}
		msg := &msgstream.InsertMsg{
			BaseMsg: msgstream.BaseMsg{
				Ctx: ctx,
			},
			InsertRequest: insertReq,
		}

		return msg
	}

	fieldsData := insertMsg.GetFieldsData()
	idxComputer := typeutil.NewFieldDataIdxComputer(fieldsData)

	repackedMsgs := make([]msgstream.TsMsg, 0)
	requestSize := 0
	msg := createInsertMsg(segmentID, channelName)
	for _, offset := range rowOffsets {
		fieldIdxs := idxComputer.Compute(int64(offset))
		curRowMessageSize, err := typeutil.EstimateEntitySize(fieldsData, offset, fieldIdxs...)
		if err != nil {
			return nil, err
		}

		// 【消息拆分】当累积消息大小超过阈值时，将当前消息保存并创建新消息
		// 这防止单条消息超过消息队列限制（Pulsar/Kafka 的 MaxMessageSize）
		if requestSize+curRowMessageSize >= threshold && msg.NumRows > 0 {
			repackedMsgs = append(repackedMsgs, msg)
			msg = createInsertMsg(segmentID, channelName)
			requestSize = 0
		}

		typeutil.AppendFieldData(msg.FieldsData, fieldsData, int64(offset), fieldIdxs...)
		msg.HashValues = append(msg.HashValues, insertMsg.HashValues[offset])
		msg.Timestamps = append(msg.Timestamps, insertMsg.Timestamps[offset])
		msg.RowIDs = append(msg.RowIDs, insertMsg.RowIDs[offset])
		msg.NumRows++
		requestSize += curRowMessageSize
	}
	if msg.NumRows > 0 {
		repackedMsgs = append(repackedMsgs, msg)
	}

	log.Ctx(ctx).Info("[TRACE-INSERT] genInsertMsgsByPartition: 打包完成",
		zap.String("channelName", channelName),
		zap.Int64("partitionID", partitionID),
		zap.Int("repackedMsgCount", len(repackedMsgs)),
		zap.Int("totalRows", len(rowOffsets)),
	)

	return repackedMsgs, nil
}
