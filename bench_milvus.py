"""
Milvus 瓶颈定位压测脚本
分离测量：embedding生成、数据插入、索引构建、向量搜索、标量过滤
"""

import time
import random
import string
import statistics
import numpy as np
from pymilvus import MilvusClient
from pymilvus import model

# ============ 配置 ============
MILVUS_URI = "milvus_demo.db"  # lite 模式，换成 "http://host:19530" 测真实部署
COLLECTION = "bench_collection"
DIM = 768
NUM_DOCS = 1000          # 插入文档数
SEARCH_ROUNDS = 100      # 搜索轮数
SEARCH_TOP_K = 10
SEARCH_CONCURRENCY = [1, 4, 8, 16]  # 并发度（逐步加压）


def timed(name):
    """计时装饰器"""
    class Timer:
        def __enter__(self):
            self.start = time.perf_counter()
            return self
        def __exit__(self, *args):
            self.elapsed = time.perf_counter() - self.start
            print(f"  [{name}] {self.elapsed:.3f}s")
    return Timer()


def random_text(length=50):
    return ''.join(random.choices(string.ascii_lowercase + ' ', k=length))


def main():
    client = MilvusClient(MILVUS_URI)

    # ---- 清理 & 建表 ----
    if client.has_collection(collection_name=COLLECTION):
        client.drop_collection(collection_name=COLLECTION)
    client.create_collection(
        collection_name=COLLECTION,
        dimension=DIM,
    )
    print(f"Collection created: dim={DIM}")

    # ============ 阶段1: Embedding 生成 ============
    print(f"\n=== 阶段1: Embedding 生成 ({NUM_DOCS} docs) ===")
    docs = [random_text() for _ in range(NUM_DOCS)]
    subjects = [random.choice(["history", "biology", "physics", "math"]) for _ in range(NUM_DOCS)]

    embedding_fn = model.DefaultEmbeddingFunction()

    with timed("embedding_encode") as t_embed:
        vectors = embedding_fn.encode_documents(docs)

    embed_per_doc = t_embed.elapsed / NUM_DOCS * 1000
    print(f"  每条 embedding: {embed_per_doc:.2f}ms")

    # ============ 阶段2: 数据插入 ============
    print(f"\n=== 阶段2: 数据插入 ({NUM_DOCS} docs) ===")
    data = [
        {"id": i, "vector": vectors[i], "text": docs[i], "subject": subjects[i]}
        for i in range(NUM_DOCS)
    ]

    # 分批插入，测吞吐
    BATCH_SIZE = 100
    insert_times = []
    for start in range(0, NUM_DOCS, BATCH_SIZE):
        batch = data[start:start + BATCH_SIZE]
        with timed(f"insert_batch_{start}") as t_ins:
            client.insert(collection_name=COLLECTION, data=batch)
        insert_times.append(t_ins.elapsed)

    total_insert = sum(insert_times)
    print(f"  总插入: {total_insert:.3f}s, 吞吐: {NUM_DOCS / total_insert:.0f} docs/s")

    # ============ 阶段3: 纯向量搜索 (无 filter) ============
    print(f"\n=== 阶段3: 纯向量搜索 ({SEARCH_ROUNDS} rounds, top_k={SEARCH_TOP_K}) ===")

    # 预生成查询向量，排除 embedding 时间
    query_texts = [random_text(20) for _ in range(SEARCH_ROUNDS)]
    with timed("query_embedding_all") as t_qembed:
        query_vectors_all = embedding_fn.encode_queries(query_texts)
    print(f"  查询 embedding: {t_qembed.elapsed / SEARCH_ROUNDS * 1000:.2f}ms/query")

    search_latencies = []
    for i in range(SEARCH_ROUNDS):
        t0 = time.perf_counter()
        client.search(
            collection_name=COLLECTION,
            data=[query_vectors_all[i]],
            limit=SEARCH_TOP_K,
            output_fields=["text", "subject"],
        )
        lat = time.perf_counter() - t0
        search_latencies.append(lat * 1000)  # ms

    print(f"  Search P50: {statistics.median(search_latencies):.2f}ms")
    print(f"  Search P95: {sorted(search_latencies)[int(0.95 * len(search_latencies))]:.2f}ms")
    print(f"  Search P99: {sorted(search_latencies)[int(0.99 * len(search_latencies))]:.2f}ms")
    print(f"  Search Avg: {statistics.mean(search_latencies):.2f}ms")

    # ============ 阶段4: 带标量过滤的搜索 ============
    print(f"\n=== 阶段4: 带 filter 搜索 ({SEARCH_ROUNDS} rounds) ===")

    filter_latencies = []
    for i in range(SEARCH_ROUNDS):
        subj = random.choice(["history", "biology", "physics", "math"])
        t0 = time.perf_counter()
        client.search(
            collection_name=COLLECTION,
            data=[query_vectors_all[i]],
            filter=f'subject == "{subj}"',
            limit=SEARCH_TOP_K,
            output_fields=["text", "subject"],
        )
        lat = time.perf_counter() - t0
        filter_latencies.append(lat * 1000)

    print(f"  Filter Search P50: {statistics.median(filter_latencies):.2f}ms")
    print(f"  Filter Search P95: {sorted(filter_latencies)[int(0.95 * len(filter_latencies))]:.2f}ms")
    print(f"  Filter Search Avg: {statistics.mean(filter_latencies):.2f}ms")

    # ============ 阶段5: 多线程并发搜索 (定位CPU瓶颈) ============
    print(f"\n=== 阶段5: 并发搜索压测 ===")

    import concurrent.futures

    for concurrency in SEARCH_CONCURRENCY:
        # 每个并发度跑固定轮数
        rounds_per_thread = max(20, SEARCH_ROUNDS // concurrency)
        total_ops = concurrency * rounds_per_thread

        def search_worker(worker_id):
            lats = []
            for j in range(rounds_per_thread):
                idx = (worker_id * rounds_per_thread + j) % len(query_vectors_all)
                t0 = time.perf_counter()
                client.search(
                    collection_name=COLLECTION,
                    data=[query_vectors_all[idx]],
                    limit=SEARCH_TOP_K,
                    output_fields=["text", "subject"],
                )
                lats.append((time.perf_counter() - t0) * 1000)
            return lats

        t_start = time.perf_counter()
        all_lats = []
        with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = [pool.submit(search_worker, w) for w in range(concurrency)]
            for f in concurrent.futures.as_completed(futures):
                all_lats.extend(f.result())
        wall_time = time.perf_counter() - t_start

        all_lats.sort()
        print(f"  并发={concurrency:2d} | QPS={total_ops / wall_time:.0f} | "
              f"P50={all_lats[len(all_lats)//2]:.2f}ms | "
              f"P99={all_lats[int(0.99*len(all_lats))]:.2f}ms | "
              f"wall={wall_time:.2f}s")

    # ============ 阶段6: 纯向量计算 vs 存储读取 对比 ============
    print(f"\n=== 阶段6: 随机向量搜索 (排除 embedding 开销) ===")

    random_queries = [np.random.rand(DIM).astype(np.float32).tolist() for _ in range(SEARCH_ROUNDS)]
    raw_search_lats = []
    for i in range(SEARCH_ROUNDS):
        t0 = time.perf_counter()
        client.search(
            collection_name=COLLECTION,
            data=[random_queries[i]],
            limit=SEARCH_TOP_K,
            output_fields=["text"],
        )
        lat = time.perf_counter() - t0
        raw_search_lats.append(lat * 1000)

    print(f"  Raw Search P50: {statistics.median(raw_search_lats):.2f}ms")
    print(f"  Raw Search P99: {sorted(raw_search_lats)[int(0.99 * len(raw_search_lats))]:.2f}ms")
    print(f"  Raw Search Avg: {statistics.mean(raw_search_lats):.2f}ms")

    # ============ 汇总 ============
    print("\n" + "=" * 60)
    print("瓶颈分析汇总:")
    print("=" * 60)
    print(f"  Embedding 生成:    {embed_per_doc:.2f} ms/doc")
    print(f"  数据插入吞吐:     {NUM_DOCS / total_insert:.0f} docs/s")
    print(f"  纯向量搜索 P50:   {statistics.median(search_latencies):.2f} ms")
    print(f"  带 filter 搜索 P50: {statistics.median(filter_latencies):.2f} ms")
    print(f"  随机向量搜索 P50: {statistics.median(raw_search_lats):.2f} ms")
    print(f"  查询 embedding:   {t_qembed.elapsed / SEARCH_ROUNDS * 1000:.2f} ms/query")
    print()

    if embed_per_doc > statistics.median(search_latencies):
        print("  >>> 瓶颈: EMBEDDING 生成 (模型推理)")
        print("      搜索本身很快，大部分时间花在 embedding 模型上")
    elif statistics.median(search_latencies) > 10:
        print("  >>> 瓶颈: 向量索引搜索 (CPU 计算)")
        print("      考虑: 减少数据量 / 换更快索引 / 加 CPU")
    else:
        print("  >>> 搜索延迟很低，当前数据规模下无明显瓶颈")

    if statistics.median(filter_latencies) > statistics.median(search_latencies) * 1.5:
        print("  >>> 标量过滤增加了显著开销，考虑为 filter 字段建标量索引")

    # 清理
    client.drop_collection(collection_name=COLLECTION)
    print("\nDone. Collection dropped.")


if __name__ == "__main__":
    main()
