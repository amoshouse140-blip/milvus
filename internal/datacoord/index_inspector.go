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

package datacoord

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus/internal/datacoord/allocator"
	"github.com/milvus-io/milvus/internal/datacoord/task"
	"github.com/milvus-io/milvus/internal/metastore/model"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/util/vecindexmgr"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
)

type indexInspector struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	notifyIndexChan chan int64

	meta                      *meta
	scheduler                 task.GlobalScheduler
	allocator                 allocator.Allocator
	handler                   Handler
	storageCli                storage.ChunkManager
	indexEngineVersionManager IndexEngineVersionManager
}

func newIndexInspector(
	ctx context.Context,
	notifyIndexChan chan int64,
	meta *meta,
	scheduler task.GlobalScheduler,
	allocator allocator.Allocator,
	handler Handler,
	storageCli storage.ChunkManager,
	indexEngineVersionManager IndexEngineVersionManager,
) *indexInspector {
	ctx, cancel := context.WithCancel(ctx)
	return &indexInspector{
		ctx:                       ctx,
		cancel:                    cancel,
		meta:                      meta,
		notifyIndexChan:           notifyIndexChan,
		scheduler:                 scheduler,
		allocator:                 allocator,
		handler:                   handler,
		storageCli:                storageCli,
		indexEngineVersionManager: indexEngineVersionManager,
	}
}

func (i *indexInspector) Start() {
	i.reloadFromMeta()
	i.wg.Add(1)
	go i.createIndexForSegmentLoop(i.ctx)
}

func (i *indexInspector) Stop() {
	i.cancel()
	i.wg.Wait()
}

func (i *indexInspector) createIndexForSegmentLoop(ctx context.Context) {
	log := log.Ctx(ctx)
	log.Info("start create index for segment loop...",
		zap.Int64("TaskCheckInterval", Params.DataCoordCfg.TaskCheckInterval.GetAsInt64()))
	defer i.wg.Done()

	ticker := time.NewTicker(Params.DataCoordCfg.TaskCheckInterval.GetAsDuration(time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Warn("DataCoord context done, exit...")
			return
		case <-ticker.C:
			segments := i.getUnIndexTaskSegments(ctx)
			for _, segment := range segments {
				if err := i.createIndexesForSegment(ctx, segment); err != nil {
					log.Warn("create index for segment fail, wait for retry", zap.Int64("segmentID", segment.ID))
					continue
				}
			}
		case collectionID := <-i.notifyIndexChan:
			log.Info("[TRACE-INDEX] DataCoord: 收到索引构建通知, 开始检查需要建索引的段",
				zap.Int64("collectionID", collectionID))
			isExternal := i.isExternalCollection(collectionID)
			segments := i.meta.SelectSegments(ctx, WithCollection(collectionID), SegmentFilterFunc(func(info *SegmentInfo) bool {
				return isFlush(info) && (!enableSortCompaction() || info.GetIsSorted() || info.GetIsSortedByNamespace() || isExternal)
			}))
			for _, segment := range segments {
				if err := i.createIndexesForSegment(ctx, segment); err != nil {
					log.Warn("create index for segment fail, wait for retry", zap.Int64("segmentID", segment.ID))
					continue
				}
			}
		case segID := <-getBuildIndexChSingleton():
			log.Info("receive new flushed segment", zap.Int64("segmentID", segID))
			segment := i.meta.GetSegment(ctx, segID)
			if segment == nil {
				log.Warn("segment is not exist, no need to build index", zap.Int64("segmentID", segID))
				continue
			}
			if err := i.createIndexesForSegment(ctx, segment); err != nil {
				log.Warn("create index for segment fail, wait for retry", zap.Int64("segmentID", segment.ID))
				continue
			}
		}
	}
}

func (i *indexInspector) getUnIndexTaskSegments(ctx context.Context) []*SegmentInfo {
	flushedSegments := i.meta.SelectSegments(ctx, SegmentFilterFunc(func(seg *SegmentInfo) bool {
		return isFlush(seg)
	}))

	unindexedSegments := make([]*SegmentInfo, 0)
	for _, segment := range flushedSegments {
		if i.meta.indexMeta.IsUnIndexedSegment(segment.CollectionID, segment.GetID()) {
			unindexedSegments = append(unindexedSegments, segment)
		}
	}
	return unindexedSegments
}

// createIndexesForSegment 为指定段创建所有缺失的索引。
// 流程：
//   1. 检查段是否已排序（未排序则跳过，等待 Sort Compaction 完成后再建索引）
//   2. 跳过 L0 段（L0 仅存删除记录，无需索引）
//   3. 获取 Collection 上定义的所有索引
//   4. 检查段上已存在的索引
//   5. 为缺失的索引调用 createIndexForSegment 创建索引任务
func (i *indexInspector) createIndexesForSegment(ctx context.Context, segment *SegmentInfo) error {
	if enableSortCompaction() && !segment.GetIsSorted() && !segment.GetIsSortedByNamespace() && !i.isExternalCollection(segment.CollectionID) {
		log.Ctx(ctx).Debug("segment is not sorted by pk, skip create indexes", zap.Int64("segmentID", segment.GetID()))
		return nil
	}
	if segment.GetLevel() == datapb.SegmentLevel_L0 {
		log.Ctx(ctx).Debug("segment is level zero, skip create indexes", zap.Int64("segmentID", segment.GetID()))
		return nil
	}

	indexes := i.meta.indexMeta.GetIndexesForCollection(segment.CollectionID, "")
	indexIDToSegIndexes := i.meta.indexMeta.GetSegmentIndexes(segment.CollectionID, segment.ID)
	for _, index := range indexes {
		if _, ok := indexIDToSegIndexes[index.IndexID]; !ok {
			if err := i.createIndexForSegment(ctx, segment, index.IndexID); err != nil {
				log.Ctx(ctx).Warn("create index for segment fail", zap.Int64("segmentID", segment.ID),
					zap.Int64("indexID", index.IndexID))
				return err
			}
		}
	}
	return nil
}

