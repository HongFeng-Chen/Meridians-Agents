package models

import (
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/index"
)

// —— 索引定义 ——

// HNSWCosine dense 向量索引（各集合统一参数）
func HNSWCosine(field string) index.Index {
	return index.NewHNSWIndex(entity.COSINE, 8, 256)
}

// BM25Index sparse 稀疏向量索引（配合 BM25 Function 使用 AutoIndex）
func BM25Index() index.Index {
	return index.NewAutoIndex(entity.BM25)
}
