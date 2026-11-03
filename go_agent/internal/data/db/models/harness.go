// harness.go 评测 Harness 持久化模型。
//
// 四张表：
//
//	harness_sets      用例集（被测对象 + 元信息）
//	harness_cases     用例（输入 + 期望 + 断言规则，JSON 列存数组）
//	harness_runs      运行行（进度实时 UPDATE，前端轮询）
//	harness_run_items 逐例结果（输出 + 逐条断言判定）
package models

import "time"

// HarnessSetPO 用例集
type HarnessSetPO struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	Name        string    `gorm:"column:name;size:128;not null" json:"name"`
	Description string    `gorm:"column:description;size:512;not null;default:''" json:"description"`
	Target      string    `gorm:"column:target;size:128;not null;default:''" json:"target"`
	Tags        string    `gorm:"column:tags;size:512;not null;default:'[]'" json:"tags"`
	Enabled     bool      `gorm:"column:is_enabled;not null;default:true" json:"enabled"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName 表名
func (HarnessSetPO) TableName() string { return "harness_sets" }

// HarnessCasePO 评测用例
type HarnessCasePO struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	SetID     uint64    `gorm:"column:set_id;not null;index" json:"set_id"`
	Name      string    `gorm:"column:name;size:128;not null;default:''" json:"name"`
	Input     string    `gorm:"column:input;type:text;not null" json:"input"`
	Context   string    `gorm:"column:context;type:text;not null" json:"context"`
	Expect    string    `gorm:"column:expect;type:text;not null" json:"expect"`
	Checks    string    `gorm:"column:checks;type:text;not null" json:"checks"` // JSON []Check
	Tags      string    `gorm:"column:tags;size:512;not null;default:'[]'" json:"tags"`
	Enabled   bool      `gorm:"column:is_enabled;not null;default:true" json:"enabled"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName 表名
func (HarnessCasePO) TableName() string { return "harness_cases" }

// HarnessRunPO 评测运行（进度行）
type HarnessRunPO struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	SetID     uint64    `gorm:"column:set_id;not null;index" json:"set_id"`
	SetName   string    `gorm:"column:set_name;size:128;not null;default:''" json:"set_name"`
	Target    string    `gorm:"column:target;size:128;not null;default:''" json:"target"`
	Status    string    `gorm:"column:status;size:16;not null;default:'pending';index" json:"status"`
	Total     int       `gorm:"column:total;not null;default:0" json:"total"`
	Done      int       `gorm:"column:done;not null;default:0" json:"done"`
	Passed    int       `gorm:"column:passed;not null;default:0" json:"passed"`
	Failed    int       `gorm:"column:failed;not null;default:0" json:"failed"`
	Errored   int       `gorm:"column:errored;not null;default:0" json:"errored"`
	Score     float64   `gorm:"column:score;not null;default:0" json:"score"`
	CostMS    int64     `gorm:"column:cost_ms;not null;default:0" json:"cost_ms"`
	Config    string    `gorm:"column:config;size:512;not null;default:'{}'" json:"config"` // JSON RunConfig
	Error     string    `gorm:"column:error;size:512;not null;default:''" json:"error"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	// FinishedAt 终态时间；*time.Time 让未结束的运行写 NULL（MySQL 拒绝零值 datetime）
	FinishedAt *time.Time `gorm:"column:finished_at" json:"finished_at"`
}

// TableName 表名
func (HarnessRunPO) TableName() string { return "harness_runs" }

// HarnessRunItemPO 单用例运行结果
type HarnessRunItemPO struct {
	ID               uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	RunID            uint64    `gorm:"column:run_id;not null;index" json:"run_id"`
	CaseID           uint64    `gorm:"column:case_id;not null;default:0" json:"case_id"`
	CaseName         string    `gorm:"column:case_name;size:128;not null;default:''" json:"case_name"`
	Output           string    `gorm:"column:output;type:mediumtext" json:"output"`
	Checks           string    `gorm:"column:checks;type:text;not null" json:"checks"` // JSON []CheckResult
	Passed           bool      `gorm:"column:passed;not null;default:false" json:"passed"`
	Score            float64   `gorm:"column:score;not null;default:0" json:"score"`
	LatencyMS        int64     `gorm:"column:latency_ms;not null;default:0" json:"latency_ms"`
	PromptTokens     int64     `gorm:"column:prompt_tokens;not null;default:0" json:"prompt_tokens"`
	CompletionTokens int64     `gorm:"column:completion_tokens;not null;default:0" json:"completion_tokens"`
	Error            string    `gorm:"column:error;size:512;not null;default:''" json:"error"`
	CreatedAt        time.Time `gorm:"column:created_at" json:"created_at"`
}

// TableName 表名
func (HarnessRunItemPO) TableName() string { return "harness_run_items" }
