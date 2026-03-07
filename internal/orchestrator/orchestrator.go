package orchestrator

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/yourusername/will/internal/config"
	"github.com/yourusername/will/internal/llm"
)

// Task 子任务：step 为单句描述（如「列出待办」），after 为依赖的任务 id
type Task struct {
	ID    int      `json:"id"`
	Step  string   `json:"step"`
	After []int    `json:"after"`
}

// Plan 规划结果
type Plan struct {
	Tasks []Task `json:"tasks"`
}

// StepRunner 执行单步的抽象，由 main 注入 runWithLLM
type StepRunner func(step string) string

// TopoSort 按依赖分波，同波内可并行；返回 [wave0, wave1, ...]
func TopoSort(plan Plan) [][]Task {
	if len(plan.Tasks) == 0 {
		return nil
	}
	idToTask := make(map[int]Task)
	for _, t := range plan.Tasks {
		idToTask[t.ID] = t
	}
	var waves [][]Task
	done := make(map[int]bool)
	for len(done) < len(plan.Tasks) {
		var wave []Task
		for _, t := range plan.Tasks {
			if done[t.ID] {
				continue
			}
			ready := true
			for _, dep := range t.After {
				if !done[dep] {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, t)
			}
		}
		if len(wave) == 0 {
			break
		}
		for _, t := range wave {
			done[t.ID] = true
		}
		waves = append(waves, wave)
	}
	return waves
}

// Planner 用 LLM 将用户请求拆解为子任务；解析失败时降级为单任务计划
func Planner(cfg *config.Config, userMessage string) (Plan, error) {
	sys := `你是 WILL 机器人的任务规划助手。WILL 的能力仅限于：查看/添加/完成/删除待办、查看/添加/修改/删除定时任务、检查程序版本、普通对话和记忆、执行 shell 命令。
根据用户请求，只生成 WILL 实际能执行的子任务，拆解为可执行步骤。必须输出纯 JSON（不要其他文字）：
{"tasks":[{"id":1,"step":"子任务一句话，使用 WILL 支持的操作","after":[]},{"id":2,"step":"另一步","after":[1]}]}
规则：id 从 1 开始；step 必须是 WILL 能完成的操作（如：查看待办、删除待办2、添加定时任务...）；after 填依赖的 id，可并行填 []；只有一个操作时只填一个 task。`
	user := "用户说：" + userMessage

	var plan Plan
	// 最多尝试 2 次
	for attempt := 0; attempt < 2; attempt++ {
		content, err := llm.CallChat(cfg, sys, user)
		if err != nil {
			return Plan{}, err
		}
		jsonStr := extractJSON(content)
		if jsonErr := json.Unmarshal([]byte(jsonStr), &plan); jsonErr == nil && len(plan.Tasks) > 0 {
			sort.Slice(plan.Tasks, func(i, j int) bool { return plan.Tasks[i].ID < plan.Tasks[j].ID })
			return plan, nil
		}
	}
	// 降级：整个消息作为单一任务
	return Plan{Tasks: []Task{{ID: 1, Step: userMessage, After: []int{}}}}, nil
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	for _, fence := range []string{"```json", "```"} {
		if strings.HasPrefix(s, fence) {
			s = strings.TrimPrefix(s, fence)
			if idx := strings.LastIndex(s, "```"); idx > 0 {
				s = s[:idx]
			}
			s = strings.TrimSpace(s)
			break
		}
	}
	start := strings.Index(s, "{")
	if start < 0 {
		return s
	}
	depth, i := 0, start
	runes := []rune(s)
	inStr, escape := false, false
	for i < len(runes) {
		ch := runes[i]
		if escape {
			escape = false
			i++
			continue
		}
		if ch == '\\' && inStr {
			escape = true
			i++
			continue
		}
		if ch == '"' {
			inStr = !inStr
			i++
			continue
		}
		if !inStr {
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 {
					return string(runes[start : i+1])
				}
			}
		}
		i++
	}
	return string(runes[start:])
}

// ReviewResult 审查结果
type ReviewResult struct {
	OK          bool   `json:"ok"`
	Reply       string `json:"reply"`
	ReworkTasks []int  `json:"rework_tasks"`
	Reason      string `json:"reason"`
}

