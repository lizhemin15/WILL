package skill

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// Prepare 尝试为指定 Skill 安装依赖（执行 metadata.openclaw.install 中首个可用的安装器）。
// 仅支持 kind=brew 与 kind=download；成功返回 nil。
func Prepare(skillName string) error {
	all := LoadAll("")
	var s *Skill
	for i := range all {
		if all[i].Name == skillName {
			s = &all[i]
			break
		}
	}
	if s == nil {
		return fmt.Errorf("未找到 Skill: %s", skillName)
	}
	if s.Meta == nil || len(s.Meta.Install) == 0 {
		return fmt.Errorf("Skill %s 未声明 install 步骤", skillName)
	}
	for _, in := range s.Meta.Install {
		if runInstaller(&in) == nil {
			return nil
		}
	}
	return fmt.Errorf("所有安装器均未成功，请手动安装依赖")
}

func runInstaller(in *Installer) error {
	switch in.Kind {
	case "brew":
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			return fmt.Errorf("brew 仅支持 macOS/Linux")
		}
		formula := in.Formula
		if formula == "" {
			return fmt.Errorf("brew 安装器缺少 formula")
		}
		cmd := exec.Command("brew", "install", formula)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		return nil
	case "download":
		// 依赖二进制下载需解压到 ~/.will/tools 并加入 PATH，暂不实现；用户可手动下载
		return fmt.Errorf("download 安装器暂不支持自动执行，请手动安装后重试")
	default:
		return fmt.Errorf("不支持的安装器: %s", in.Kind)
	}
}

// PrepareAll 对 LoadAll 中所有 Disabled 且声明了 install 的 Skill 尝试执行首个安装器。
func PrepareAll() (done []string, errs []string) {
	all := LoadAll("")
	for i := range all {
		s := &all[i]
		if !s.Disabled || s.Meta == nil || len(s.Meta.Install) == 0 {
			continue
		}
		if err := Prepare(s.Name); err != nil {
			errs = append(errs, s.Name+": "+err.Error())
		} else {
			done = append(done, s.Name)
		}
	}
	return done, errs
}