// createIndexForSegment 为指定段和索引创建一个索引构建任务。
// 流程：
//   1. 分配全局唯一 BuildID
//   2. 获取索引参数和索引类型（HNSW/IVF/DiskANN 等）
//   3. 评估任务需要的资源槽位（根据字段数据大小和索引类型）
//   4. 如果 Knowhere 配置启用，可能覆盖索引类型参数
//   5. 创建 SegmentIndex 元数据并持久化
//   6. 提交索引构建任务到全局调度器，由 DataNode 执行实际构建
func (i *indexInspector) createIndexForSegment(ctx context.Context, segment *SegmentInfo, indexID UniqueID) error {
	log.Info("[TRACE-INDEX] DataCoord: 为段创建索引任务",
		zap.Int64("segmentID", segment.ID),
		zap.Int64("collectionID", segment.CollectionID),
		zap.Int64("partitionID", segment.PartitionID),
		zap.Int64("numOfRows", segment.NumOfRows),
		zap.Int64("indexID", indexID),
	)
	buildID, err := i.allocator.AllocID(context.Background())
	if err != nil {
		return err
	}

	indexParams := i.meta.indexMeta.GetIndexParams(segment.CollectionID, indexID)
	indexType := GetIndexType(indexParams)
	isVectorIndex := vecindexmgr.GetVecIndexMgrInstance().IsVecIndex(indexType)
	fieldID := i.meta.indexMeta.GetFieldIDByIndexID(segment.CollectionID, indexID)
	fieldSize := segment.getFieldBinlogSize(fieldID)
	taskSlot := calculateIndexTaskSlot(fieldSize, isVectorIndex)

	// rewrite the index type if needed, and this final index type will be persisted in the meta
	if isVectorIndex && Params.KnowhereConfig.Enable.GetAsBool() {
		var err error
		indexParams, err = Params.KnowhereConfig.UpdateIndexParams(indexType, paramtable.BuildStage, indexParams)
		if err != nil {
			return err
		}
	}
	newIndexType := GetIndexType(indexParams)
	if newIndexType != "" && newIndexType != indexType {
		log.Info("override index type", zap.String("indexType", indexType), zap.String("newIndexType", newIndexType))
		indexType = newIndexType
	}

	segIndex := &model.SegmentIndex{
		SegmentID:      segment.ID,
		CollectionID:   segment.CollectionID,
		PartitionID:    segment.PartitionID,
		NumRows:        segment.NumOfRows,
		IndexID:        indexID,
		BuildID:        buildID,
		CreatedUTCTime: uint64(time.Now().Unix()),
		WriteHandoff:   false,
		IndexType:      indexType,
	}
	if err = i.meta.indexMeta.AddSegmentIndex(ctx, segIndex); err != nil {
		return err
	}
	i.scheduler.Enqueue(newIndexBuildTask(model.CloneSegmentIndex(segIndex),
		taskSlot,
		i.meta,
		i.handler,
		i.storageCli,
		i.indexEngineVersionManager))
	log.Info("indexInspector create index for segment success",
		zap.Int64("segmentID", segment.ID),
		zap.Int64("indexID", indexID),
		zap.Int64("fieldID", fieldID),
		zap.Int64("segment size", segment.getSegmentSize()),
		zap.Int64("field size", fieldSize),
		zap.Int64("task slot", taskSlot))
	return nil
}

func (i *indexInspector) isExternalCollection(collectionID int64) bool {
	coll := i.meta.GetCollection(collectionID)
	return coll != nil && coll.IsExternal()
}

func (i *indexInspector) reloadFromMeta() {
	segments := i.meta.GetAllSegmentsUnsafe()
	for _, segment := range segments {
		for _, segIndex := range i.meta.indexMeta.GetSegmentIndexes(segment.GetCollectionID(), segment.ID) {
			if segIndex.IsDeleted || (segIndex.IndexState != commonpb.IndexState_Unissued &&
				segIndex.IndexState != commonpb.IndexState_Retry &&
				segIndex.IndexState != commonpb.IndexState_InProgress) {
				continue
			}

			indexParams := i.meta.indexMeta.GetIndexParams(segment.CollectionID, segIndex.IndexID)
			indexType := GetIndexType(indexParams)
			isVectorIndex := vecindexmgr.GetVecIndexMgrInstance().IsVecIndex(indexType)
			fieldID := i.meta.indexMeta.GetFieldIDByIndexID(segment.CollectionID, segIndex.IndexID)
			fieldSize := segment.getFieldBinlogSize(fieldID)
			taskSlot := calculateIndexTaskSlot(fieldSize, isVectorIndex)

			i.scheduler.Enqueue(newIndexBuildTask(
				model.CloneSegmentIndex(segIndex),
				taskSlot,
				i.meta,
				i.handler,
				i.storageCli,
				i.indexEngineVersionManager,
			))
		}
	}
}