// buildResultSummary 把各步结果拼成可读文本，用于降级回复
func buildResultSummary(plan Plan, results map[int]string) string {
	if len(plan.Tasks) == 1 {
		return results[plan.Tasks[0].ID]
	}
	var b strings.Builder
	for _, t := range plan.Tasks {
		b.WriteString(fmt.Sprintf("▸ %s\n%s\n", t.Step, results[t.ID]))
	}
	return strings.TrimSpace(b.String())
}

// Reviewer 根据用户请求、计划与各步结果，判断是否完成得当并生成回复或返工建议；解析失败时降级返回汇总
func Reviewer(cfg *config.Config, userMessage string, plan Plan, results map[int]string) (ReviewResult, error) {
	var buf strings.Builder
	buf.WriteString("用户请求：" + userMessage + "\n\n各子任务执行结果：\n")
	for _, t := range plan.Tasks {
		r := results[t.ID]
		if len([]rune(r)) > 300 {
			r = string([]rune(r)[:300]) + "…"
		}
		buf.WriteString(fmt.Sprintf("[%d] %s → %s\n", t.ID, t.Step, r))
	}
	sys := `你是质量审查助手。判断各子任务是否完成用户请求。必须输出纯 JSON（不要其他文字）：
完成得好：{"ok":true,"reply":"给用户的汇总回复，简洁友好，合并各步结果，不要暴露内部任务 id"}
需返工：{"ok":false,"reply":"","rework_tasks":[失败的任务id数字列表],"reason":"一句原因"}。只填真正失败的任务。`
	content, err := llm.CallChat(cfg, sys, buf.String())
	if err != nil {
		// 审查失败降级：直接用各步结果拼汇总，标记 ok=true
		return ReviewResult{OK: true, Reply: buildResultSummary(plan, results)}, nil
	}
	jsonStr := extractJSON(content)
	var r ReviewResult
	if err := json.Unmarshal([]byte(jsonStr), &r); err != nil {
		// 解析失败降级
		return ReviewResult{OK: true, Reply: buildResultSummary(plan, results)}, nil
	}
	return r, nil
}

// Run 多智能体流程：规划 -> 按波执行（同波并行）-> 审查 -> 必要时返工一次
func Run(cfg *config.Config, userMessage string, runStep StepRunner) string {
	plan, err := Planner(cfg, userMessage)
	if err != nil {
		return "规划失败 — " + err.Error()
	}
	if len(plan.Tasks) == 0 {
		return "未拆解出子任务，请换个说法。"
	}
	idToTask := make(map[int]Task)
	for _, t := range plan.Tasks {
		idToTask[t.ID] = t
	}
	results := make(map[int]string)
	var resultsMu sync.Mutex

	runWave := func(wave []Task) {
		var wg sync.WaitGroup
		for _, t := range wave {
			wg.Add(1)
			go func(task Task) {
				defer wg.Done()
				out := runStep(strings.TrimSpace(task.Step))
				resultsMu.Lock()
				results[task.ID] = out
				resultsMu.Unlock()
			}(t)
		}
		wg.Wait()
	}

	waves := TopoSort(plan)
	for _, wave := range waves {
		runWave(wave)
	}

	// 单任务无需审查，直接返回执行结果
	if len(plan.Tasks) == 1 {
		return results[plan.Tasks[0].ID]
	}

	review, err := Reviewer(cfg, userMessage, plan, results)
	if err != nil {
		return buildResultSummary(plan, results)
	}
	if review.OK {
		if review.Reply != "" {
			return review.Reply
		}
		return buildResultSummary(plan, results)
	}
	if len(review.ReworkTasks) > 0 {
		for _, id := range review.ReworkTasks {
			if t, ok := idToTask[id]; ok {
				results[id] = runStep(t.Step)
			}
		}
		review2, err := Reviewer(cfg, userMessage, plan, results)
		if err == nil {
			if review2.Reply != "" {
				return review2.Reply
			}
			return buildResultSummary(plan, results)
		}
	}
	return buildResultSummary(plan, results)
}
