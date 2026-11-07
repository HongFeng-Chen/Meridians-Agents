package agentfactory

// 服务端意图推导：不依赖模型守规矩。
//
// CreateByLLM 的提示词只是「请求」模型填 auto_run/cron/auto_compose——模型漏填时
// 这些意图会静默丢失（用户写了「每小时巡检」却得到一个手动 Agent）。本文件把
// 周期执行与多 Agent 协同两类高频意图下沉为服务端保证：模型没给，就从需求原文
// 推导；给了但非法，先走 sanitizeFactoryIntent 的降级路径。
//
// 推导规则刻意保守：只在需求文本出现明确语义时生效，识别不到就不动模型的输出。

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	reEveryNMinutes = regexp.MustCompile(`每(?:隔)?\s*(\d{1,2})\s*分钟`)
	reHalfHour      = regexp.MustCompile(`每(?:隔)?\s*半\s*(?:个)?\s*小时`)
	reEveryNHours   = regexp.MustCompile(`每(?:隔)?\s*(\d{1,2})\s*(?:个)?\s*小时`)
	reHourly        = regexp.MustCompile(`每小时|每个小时|按小时`)
	reWorkdayAt     = regexp.MustCompile(`工作日(?:的)?(?:早上|上午|凌晨|晚上)?\s*(\d{1,2})\s*[点时]`)
	reWorkday       = regexp.MustCompile(`工作日`)
	reWeeklyAt      = regexp.MustCompile(`每[周星期]\s*([一二三四五六日天1-6])\s*(?:的)?(?:早上|上午)?\s*(\d{1,2})\s*[点时]`)
	reWeekly        = regexp.MustCompile(`每周|每个星期`)
	reDailyAt       = regexp.MustCompile(`每[天日](?:的)?(?:早上|上午|凌晨|傍晚|晚上)?\s*(\d{1,2})\s*[点时]`)
	reDaily         = regexp.MustCompile(`每天|每日`)
	rePeriodicWord  = regexp.MustCompile(`定时|定期|周期|轮询|按时|自动巡检|后台任务|定时任务|自动运行|定期执行`)

	// reCollab 多 Agent 协同语义：需求出现即认为用户要的是「组队干活」，
	// 对应 auto_compose=true（首次任务运行时由 LLM 规划架构并自动创建子 Agent）。
	reCollab = regexp.MustCompile(`子\s*agent|多个?\s*agent|多\s*agent|multi-agent|协同|协作|组队|团队|分工|编排|流水线|串联|并行`)
)

// cnWeekday 中文周几 → cron 周字段（0=周日）
var cnWeekday = map[string]int{"日": 0, "天": 0, "一": 1, "二": 2, "三": 3, "四": 4, "五": 5, "六": 6,
	"1": 1, "2": 2, "3": 3, "4": 4, "5": 5, "6": 6}

// deriveCronFromRequirement 从需求原文推导 cron 表达式。
// 返回（表达式, 依据描述）；识别不到周期语义时返回空串。
// 识别顺序从具体到宽泛：N 分钟 > 半小时 > N 小时 > 每小时 > 工作日 X 点 > 每周 X 的 X 点 > 每天 X 点 > 泛周期词。
func deriveCronFromRequirement(req string) (string, string) {
	req = strings.TrimSpace(req)
	if req == "" {
		return "", ""
	}
	if m := reEveryNMinutes.FindStringSubmatch(req); m != nil {
		n := clampNum(m[1], 1, 59)
		return "*/" + n + " * * * *", "每 " + n + " 分钟"
	}
	if reHalfHour.MatchString(req) {
		return "*/30 * * * *", "每半小时"
	}
	if m := reEveryNHours.FindStringSubmatch(req); m != nil {
		n := clampNum(m[1], 1, 23)
		return "0 */" + n + " * * *", "每 " + n + " 小时"
	}
	if reHourly.MatchString(req) {
		return "0 * * * *", "每小时"
	}
	if m := reWorkdayAt.FindStringSubmatch(req); m != nil {
		h := clampNum(m[1], 0, 23)
		return "0 " + h + " * * 1-5", "工作日 " + h + " 点"
	}
	if reWorkday.MatchString(req) {
		return "0 9 * * 1-5", "工作日（未指明时刻，默认 9 点）"
	}
	if m := reWeeklyAt.FindStringSubmatch(req); m != nil {
		w, ok := cnWeekday[m[1]]
		if !ok {
			w = 1
		}
		h := clampNum(m[2], 0, 23)
		return "0 " + h + " * * " + strconv.Itoa(w), "每周" + m[1] + " " + h + " 点"
	}
	if m := reDailyAt.FindStringSubmatch(req); m != nil {
		h := clampNum(m[1], 0, 23)
		return "0 " + h + " * * *", "每天 " + h + " 点"
	}
	if reDaily.MatchString(req) {
		return "0 9 * * *", "每天（未指明时刻，默认 9 点）"
	}
	if reWeekly.MatchString(req) {
		return "0 9 * * 1", "每周（未指明细节，默认周一 9 点）"
	}
	if rePeriodicWord.MatchString(req) {
		return "0 * * * *", "需求含定时/周期语义，未指明频率，默认每小时"
	}
	return "", ""
}

// requirementImpliesCollab 需求是否表达多 Agent 协同/编排意图
func requirementImpliesCollab(req string) bool {
	return reCollab.MatchString(strings.TrimSpace(req))
}

// clampNum 数字字符串夹取到 [lo, hi]，非法时取 lo
func clampNum(s string, lo, hi int) string {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	return strconv.Itoa(n)
}
