package updater

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// ApplyUpdate 将已校验的新版本二进制文件替换到当前正在运行的程序文件位置。
func ApplyUpdate(downloadedPath string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	if realPath, err := filepath.EvalSymlinks(execPath); err == nil && realPath != "" {
		execPath = realPath
	}

	// 目标程序所在目录
	targetDir := filepath.Dir(execPath)
	targetName := filepath.Base(execPath)

	if runtime.GOOS == "windows" {
		// Windows: 运行中 exe 不可写，但可重命名。
		oldPath := filepath.Join(targetDir, targetName+".old")
		_ = os.Remove(oldPath) // 尝试清理历史 .old 文件

		if err := os.Rename(execPath, oldPath); err != nil {
			return fmt.Errorf("rename running binary to .old: %w", err)
		}

		// 将下载的新文件移动/复制到目标位置
		if err := moveOrCopy(downloadedPath, execPath); err != nil {
			// 若移动失败，尝试回滚还原旧文件
			_ = os.Rename(oldPath, execPath)
			return fmt.Errorf("place new binary: %w", err)
		}
	} else {
		// Linux / macOS: 使用同目录临时文件原子 Rename 替换
		tmpTarget := filepath.Join(targetDir, "."+targetName+".new")
		_ = os.Remove(tmpTarget)

		if err := moveOrCopy(downloadedPath, tmpTarget); err != nil {
			return fmt.Errorf("copy to target dir: %w", err)
		}

		// 赋予可执行权限 0755
		_ = os.Chmod(tmpTarget, 0755)

		if err := os.Rename(tmpTarget, execPath); err != nil {
			_ = os.Remove(tmpTarget)
			return fmt.Errorf("atomic rename new binary: %w", err)
		}
		_ = os.Chmod(execPath, 0755)
	}

	_ = os.Remove(downloadedPath)
	return nil
}

// CleanupOldFiles 清理旧版残留的 .old 文件。在程序启动时调用。
func CleanupOldFiles() {
	execPath, err := os.Executable()
	if err != nil {
		return
	}
	if realPath, err := filepath.EvalSymlinks(execPath); err == nil && realPath != "" {
		execPath = realPath
	}

	oldPath := execPath + ".old"
	if _, err := os.Stat(oldPath); err == nil {
		_ = os.Remove(oldPath)
	}
}

// Restart 启动新进程并退出当前进程。
func Restart() error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable for restart: %w", err)
	}
	if realPath, err := filepath.EvalSymlinks(execPath); err == nil && realPath != "" {
		execPath = realPath
	}

	cmd := exec.Command(execPath, os.Args[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start new process: %w", err)
	}

	// 留 500ms 缓冲后优雅退出
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()

	return nil
}

func moveOrCopy(src, dst string) error {
	// 首先尝试快速原子重命名
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	// 跨盘/跨挂载点时回退到复制文件
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}
	_ = srcFile.Close()
	_ = os.Remove(src)
	return nil
}
