package proxy

import (
	"context"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/allocator"
	"github.com/milvus-io/milvus/pkg/v2/common"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/metrics"
	"github.com/milvus-io/milvus/pkg/v2/util/commonpbutil"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/timerecord"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

// insertTask 封装一次 Insert 请求的完整生命周期。
// 核心流程：PreExecute（验证+主键分配） → Execute（分片+写WAL） → PostExecute（无操作）
//
// 关键字段说明：
//   - insertMsg: 封装后的插入消息，包含列式数据、RowID、时间戳
//   - idAllocator: 全局 ID 分配器，用于 Auto-ID 模式下分配主键
//   - chMgr: Channel 管理器，维护 Collection → VChannel/PChannel 的映射
//   - partitionKeys: 当 Collection 使用 Partition Key 时，存储分区键字段数据
//   - collectionID: 从 MetaCache 获取的 Collection 内部 ID
type insertTask struct {
	baseTask
	// req *milvuspb.InsertRequest
	Condition
	insertMsg *BaseInsertTask
	ctx       context.Context

	result          *milvuspb.MutationResult
	idAllocator     *allocator.IDAllocator
	chMgr           channelsMgr
	vChannels       []vChan
	pChannels       []pChan
	schema          *schemapb.CollectionSchema
	partitionKeys   *schemapb.FieldData
	schemaTimestamp uint64
	collectionID    int64
}

// TraceCtx returns insertTask context
func (it *insertTask) TraceCtx() context.Context {
	return it.ctx
}

func (it *insertTask) ID() UniqueID {
	return it.insertMsg.Base.MsgID
}

func (it *insertTask) SetID(uid UniqueID) {
	it.insertMsg.Base.MsgID = uid
}

func (it *insertTask) Name() string {
	return InsertTaskName
}

func (it *insertTask) Type() commonpb.MsgType {
	return it.insertMsg.Base.MsgType
}

func (it *insertTask) BeginTs() Timestamp {
	return it.insertMsg.BeginTimestamp
}

func (it *insertTask) SetTs(ts Timestamp) {
	it.insertMsg.BeginTimestamp = ts
	it.insertMsg.EndTimestamp = ts
}

func (it *insertTask) EndTs() Timestamp {
	return it.insertMsg.EndTimestamp
}

func (it *insertTask) setChannels() error {
	collID, err := globalMetaCache.GetCollectionID(it.ctx, it.insertMsg.GetDbName(), it.insertMsg.CollectionName)
	if err != nil {
		return err
	}
	channels, err := it.chMgr.getChannels(collID)
	if err != nil {
		return err
	}
	it.pChannels = channels
	return nil
}

func (it *insertTask) getChannels() []pChan {
	return it.pChannels
}

func (it *insertTask) OnEnqueue() error {
	if it.insertMsg.Base == nil {
		it.insertMsg.Base = commonpbutil.NewMsgBase()
	}
	it.insertMsg.Base.MsgType = commonpb.MsgType_Insert
	it.insertMsg.Base.SourceID = paramtable.GetNodeID()
	return nil
}

// PreExecute 在任务实际执行前进行参数校验和数据准备。
// 主要步骤：
//   1. 验证 Collection 名称合法性
//   2. 检查请求大小不超过 maxInsertSize 限制
//   3. 从 MetaCache 获取 CollectionID 和 Schema
//   4. 检查 Schema 版本一致性（防止 Schema 变更导致数据不一致）
//   5. 为所有行分配全局唯一 RowID（通过 idAllocator 批量分配）
//   6. 为所有行设置统一时间戳（BeginTimestamp）
//   7. 处理动态字段、结构体字段展平
//   8. 检查主键字段数据（Auto-ID 时自动填充，非 Auto-ID 时验证用户提供的主键）
//   9. 判断 Partition Key 模式，决定分区路由策略
//  10. 数据合法性校验（NaN检查、溢出检查、长度检查）
func (it *insertTask) PreExecute(ctx context.Context) error {
	ctx, sp := otel.Tracer(typeutil.ProxyRole).Start(ctx, "Proxy-Insert-PreExecute")
	defer sp.End()

	it.result = &milvuspb.MutationResult{
		Status: merr.Success(),
		IDs: &schemapb.IDs{
			IdField: nil,
		},
		Timestamp: it.EndTs(),
	}

	collectionName := it.insertMsg.CollectionName
	// 关键日志字段说明：
	// - collectionName: 当前要校验的集合名
	// - numRows: insertMsg 中的行数，会贯穿后续主键分配、校验和消息打包流程
	log.Ctx(ctx).Info("[TRACE-INSERT] Step1: PreExecute 开始验证",
		zap.String("collectionName", collectionName),
		zap.Uint64("numRows", it.insertMsg.NumRows),
	)
	if err := validateCollectionName(collectionName); err != nil {
		log.Ctx(ctx).Warn("valid collection name failed", zap.String("collectionName", collectionName), zap.Error(err))
		return err
	}

	maxInsertSize := Params.QuotaConfig.MaxInsertSize.GetAsInt()
	if maxInsertSize != -1 && it.insertMsg.Size() > maxInsertSize {
		log.Ctx(ctx).Warn("insert request size exceeds maxInsertSize",
			zap.Int("request size", it.insertMsg.Size()), zap.Int("maxInsertSize", maxInsertSize))
		return merr.WrapErrAsInputError(merr.WrapErrParameterTooLarge("insert request size exceeds maxInsertSize"))
	}

	// 【获取 Collection 元数据】从全局 MetaCache 获取 CollectionID
	// MetaCache 缓存了 RootCoord 的元数据，避免每次请求都访问 etcd
	collID, err := globalMetaCache.GetCollectionID(context.Background(), it.insertMsg.GetDbName(), collectionName)
	if err != nil {
		log.Ctx(ctx).Warn("fail to get collection id", zap.Error(err))
		return err
	}
	it.collectionID = collID

	// 获取 Collection 详细信息（包含 Schema 版本等）
	colInfo, err := globalMetaCache.GetCollectionInfo(ctx, it.insertMsg.GetDbName(), collectionName, collID)
	if err != nil {
		log.Ctx(ctx).Warn("fail to get collection info", zap.Error(err))
		return err
	}

	if it.schemaTimestamp != 0 {
		if it.schemaTimestamp != colInfo.updateTimestamp {
			err := merr.WrapErrCollectionSchemaMisMatch(collectionName)
			log.Ctx(ctx).Info("collection schema mismatch",
				zap.String("collectionName", collectionName),
				zap.Uint64("requestSchemaTs", it.schemaTimestamp),
				zap.Uint64("collectionSchemaTs", colInfo.updateTimestamp),
				zap.Error(err))
			return err
		}
	}

	schema, err := globalMetaCache.GetCollectionSchema(ctx, it.insertMsg.GetDbName(), collectionName)
	if err != nil {
		log.Ctx(ctx).Warn("get collection schema from global meta cache failed", zap.String("collectionName", collectionName), zap.Error(err))
		return merr.WrapErrAsInputErrorWhen(err, merr.ErrCollectionNotFound, merr.ErrDatabaseNotFound)
	}
	it.schema = schema.CollectionSchema

	if err := genFunctionFields(ctx, it.insertMsg, schema, false); err != nil {
		return err
	}

	rowNums := uint32(it.insertMsg.NRows())
	// 【主键 ID 批量分配】通过全局 IDAllocator 为每一行分配唯一 RowID
	// RowID 由 ClusterID + 自增序列号组成，确保集群内全局唯一
	// 分配范围为 [rowIDBegin, rowIDEnd)，每行一个
	var rowIDBegin UniqueID
	var rowIDEnd UniqueID
	tr := timerecord.NewTimeRecorder("applyPK")
	clusterID := Params.CommonCfg.ClusterID.GetAsUint64()
	rowIDBegin, rowIDEnd, AllocErr := common.AllocAutoID(it.idAllocator.Alloc, rowNums, clusterID)
	metrics.ProxyApplyPrimaryKeyLatency.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10)).Observe(float64(tr.ElapseSpan().Microseconds()) / 1000.0)
	if AllocErr != nil {
		log.Ctx(ctx).Warn("failed to allocate auto id",
			zap.String("collectionName", collectionName),
			zap.Int64("collectionID", it.collectionID),
			zap.Uint32("rowNums", rowNums),
			zap.Error(AllocErr))
		return AllocErr
	}

	// 关键日志字段说明：
	// - collectionID: 集合在集群内部的唯一 ID，后续和 RootCoord/DataCoord 交互都用这个值
	// - rowIDBegin/rowIDEnd: 这批行分配到的 RowID 区间，实际范围是 [rowIDBegin, rowIDEnd)
	// - rowNums: 这次批量分配覆盖了多少行，正常应与请求的 numRows 对齐
	log.Ctx(ctx).Info("[TRACE-INSERT] Step2: 主键 ID 分配完成",
		zap.Int64("collectionID", it.collectionID),
		zap.Int64("rowIDBegin", rowIDBegin),
		zap.Int64("rowIDEnd", rowIDEnd),
		zap.Uint32("rowNums", rowNums),
	)
	it.insertMsg.RowIDs = make([]UniqueID, rowNums)
	for i := rowIDBegin; i < rowIDEnd; i++ {
		offset := i - rowIDBegin
		it.insertMsg.RowIDs[offset] = i
	}
	// set insertTask.timeStamps
	rowNum := it.insertMsg.NRows()
	it.insertMsg.Timestamps = make([]uint64, rowNum)
	for index := range it.insertMsg.Timestamps {
		it.insertMsg.Timestamps[index] = it.insertMsg.BeginTimestamp
	}

	// set result.SuccIndex
	sliceIndex := make([]uint32, rowNums)
	for i := uint32(0); i < rowNums; i++ {
		sliceIndex[i] = i
	}
	it.result.SuccIndex = sliceIndex

	// 【动态字段处理】如果 Schema 开启了动态字段(EnableDynamicField)，
	// 检查并处理 JSON 格式的动态字段数据
	if it.schema.EnableDynamicField {
		err = checkDynamicFieldData(it.schema, it.insertMsg)
		if err != nil {
			return err
		}
	}

	// 【命名空间数据添加】为支持多租户隔离的 namespace 字段注入数据
	err = addNamespaceData(it.schema, it.insertMsg)
	if err != nil {
		return err
	}

	// 【结构体字段展平】将嵌套的 Struct 类型字段展平为多个独立字段
	err = checkAndFlattenStructFieldData(it.schema, it.insertMsg)
	if err != nil {
		return err
	}

	allFields := typeutil.GetAllFieldSchemas(it.schema)

	// 【主键字段处理】
	// Auto-ID 模式：将之前分配的 RowID 作为主键填入数据
	// 非 Auto-ID 模式：验证用户提供的主键数据合法性
	// 同时对主键进行 Hash，为后续 Channel 路由做准备
	it.result.IDs, err = checkPrimaryFieldData(allFields, it.schema, it.insertMsg)
	log := log.Ctx(ctx).With(zap.String("collectionName", collectionName))
	if err != nil {
		log.Warn("check primary field data and hash primary key failed",
			zap.Error(err))
		return err
	}

	// check varchar/text with analyzer was utf-8 format
	err = checkInputUtf8Compatiable(allFields, it.insertMsg)
	if err != nil {
		log.Warn("check varchar/text format failed", zap.Error(err))
		return err
	}

	// Validate and set field ID to insert field data
	err = validateFieldDataColumns(it.insertMsg.GetFieldsData(), schema)
	if err != nil {
		log.Info("validate field data columns failed", zap.Error(err))
		return err
	}
	err = fillFieldPropertiesOnly(it.insertMsg.GetFieldsData(), schema)
	if err != nil {
		log.Info("fill field properties failed", zap.Error(err))
		return err
	}

	partitionKeyMode, err := isPartitionKeyMode(ctx, it.insertMsg.GetDbName(), collectionName)
	if err != nil {
		log.Warn("check partition key mode failed", zap.String("collectionName", collectionName), zap.Error(err))
		return err
	}
	log.Ctx(ctx).Info("[TRACE-INSERT] Step3: Partition Key 模式检查",
		zap.Bool("partitionKeyMode", partitionKeyMode),
		zap.String("collectionName", collectionName),
	)
	if partitionKeyMode {
		fieldSchema, _ := typeutil.GetPartitionKeyFieldSchema(it.schema)
		it.partitionKeys, err = getPartitionKeyFieldData(fieldSchema, it.insertMsg)
		if err != nil {
			log.Warn("get partition keys from insert request failed", zap.String("collectionName", collectionName), zap.Error(err))
			return err
		}
	} else {
		// set default partition name if not use partition key
		// insert to _default partition
		partitionTag := it.insertMsg.GetPartitionName()
		if len(partitionTag) <= 0 {
			pinfo, err := globalMetaCache.GetPartitionInfo(ctx, it.insertMsg.GetDbName(), collectionName, "")
			if err != nil {
				log.Warn("get partition info failed", zap.String("collectionName", collectionName), zap.Error(err))
				return err
			}
			partitionTag = pinfo.name
			it.insertMsg.PartitionName = partitionTag
		}

		if err := validatePartitionTag(partitionTag, true); err != nil {
			log.Warn("valid partition name failed", zap.String("partition name", partitionTag), zap.Error(err))
			return err
		}
	}

	// 【数据合法性校验】对所有字段数据执行以下检查：
	// - NaN 检查: 浮点数不允许 NaN 值
	// - 溢出检查: 数值不超出字段类型范围
	// - 长度检查: varchar 不超过 max_length
	// - 容量检查: array 类型不超过 max_capacity
	if err := newValidateUtil(withNANCheck(), withOverflowCheck(), withMaxLenCheck(), withMaxCapCheck()).
		Validate(it.insertMsg.GetFieldsData(), schema.schemaHelper, it.insertMsg.NRows()); err != nil {
		return merr.WrapErrAsInputError(err)
	}

	log.Debug("Proxy Insert PreExecute done")

	return nil
}

func (it *insertTask) PostExecute(ctx context.Context) error {
	return nil
}
