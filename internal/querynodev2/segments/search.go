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

package segments

import (
	"context"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus/internal/querynodev2/segments/metricsutil"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/metrics"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/timerecord"
)

// searchSegments 在给定的段列表上执行并行搜索。
// 核心流程：
//   1. 为每个段创建一个搜索 goroutine（单段时走快速路径，跳过 errgroup 开销）
//   2. 每个 goroutine 调用 Segment.Search()，通过 CGO 调用 C++ Knowhere 引擎
//   3. 有索引的段使用 ANN 近似搜索（HNSW/IVF/DiskANN 等）
//   4. 无索引的段使用暴力线性扫描（brute force）
//   5. 搜索前通过 ptrLock.PinIf() 锁定段，防止搜索期间段被释放
//   6. 返回各段的 SearchResult 列表（未归并）
func searchSegments(ctx context.Context, mgr *Manager, segments []Segment, segType SegmentType, searchReq *SearchRequest) ([]*SearchResult, error) {
	searchLabel := metrics.SealedSegmentLabel
	if segType == commonpb.SegmentState_Growing {
		searchLabel = metrics.GrowingSegmentLabel
	}

	nodeIDStr := paramtable.GetStringNodeID()
	searchResults := make([]*SearchResult, len(segments))
	searcher := func(ctx context.Context, s Segment, idx int) error {
		tr := timerecord.NewTimeRecorder("searchOnSegments")
		log.Ctx(ctx).Info("[TRACE-SEARCH] searchSegments: 开始搜索单个段 (CGO调用C++)",
			zap.Int64("segmentID", s.ID()),
			zap.String("segmentType", searchLabel),
			zap.Int64("rowNum", s.RowNum()),
			zap.Bool("hasIndex", s.ExistIndex(searchReq.SearchFieldID())),
		)
		searchResult, err := s.Search(ctx, searchReq)
		if err != nil {
			return err
		}
		searchResults[idx] = searchResult
		elapsed := float64(tr.ElapseSpan().Microseconds()) / 1000.0
		log.Ctx(ctx).Info("[TRACE-SEARCH] searchSegments: 单段搜索完成",
			zap.Int64("segmentID", s.ID()),
			zap.Float64("elapsedMs", elapsed),
		)
		metrics.QueryNodeSQSegmentLatency.WithLabelValues(nodeIDStr,
			metrics.SearchLabel, searchLabel).Observe(elapsed)
		metrics.QueryNodeSegmentSearchLatencyPerVector.WithLabelValues(nodeIDStr,
			metrics.SearchLabel, searchLabel).Observe(elapsed / float64(searchReq.GetNumOfQuery()))
		return nil
	}

	executeSegment := func(ctx context.Context, seg Segment, idx int) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var err error
		accessRecord := metricsutil.NewSearchSegmentAccessRecord(getSegmentMetricLabel(seg))
		defer func() {
			accessRecord.Finish(err)
		}()

		return searcher(ctx, seg, idx)
	}

	segmentsWithoutIndex := make([]int64, 0, len(segments))
	for _, seg := range segments {
		if !seg.ExistIndex(searchReq.SearchFieldID()) {
			segmentsWithoutIndex = append(segmentsWithoutIndex, seg.ID())
		}
	}

	var err error
	if len(segments) == 1 {
		// Single segment fast path: skip errgroup/goroutine overhead
		err = executeSegment(ctx, segments[0], 0)
	} else {
		errGroup, groupCtx := errgroup.WithContext(ctx)
		for i, segment := range segments {
			segIdx := i
			seg := segment
			errGroup.Go(func() error {
				return executeSegment(groupCtx, seg, segIdx)
			})
		}
		err = errGroup.Wait()
	}

	if err != nil {
		// Collect non-nil results for cleanup
		validResults := make([]*SearchResult, 0, len(segments))
		for _, r := range searchResults {
			if r != nil {
				validResults = append(validResults, r)
			}
		}
		DeleteSearchResults(validResults)
		return nil, err
	}

	if len(segmentsWithoutIndex) > 0 {
		log.Ctx(ctx).Debug("search growing/sealed segments without indexes", zap.Int64s("segmentIDs", segmentsWithoutIndex))
	}

	return searchResults, nil
}

