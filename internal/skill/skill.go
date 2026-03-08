// Package skill 实现 OpenClaw/AgentSkills 兼容的 Skill 加载与搜索。
// 支持门控（requires.bins/env）、注册表安装、依赖安装器（brew/download）。
package skill

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Skill 表示一个可复用的技能：名称、描述与正文说明（供模型遵循）。
type Skill struct {
	Name        string   // 唯一标识，用于搜索与引用
	Description string   // 简短描述
	Body        string   // SKILL.md 中 frontmatter 之后的正文
	Dir         string   // 所在目录（用于 prepare 时执行安装器）
	Disabled    bool     // 门控未通过时为 true
	Missing     []string // 未满足的依赖描述，如 "缺少二进制: ffmpeg"
	Meta        *SkillMeta
}

// SkillMeta 与 OpenClaw metadata.openclaw 兼容
type SkillMeta struct {
	Requires *Requires  `json:"requires,omitempty"`
	Install  []Installer `json:"install,omitempty"`
}

type Requires struct {
	Bins    []string `json:"bins,omitempty"`    // 全部需在 PATH
	Env     []string `json:"env,omitempty"`     // 环境变量需已设置
	AnyBins []string `json:"anyBins,omitempty"` // 至少一个在 PATH
}

type Installer struct {
	Kind     string `json:"kind"`               // brew | download | node | go
	Formula  string `json:"formula,omitempty"`  // brew 包名
	URL      string `json:"url,omitempty"`     // download 的 URL
	Archive  string `json:"archive,omitempty"`  // tar.gz | zip
	Bins     []string `json:"bins,omitempty"`  // 安装后需存在的二进制
	NodePkg  string `json:"package,omitempty"` // node 包名
}

const (
	skillFile     = "SKILL.md"
	maxBodyRunes  = 4000   // 单 skill 正文截断，控制 token
	maxTotalRunes = 12000  // 注入到 system 的 skills 总长度上限
	extraDirsEnv  = "WILL_SKILLS_EXTRA_DIRS"
)

// Load 从默认及可选目录加载所有 Skill。优先级：工作区 ./skills > ~/.will/skills > 环境变量额外目录。
// 同名 Skill 只保留先扫描到的。workDir 为程序工作目录，传空则用当前工作目录。
func Load(workDir string) []Skill {
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	home, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(workDir, "skills"),
		filepath.Join(home, ".will", "skills"),
	}
	if v := os.Getenv(extraDirsEnv); v != "" {
		for _, d := range strings.Split(v, ",") {
			if d = strings.TrimSpace(d); d != "" {
				dirs = append(dirs, d)
			}
		}
	}

	seen := make(map[string]struct{})
	var out []Skill
	for _, dir := range dirs {
		skills := loadDir(dir)
		for _, s := range skills {
			if s.Name == "" {
				continue
			}
			if _, ok := seen[s.Name]; ok {
				continue
			}
			if s.Disabled {
				continue
			}
			seen[s.Name] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// LoadAll 与 Load 相同目录顺序，但返回全部 Skill（含门控未通过的），用于 CLI 列表展示。
func LoadAll(workDir string) []Skill {
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	home, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(workDir, "skills"),
		filepath.Join(home, ".will", "skills"),
	}
	if v := os.Getenv(extraDirsEnv); v != "" {
		for _, d := range strings.Split(v, ",") {
			if d = strings.TrimSpace(d); d != "" {
				dirs = append(dirs, d)
			}
		}
	}
	seen := make(map[string]struct{})
	var out []Skill
	for _, dir := range dirs {
		skills := loadDir(dir)
		for _, s := range skills {
			if s.Name == "" {
				continue
			}
			if _, ok := seen[s.Name]; ok {
				continue
			}
			seen[s.Name] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func loadDir(dir string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillDir := filepath.Join(dir, e.Name())
		path := filepath.Join(skillDir, skillFile)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		s := parseSKILL(string(data))
		if s != nil && s.Name != "" {
			s.Dir = skillDir
			gateCheck(s)
			out = append(out, *s)
		}
	}
	return out
}

// parseSKILL 解析 SKILL.md：YAML frontmatter（---...---） + 正文；含 metadata.openclaw。
func parseSKILL(content string) *Skill {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "---") {
		return nil
	}
	rest := strings.TrimPrefix(content[3:], "\n")
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil
	}
	front := rest[:idx]
	body := strings.TrimSpace(rest[idx+4:])

	name := frontValue(front, "name")
	desc := frontValue(front, "description")
	if name == "" {
		return nil
	}
	if desc == "" {
		desc = name
	}
	// 正文截断
	if runeCount(body) > maxBodyRunes {
		body = runeTruncate(body, maxBodyRunes) + "\n…"
	}
	s := &Skill{Name: name, Description: desc, Body: body}
	if meta := frontMetadata(front); meta != nil {
		s.Meta = meta
	}
	return s
}

// frontMetadata 从 frontmatter 中解析 metadata 块（JSON），提取 openclaw 段。
func frontMetadata(front string) *SkillMeta {
	raw := frontMetadataRaw(front)
	if raw == "" {
		return nil
	}
	var out struct {
		Openclaw *SkillMeta `json:"openclaw"`
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out.Openclaw
}

func frontMetadataRaw(front string) string {
	lines := strings.Split(front, "\n")
	var in bool
	var b strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "metadata:") {
			in = true
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "metadata:"))
			if rest != "" && rest != "|" && rest != ">" {
				b.WriteString(rest)
				b.WriteByte('\n')
			}
			continue
		}
		if in {
			if trimmed == "" {
				b.WriteByte('\n')
				continue
			}
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				b.WriteString(strings.TrimSpace(line))
				b.WriteByte('\n')
			} else {
				break
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// gateCheck 根据 requires 检查环境，未通过则设置 Disabled 与 Missing。
func gateCheck(s *Skill) {
	if s.Meta == nil || s.Meta.Requires == nil {
		return
	}
	r := s.Meta.Requires
	for _, bin := range r.Bins {
		if _, err := exec.LookPath(bin); err != nil {
			s.Disabled = true
			s.Missing = append(s.Missing, "缺少二进制: "+bin)
		}
	}
	for _, key := range r.Env {
		if os.Getenv(key) == "" {
			s.Disabled = true
			s.Missing = append(s.Missing, "缺少环境变量: "+key)
		}
	}
	if len(r.AnyBins) > 0 {
		var found bool
		for _, bin := range r.AnyBins {
			if _, err := exec.LookPath(bin); err == nil {
				found = true
				break
			}
		}
		if !found {
			s.Disabled = true
			s.Missing = append(s.Missing, "需要以下任一二进制: "+strings.Join(r.AnyBins, ", "))
		}
	}
}

func frontValue(front, key string) string {
	key = key + ":"
	for _, line := range strings.Split(front, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key) {
			return strings.TrimSpace(strings.TrimPrefix(line, key))
		}
	}
	return ""
}

func runeCount(s string) int { return len([]rune(s)) }

func runeTruncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// FormatForPrompt 将 Skill 列表格式化为注入到 system prompt 的文本，总长度受 maxTotalRunes 限制。
func FormatForPrompt(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【可用 Skills】根据用户意图搜索并复用以下技能；按技能说明执行。\n")
	used := 0
	for i := range skills {
		s := &skills[i]
		block := "\n## " + s.Name + "\n" + s.Description + "\n\n" + s.Body + "\n"
		if used+runeCount(block) > maxTotalRunes {
			b.WriteString("\n（其余 Skill 已省略）\n")
			break
		}
		b.WriteString(block)
		used += runeCount(block)
	}
	return b.String()
}
