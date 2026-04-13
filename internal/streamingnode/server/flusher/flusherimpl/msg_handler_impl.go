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

package flusherimpl

import (
	"context"

	"github.com/cockroachdb/errors"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/flushcommon/writebuffer"
	"github.com/milvus-io/milvus/internal/streamingnode/server/resource"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v2/streaming/util/message"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/retry"
)

// newMsgHandler 创建消息处理器，负责处理 StreamingNode 从 WAL 消费到的各种消息。
// 这是插入数据从 WAL 到 WriteBuffer 的桥梁：
//   - HandleCreateSegment: WAL 中的建段消息 → 在 DataCoord 注册段 + 在 WriteBuffer 中创建缓冲区
//   - HandleFlush: 封存指定段 → 触发 SyncTask 将内存数据刷写到对象存储
//   - HandleManualFlush: 用户手动 Flush → 封存段 + 设置 FlushTimestamp
//   - HandleFlushAll: 封存所有段 + Flush 整个 Channel
//   - HandleSchemaChange: Schema 变更时封存旧段
//
// 插入链路在这里的直观理解可以是：
//   Proxy 写 WAL 前:
//     ch0 的 InsertMsg -> id = [101, 103]
//     ch1 的 InsertMsg -> id = [102]
//
//   StreamingNode 消费 WAL 后:
//     GrowingSegment(7001, ch0) <- [101, 103]
//     GrowingSegment(7002, ch1) <- [102]
//
//   后续再由 Flush / Seal 把这些内存里的列式数据刷到对象存储。
func newMsgHandler(wbMgr writebuffer.BufferManager) *msgHandlerImpl {
	return &msgHandlerImpl{
		wbMgr: wbMgr,
	}
}

// msgHandlerImpl 是消息处理器的具体实现。
// wbMgr (BufferManager) 管理每个 VChannel 的 WriteBuffer，
// 负责在内存中缓冲 Insert 数据并在适当时机触发 Flush。
type msgHandlerImpl struct {
	wbMgr writebuffer.BufferManager
}

func (impl *msgHandlerImpl) HandleCreateSegment(ctx context.Context, createSegmentMsg message.ImmutableCreateSegmentMessageV2) error {
	vchannel := createSegmentMsg.VChannel()
	h := createSegmentMsg.Header()
	log.Info("[TRACE-INSERT] StreamingNode: 收到 CreateSegment 消息, 创建 Growing Segment",
		zap.String("vchannel", vchannel),
		zap.Int64("collectionID", h.CollectionId),
		zap.Int64("partitionID", h.PartitionId),
		zap.Int64("segmentID", h.SegmentId),
		zap.String("level", h.Level.String()),
	)
	if err := impl.createNewGrowingSegment(ctx, vchannel, h); err != nil {
		return err
	}
	logger := log.With(log.FieldMessage(createSegmentMsg))
	if err := impl.wbMgr.CreateNewGrowingSegment(ctx, vchannel, h.PartitionId, h.SegmentId); err != nil {
		logger.Warn("fail to create new growing segment")
		return err
	}
	log.Info("[TRACE-INSERT] StreamingNode: Growing Segment 创建成功",
		zap.Int64("segmentID", h.SegmentId),
		zap.Int64("partitionID", h.PartitionId),
	)
	return nil
}

// createNewGrowingSegment 在 DataCoord 注册一个新的 Growing 段。
// L0 段跳过此步骤（L0 仅用于存储删除记录，其 binlog 上传和 flush 是一次性操作）。
// 对于 L1 段：向 DataCoord 发送 AllocSegment RPC，使 DataCoord 跟踪该段的生命周期。
func (impl *msgHandlerImpl) createNewGrowingSegment(ctx context.Context, vchannel string, h *message.CreateSegmentMessageHeader) error {
	if h.Level == datapb.SegmentLevel_L0 {
		// L0 段跳过：L0 仅存删除记录，不走 Growing→Sealed→Flushed 的标准生命周期
		return nil
	}
	// 将待定段转为 Growing 状态，先在 DataCoord 分配段元数据
	mix, err := resource.Resource().MixCoordClient().GetWithContext(ctx)
	if err != nil {
		return err
	}
	logger := log.With(zap.Int64("collectionID", h.CollectionId), zap.Int64("partitionID", h.PartitionId), zap.Int64("segmentID", h.SegmentId))
	return retry.Do(ctx, func() (err error) {
		resp, err := mix.AllocSegment(ctx, &datapb.AllocSegmentRequest{
			CollectionId:         h.CollectionId,
			PartitionId:          h.PartitionId,
			SegmentId:            h.SegmentId,
			Vchannel:             vchannel,
			StorageVersion:       h.StorageVersion,
			IsCreatedByStreaming: true,
		})
		if err := merr.CheckRPCCall(resp, err); err != nil {
			logger.Warn("failed to alloc growing segment at datacoord")
			return errors.Wrap(err, "failed to alloc growing segment at datacoord")
		}
		logger.Info("alloc growing segment at datacoord")
		return nil
	}, retry.AttemptAlways())
}

