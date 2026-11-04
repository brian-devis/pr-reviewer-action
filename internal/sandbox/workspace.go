package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// maxCopyBytes 是工作副本的大小上限，防止仓库里塞了巨大文件把磁盘写满。
	maxCopyBytes = 1 << 30
	// maxCopyFiles 是文件数上限。
	maxCopyFiles = 200000
)

// skipDirs 是不会复制进工作副本的顶层目录。
//
// .git 必须排除：actions/checkout 默认会把 GitHub Token 写在 .git/config 里，
// 复制进去就等于让沙盒里的 PR 代码读到令牌。
var skipDirs = map[string]bool{".git": true}

// CopyWorkspace 把 src 复制到 dst，作为沙盒使用的临时工作副本。
//
//   - 排除 .git，见 skipDirs 的说明。
//   - 符号链接按原样复制成链接，不会跟进去读取目标，所以指向宿主机路径的链接
//     在容器里只会指向容器自己的文件系统。
//   - 目录和文件被放宽权限，让容器里的 nobody 用户可以读写；副本本身是一次性的。
func CopyWorkspace(src, dst string) error {
	var total int64
	var files int
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.Chmod(dst, 0o777)
		}
		if d.IsDir() && skipDirs[rel] {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)

		files++
		if files > maxCopyFiles {
			return fmt.Errorf("工作副本文件数超过上限 %d", maxCopyFiles)
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o777)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			total += info.Size()
			if total > maxCopyBytes {
				return fmt.Errorf("工作副本大小超过上限 %d MiB", maxCopyBytes>>20)
			}
			return copyFile(path, target, info.Mode())
		default:
			return nil // 设备文件、套接字、管道等一律跳过
		}
	})
}

func copyFile(src, dst string, mode fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	perm := fs.FileMode(0o666)
	if mode&0o111 != 0 {
		perm = 0o777
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err := out.ReadFrom(in); err != nil {
		return err
	}
	// 创建时的权限会被 umask 削掉，这里显式放开
	return os.Chmod(dst, perm)
}

// HasFile 报告 dir 下是否存在名为 name 的普通文件。
func HasFile(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && info.Mode().IsRegular()
}

// GoVersion 从 go.mod 的 go 指令里读出版本，如 "1.22"，读不到返回空串。
func GoVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 2 && f[0] == "go" {
			parts := strings.Split(f[1], ".")
			if len(parts) >= 2 {
				return parts[0] + "." + parts[1]
			}
			return f[1]
		}
	}
	return ""
}

// ErrNoCommands 表示既没有配置命令，也无法自动识别项目类型。
var ErrNoCommands = errors.New("没有配置 sandbox_test_command / sandbox_lint_command，且无法根据项目自动识别")
