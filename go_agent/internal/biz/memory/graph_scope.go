package memory

// 记忆图谱的归属隔离。
//
// 背景：记忆与 RAG 文档共用同一套图谱集合（graphrag.Store），实体表里
// 只有 name / type / description / freq / refs，没有 user_id / tenant_id。
// 于是「Milvus」这类通用实体会被所有用户的记忆合并成同一个节点：
// 频率累加、描述取更长的那条、引用混在一起。查询侧再没有任何过滤，
// 于是 A 的记忆里抽出来的实体描述会出现在 B 的召回结果里。
//
// 没有独立字段可用时，唯一能承载归属的是引用本身。所以记忆引用从
// "<memoryID>#0" 改为 "<user 前缀>::<memoryID>#0"：
//
//	写入：只与「同一个用户的既有实体」合并（不再跨用户累加描述/频率）
//	读取：只保留含本用户引用的实体/关系，并裁掉其中的他人引用
//	删除：按带归属的前缀清理孤儿节点
//
// 代价：升级前的旧引用（"<memoryID>#0"）不再被识别，历史记忆的图谱节点
// 会从召回中消失——这是「静默丢一部分增强能力」换「不再跨用户泄漏」，
// 需要保留历史图谱的话应做一次引用前缀回填（见 DEPLOY 说明）。

import "strings"

const (
	// memoryRefRoot 记忆引用的统一根前缀，与文档引用（"<docID>#idx"）区分
	memoryRefRoot = "mem::"
	// memoryRefSep 归属与记忆 ID 的分隔符。用 "::" 而不是 ":"，
	// 避免 "alice" 与 "alice2" 这种前缀关系互相误命中。
	memoryRefSep = "::"
)

// memoryRef 记忆图谱引用："mem::<userID>::<memoryID>#0"（单块记忆固定 #0）
func memoryRef(userID, memoryID string) string {
	return memoryRefOwnerPrefix(userID) + memoryID + "#0"
}

// memoryRefOwnerPrefix 某用户全部记忆引用的前缀（用于归属判定与批量删除）
func memoryRefOwnerPrefix(userID string) string {
	if strings.TrimSpace(userID) == "" {
		userID = "default"
	}
	return memoryRefRoot + userID + memoryRefSep
}

// memoryRefDocPrefix 单条记忆的引用前缀（删除孤儿节点用）
func memoryRefDocPrefix(userID, memoryID string) string {
	return memoryRefOwnerPrefix(userID) + memoryID + "#"
}

// refsOwnedBy 引用集合里是否有属于该用户的项
func refsOwnedBy(refs []string, userID string) bool {
	prefix := memoryRefOwnerPrefix(userID)
	for _, r := range refs {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

// pruneRefs 只保留属于该用户的引用（返回新切片，不改原对象）
func pruneRefs(refs []string, userID string) []string {
	prefix := memoryRefOwnerPrefix(userID)
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if strings.HasPrefix(r, prefix) {
			out = append(out, r)
		}
	}
	return out
}