func (impl *msgHandlerImpl) HandleFlush(flushMsg message.ImmutableFlushMessageV2) error {
	vchannel := flushMsg.VChannel()
	log.Info("[TRACE-INSERT] StreamingNode: 收到 Flush 消息, 开始封存 Segment",
		zap.String("vchannel", vchannel),
		zap.Int64("segmentID", flushMsg.Header().SegmentId),
	)
	if err := impl.wbMgr.SealSegments(context.Background(), vchannel, []int64{flushMsg.Header().SegmentId}); err != nil {
		return errors.Wrap(err, "failed to seal segments")
	}
	log.Info("[TRACE-INSERT] StreamingNode: Segment 封存完成",
		zap.Int64("segmentID", flushMsg.Header().SegmentId),
	)
	return nil
}

func (impl *msgHandlerImpl) HandleManualFlush(flushMsg message.ImmutableManualFlushMessageV2) error {
	vchannel := flushMsg.VChannel()
	if err := impl.wbMgr.SealSegments(context.Background(), vchannel, flushMsg.Header().SegmentIds); err != nil {
		return errors.Wrap(err, "failed to seal segments")
	}
	if err := impl.wbMgr.FlushChannel(context.Background(), vchannel, flushMsg.TimeTick()); err != nil {
		return errors.Wrap(err, "failed to flush channel")
	} // may be redundant.
	return nil
}

func (impl *msgHandlerImpl) HandleFlushAll(vchannel string, flushAllMsg message.ImmutableFlushAllMessageV2) error {
	if err := impl.wbMgr.SealAllSegments(context.Background(), vchannel); err != nil {
		return errors.Wrap(err, "failed to seal all segments")
	}
	// Use FlushAllMsg's ts as flush ts.
	if err := impl.wbMgr.FlushChannel(context.Background(), vchannel, flushAllMsg.TimeTick()); err != nil {
		return errors.Wrap(err, "failed to flush channel")
	} // may be redundant.
	return nil
}

func (impl *msgHandlerImpl) HandleSchemaChange(ctx context.Context, msg message.ImmutableSchemaChangeMessageV2) error {
	return impl.wbMgr.SealSegments(context.Background(), msg.VChannel(), msg.Header().FlushedSegmentIds)
}

func (impl *msgHandlerImpl) HandleAlterCollection(ctx context.Context, putCollectionMsg message.ImmutableAlterCollectionMessageV2) error {
	return impl.wbMgr.SealSegments(context.Background(), putCollectionMsg.VChannel(), putCollectionMsg.Header().FlushedSegmentIds)
}

func (impl *msgHandlerImpl) HandleTruncateCollection(flushMsg message.ImmutableTruncateCollectionMessageV2) error {
	vchannel := flushMsg.VChannel()
	if err := impl.wbMgr.SealSegments(context.Background(), vchannel, flushMsg.Header().SegmentIds); err != nil {
		return errors.Wrap(err, "failed to seal segments")
	}
	if err := impl.wbMgr.FlushChannel(context.Background(), vchannel, flushMsg.TimeTick()); err != nil {
		return errors.Wrap(err, "failed to flush channel")
	}
	return nil
}

// HandleAlterWAL handles Alter WAL message by sealing all segments and flushing the channel.
// This ensures all buffered data is persisted before switching to the new WAL implementation.
func (impl *msgHandlerImpl) HandleAlterWAL(ctx context.Context, alterWALMsg message.ImmutableAlterWALMessageV2, currentVChannel string) error {
	vchannel := currentVChannel
	logger := log.With(
		zap.String("vchannel", vchannel),
		zap.Uint64("alterWALTimeTick", alterWALMsg.TimeTick()),
		zap.String("currentWALName", alterWALMsg.WALName().String()),
		zap.Stringer("targetWALName", alterWALMsg.Header().TargetWalName))

	// Seal all segments in the current vchannel before WAL switch
	if err := impl.wbMgr.SealAllSegments(ctx, vchannel); err != nil {
		logger.Warn("failed to seal all segments for WAL switch", zap.Error(err))
		return errors.Wrap(err, "failed to seal all segments")
	}
	logger.Info("sealed all segments for WAL switch")

	// Flush channel to persist buffered data before switching WAL
	if err := impl.wbMgr.FlushChannel(ctx, vchannel, alterWALMsg.TimeTick()); err != nil {
		logger.Warn("failed to flush channel for WAL switch", zap.Error(err))
		return errors.Wrap(err, "failed to flush channel")
	}
	logger.Info("flushed channel for WAL switch")
	return nil
}