// SearchHistorical 在 Sealed 段（历史数据）上执行搜索。
// Sealed 段的数据来自对象存储 (MinIO/S3)，已完成 Flush 和索引构建。
// 数据加载路径：对象存储 → DiskCache（本地磁盘缓存） → 内存/Mmap
// 如果数据已在缓存中则直接使用，否则先从对象存储下载。
//
// 示例：
//   之前 flush 后对象存储里已有：
//     segment 7001 -> id [101, 103]
//   那么 SearchHistorical 负责把这类“已经持久化的历史段”拿出来搜索。
func SearchHistorical(ctx context.Context, manager *Manager, searchReq *SearchRequest, collID int64, partIDs []int64, segIDs []int64) ([]*SearchResult, []Segment, error) {
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}

	segments, err := validateOnHistorical(ctx, manager, collID, partIDs, segIDs)
	if err != nil {
		return nil, nil, err
	}
	segmentIDs := make([]int64, 0, len(segments))
	for _, seg := range segments {
		segmentIDs = append(segmentIDs, seg.ID())
	}
	log.Ctx(ctx).Info("[TRACE-SEARCH] SearchHistorical: 搜索 Sealed 段 (历史数据)",
		zap.Int64("collectionID", collID),
		zap.Int("segmentCount", len(segments)),
		zap.Int64s("segmentIDs", segmentIDs),
	)
	searchResults, err := searchSegments(ctx, manager, segments, SegmentTypeSealed, searchReq)
	if err == nil {
		log.Ctx(ctx).Info("[TRACE-SEARCH] SearchHistorical: Sealed 段搜索完成",
			zap.Int("resultCount", len(searchResults)),
		)
	}
	return searchResults, segments, err
}

// SearchStreaming 在 Growing 段（实时流式数据）上执行搜索。
// Growing 段的数据来自 WAL 的实时消费，驻留在内存中，尚未 Flush 到对象存储。
// Growing 段通常没有向量索引，搜索使用暴力扫描 (brute force)。
// Growing 段保证了插入后的数据能被立即搜索到（实时性保证）。
//
// 示例：
//   用户刚插入：
//     [101, 102, 103]
//   但还没 flush，此时它们可能只存在于内存里的 Growing Segments：
//     growing seg 8001 -> [101, 103]
//     growing seg 8002 -> [102]
//
//   SearchStreaming 就是负责搜这部分“刚写入、还没落对象存储”的实时数据。
func SearchStreaming(ctx context.Context, manager *Manager, searchReq *SearchRequest, collID int64, partIDs []int64, segIDs []int64) ([]*SearchResult, []Segment, error) {
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}

	segments, err := validateOnStream(ctx, manager, collID, partIDs, segIDs)
	if err != nil {
		return nil, nil, err
	}
	segmentIDs := make([]int64, 0, len(segments))
	for _, seg := range segments {
		segmentIDs = append(segmentIDs, seg.ID())
	}
	log.Ctx(ctx).Info("[TRACE-SEARCH] SearchStreaming: 搜索 Growing 段 (实时数据)",
		zap.Int64("collectionID", collID),
		zap.Int("segmentCount", len(segments)),
		zap.Int64s("segmentIDs", segmentIDs),
	)
	searchResults, err := searchSegments(ctx, manager, segments, SegmentTypeGrowing, searchReq)
	if err == nil {
		log.Ctx(ctx).Info("[TRACE-SEARCH] SearchStreaming: Growing 段搜索完成",
			zap.Int("resultCount", len(searchResults)),
		)
	}
	return searchResults, segments, err
}
