package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Rehtt/Kit/util/size"
	"github.com/gogf/gf/v2/util/gconv"
)

type LogFile struct {
	mu      sync.RWMutex
	logfile string
	f       *os.File

	size        uint64
	limitSize   uint64
	closed      bool
	compression bool
}

func (l *LogFile) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return 0, fmt.Errorf("log file is closed")
	}

	if l.f == nil {
		if err = l.newFile(); err != nil {
			slog.Error("创建新日志文件失败", "logfile", l.logfile, "error", err)
			return
		}
	}

	// 如果当前写入不会超过限制，直接写入
	if size := uint64(len(p)); l.size+size <= l.limitSize {
		n, err = l.f.Write(p)
		if err == nil {
			l.size += uint64(n)
		}
		return
	}

	// 需要轮转文件，按行分割写入
	for data := range bytes.SplitAfterSeq(p, []byte("\n")) {
		size := uint64(len(data))

		// 如果单行本身超过限制，直接写入，避免死循环
		if size > l.limitSize {
			slog.Warn("单行日志超过大小限制，直接写入", "logfile", l.logfile, "line_size", size, "limit", l.limitSize)
			nn, err := l.f.Write(data)
			if err != nil {
				slog.Error("写入超大日志数据失败", "logfile", l.logfile, "error", err)
				return n, err
			}
			n += nn
			l.size += uint64(nn) // 立即更新大小
			continue
		}

		// 如果写入会超过限制，先轮转
		if l.size+size > l.limitSize {
			if err = l.newFile(); err != nil {
				slog.Error("轮转日志文件失败", "logfile", l.logfile, "error", err)
				return
			}
		}

		nn, err := l.f.Write(data)
		if err != nil {
			slog.Error("写入日志数据失败", "logfile", l.logfile, "error", err)
			return n, err
		}
		n += nn
		l.size += uint64(nn) // 立即更新大小
	}
	return
}

func (l *LogFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return nil
	}

	l.closed = true

	if l.f != nil {
		if err := l.f.Close(); err != nil {
			slog.Error("关闭日志文件失败", "logfile", l.logfile, "error", err)
			return err
		}
		l.f = nil
	}
	return nil
}

func (l *LogFile) newFile() (err error) {
	if l.f != nil {
		if err = l.f.Close(); err != nil {
			slog.Error("关闭当前日志文件失败", "logfile", l.logfile, "error", err)
			return
		}

		// 查找下一个可用的文件索引
		var maxIndex int
		dir := filepath.Dir(l.logfile)
		baseName := filepath.Base(l.logfile)

		if err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}

			// 检查是否是当前日志文件的轮转文件
			if suffix, found := strings.CutPrefix(d.Name(), baseName+"."); found {
				// 如果是压缩文件，先去掉 .gz 后缀
				suffix = strings.TrimSuffix(suffix, ".gz")
				if index := gconv.Int(suffix); index > 0 {
					maxIndex = max(maxIndex, index)
				}
			}
			return nil
		}); err != nil {
			slog.Error("遍历目录失败", "dir", dir, "error", err)
			return fmt.Errorf("walk dir %s error: %v", dir, err)
		}

		// 重命名当前文件
		newName := fmt.Sprintf("%s.%d", l.logfile, maxIndex+1)
		if l.compression {
			// 闭包处理
			if err := func() error {
				newName = newName + ".gz"
				f, err := os.OpenFile(newName, os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					slog.Error("创建新日志文件失败", "logfile", newName, "error", err)
					return fmt.Errorf("create log file %s error: %v", newName, err)
				}
				defer f.Close()
				g := gzip.NewWriter(f)
				defer g.Close()
				oldLog, err := os.Open(l.logfile)
				if err != nil {
					slog.Error("打开日志文件失败", "logfile", l.logfile, "error", err)
					return fmt.Errorf("open log file %s error: %v", l.logfile, err)
				}
				defer oldLog.Close()
				_, err = io.Copy(g, oldLog)
				if err != nil {
					slog.Error("写入日志文件失败", "logfile", l.logfile, "error", err)
					return fmt.Errorf("write log file %s error: %v", l.logfile, err)
				}
				return nil
			}(); err != nil {
				// 压缩失败时清理不完整的压缩文件
				os.Remove(newName)
				return err
			}

			if err = os.Remove(l.logfile); err != nil {
				slog.Error("删除旧日志文件失败", "logfile", l.logfile, "error", err)
				return fmt.Errorf("remove log file %s error: %v", l.logfile, err)
			}
		} else {
			if err = os.Rename(l.logfile, newName); err != nil {
				slog.Error("重命名日志文件失败", "from", l.logfile, "to", newName, "error", err)
				return fmt.Errorf("rename %s to %s error: %v", l.logfile, newName, err)
			}
		}

		slog.Info("日志文件轮转完成", "from", l.logfile, "to", newName)
	}

	// 创建新的日志文件
	l.f, err = os.OpenFile(l.logfile, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o644)
	if err != nil {
		slog.Error("创建新日志文件失败", "logfile", l.logfile, "error", err)
		return fmt.Errorf("create log file %s error: %v", l.logfile, err)
	}

	info, err := l.f.Stat()
	if err != nil {
		slog.Error("获取文件信息失败", "logfile", l.logfile, "error", err)
		return fmt.Errorf("stat log file %s error: %v", l.logfile, err)
	}

	l.size = uint64(info.Size())
	slog.Debug("创建新日志文件", "logfile", l.logfile, "size", l.size)
	return
}

func NewLogFile(serviceName string, limitSize size.ByteSize, compression bool) (*LogFile, error) {
	dir := filepath.Join(logPath, serviceName)
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		slog.Error("创建日志目录失败", "dir", dir, "error", err)
		return nil, fmt.Errorf("create log dir %s error: %v", dir, err)
	}

	logfile := filepath.Join(dir, serviceName+".log")
	l := &LogFile{
		logfile:     logfile,
		limitSize:   uint64(limitSize),
		closed:      false,
		compression: compression,
	}

	slog.Debug("创建日志文件", "service", serviceName, "logfile", logfile, "limit", limitSize)

	if err := l.newFile(); err != nil {
		return nil, err
	}

	return l, nil
}
