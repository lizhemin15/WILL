package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/yourusername/will/internal/skill"
)

func runSkillCLI() {
	args := os.Args[2:]
	if len(args) == 0 {
		printSkillHelp()
		os.Exit(0)
	}
	cmd := strings.ToLower(args[0])
	switch cmd {
	case "list":
		runSkillList(args[1:])
	case "install":
		runSkillInstall(args[1:])
	case "prepare":
		runSkillPrepare(args[1:])
	case "update":
		runSkillUpdate(args[1:])
	case "help", "-h", "--help":
		printSkillHelp()
	default:
		log.Fatalf("未知子命令: %s\n%s", cmd, skillHelpText())
	}
}

func printSkillHelp() {
	fmt.Print(skillHelpText())
}

func skillHelpText() string {
	return `will skill — 管理 Skills（ClawHub 风格）

   list              列出已加载的 Skill（含门控未通过的）
   list --remote     从注册表拉取并列出可安装的 Skill
   install <name>    从注册表安装指定名称的 Skill
   install <url>    从 zip/tar.gz 链接直接安装
   prepare <name>    为指定 Skill 安装依赖（执行 metadata.install，如 brew）
   update            从注册表更新已安装的 Skill（覆盖安装）

环境变量:
  WILL_SKILLS_REGISTRY_URL  注册表 JSON 地址（默认见文档）
  WILL_SKILLS_EXTRA_DIRS    额外 Skill 目录，逗号分隔
`
}

func runSkillList(args []string) {
	remote := false
	for _, a := range args {
		if a == "--remote" || a == "-r" {
			remote = true
			break
		}
	}
	if remote {
		entries, err := skill.FetchRegistry()
		if err != nil {
			log.Fatalf("拉取注册表失败: %v", err)
		}
		if len(entries) == 0 {
			fmt.Println("注册表为空")
			return
		}
		fmt.Println("可安装的 Skill（will skill install <name>）：")
		for _, e := range entries {
			fmt.Printf("  %s — %s\n", e.Name, e.Description)
		}
		return
	}
	all := skill.LoadAll("")
	if len(all) == 0 {
		fmt.Println("未加载任何 Skill。使用 will skill install <name> 或放入 ./skills、~/.will/skills")
		return
	}
	fmt.Println("已加载的 Skill：")
	for _, s := range all {
		status := "可用"
		if s.Disabled {
			status = "未就绪: " + strings.Join(s.Missing, "; ")
		}
		fmt.Printf("  %s — %s [%s]\n", s.Name, s.Description, status)
	}
}

func runSkillInstall(args []string) {
	if len(args) == 0 {
		log.Fatal("用法: will skill install <name|url>")
	}
	target := strings.TrimSpace(args[0])
	if target == "" {
		log.Fatal("用法: will skill install <name|url>")
	}
	var dir string
	var err error
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		dir, err = skill.InstallFromURL(target, "")
		if err != nil {
			log.Fatalf("安装失败: %v", err)
		}
		fmt.Printf("已从链接安装到 %s\n", dir)
		return
	}
	entries, err := skill.FetchRegistry()
	if err != nil {
		log.Fatalf("拉取注册表失败: %v", err)
	}
	for _, e := range entries {
		if e.Name == target {
			if e.RepoSubpath != "" {
				dir, err = skill.InstallFromRepoZip(e.URL, e.RepoSubpath, e.Name)
			} else {
				dir, err = skill.InstallFromURL(e.URL, e.Name)
			}
			if err != nil {
				log.Fatalf("安装 %s 失败: %v", target, err)
			}
			fmt.Printf("已安装 %s 到 %s\n", target, dir)
			return
		}
	}
	log.Fatalf("注册表中未找到 %s，请使用 will skill list --remote 查看可用列表，或使用 install <url> 直接安装", target)
}

func runSkillPrepare(args []string) {
	if len(args) == 0 {
		log.Fatal("用法: will skill prepare <name>")
	}
	name := strings.TrimSpace(args[0])
	if err := skill.Prepare(name); err != nil {
		log.Fatalf("prepare 失败: %v", err)
	}
	fmt.Printf("已为 %s 安装依赖\n", name)
}

func runSkillUpdate(args []string) {
	entries, err := skill.FetchRegistry()
	if err != nil {
		log.Fatalf("拉取注册表失败: %v", err)
	}
	installed := skill.LoadAll("")
	updated := 0
	for _, e := range entries {
		for _, s := range installed {
			if s.Name == e.Name {
				_, err := skill.InstallFromURL(e.URL, e.Name)
				if err != nil {
					log.Printf("更新 %s 失败: %v", e.Name, err)
					continue
				}
				fmt.Printf("已更新 %s\n", e.Name)
				updated++
				break
			}
		}
	}
	if updated == 0 {
		fmt.Println("没有可从注册表更新的 Skill")
	} else {
		fmt.Printf("共更新 %d 个 Skill\n", updated)
	}
}
