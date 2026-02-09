package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rehtt/Kit/cli"
	"github.com/Rehtt/Kit/util/size"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

var (
	logPath        string
	logLimitSize   string
	containerNames string
	compression    bool
)

func main() {
	// 初始化日志记录器
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	app := cli.NewCLI("", "")
	app.StringVarShortLong(&logPath, "o", "log-path", "/var/log", "out log path")
	app.StringVarShortLong(&logLimitSize, "l", "limit", "50MB", "log limit size")
	app.StringVarShortLong(&containerNames, "n", "container-names", "", "container names. eg: name1,name2")
	app.BoolVarShortLong(&compression, "c", "compression", false, "log file compression")

	app.CommandFunc = func(args []string) error {
		return run()
	}

	app.Run(os.Args[1:])
}

func handleWithContext(ctx context.Context, name string, id string, c *client.Client, limit size.ByteSize) {
	slog.Info("开始处理容器日志", "container", name, "id", id[:12])

	logfile, err := NewLogFile(name, limit, compression)
	if err != nil {
		slog.Error("创建日志文件失败", "container", name, "error", err)
		return
	}
	defer func() {
		if err := logfile.Close(); err != nil {
			slog.Error("关闭日志文件失败", "container", name, "error", err)
		}
	}()

	lastLine, err := ReadLastLine(logfile.f)
	if err != nil {
		slog.Warn("读取最后一行失败，从头开始", "container", name, "error", err)
		lastLine = ""
	}

	var since string
	if lastLine != "" {
		parts := strings.Split(lastLine, " ")
		if len(parts) > 0 {
			timestamp := parts[0]
			// 解析时间戳并加上1纳秒，避免重复读取最后一行
			since, err = addNanosecond(timestamp)
			if err != nil {
				slog.Warn("解析时间戳失败，使用原时间戳", "timestamp", timestamp, "error", err)
				since = timestamp
			}
		}
	}

	slog.Debug("获取容器日志", "container", name, "since", since)

	r, err := c.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Since:      since,
		Timestamps: true,
		Follow:     true,
		Details:    false,
	})
	if err != nil {
		slog.Error("获取容器日志失败", "container", name, "error", err)
		return
	}
	defer func() {
		if err := r.Close(); err != nil {
			slog.Error("关闭日志流失败", "container", name, "error", err)
		}
	}()

	_, err = stdcopy.StdCopy(logfile, logfile, r)

	// 检查是否是因为 context 取消
	if ctx.Err() != nil {
		slog.Info("容器监控被取消", "container", name, "reason", ctx.Err())
		return
	}

	if err != nil {
		slog.Error("复制日志数据失败", "container", name, "error", err)
	}

	slog.Info("容器日志处理完成", "container", name)
}

func run() error {
	limit, err := size.ParseFromString(logLimitSize)
	if err != nil {
		slog.Error("解析日志限制大小失败", "error", err, "limit", logLimitSize)
		return fmt.Errorf("parse limit size %s error: %v", logLimitSize, err)
	}

	c, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		slog.Error("创建 Docker 客户端失败", "error", err)
		return fmt.Errorf("create docker client error: %v", err)
	}
	defer c.Close()

	names := strings.Split(containerNames, ",")
	if len(names) == 0 || (len(names) == 1 && names[0] == "") {
		slog.Error("未指定容器名称", "container-names", containerNames)
		return fmt.Errorf("no container names specified: %s", containerNames)
	}

	slog.Info("开始监控容器日志", "containers", names, "log-path", logPath, "limit", logLimitSize)

	// 跟踪每个容器的当前 ID 和取消函数，用于检测容器重启
	type containerInfo struct {
		id     string
		cancel context.CancelFunc
		wg     *sync.WaitGroup
		run    atomic.Bool
	}
	var containerInfos sync.Map // map[containerName]*containerInfo

	for {
		infos, err := c.ContainerList(context.Background(), container.ListOptions{})
		if err != nil {
			slog.Error("获取容器列表失败", "error", err)
			time.Sleep(5 * time.Second) // 等待后重试
			continue
		}

		// 构建当前运行的容器映射
		runningContainers := make(map[string]string) // map[name]id
		for _, info := range infos {
			for _, cname := range info.Names {
				cname = strings.TrimPrefix(cname, "/")
				if slices.Contains(names, cname) {
					runningContainers[cname] = info.ID
					break
				}
			}
		}

		// 检查需要监控的每个容器
		for _, name := range names {
			newID, isRunning := runningContainers[name]

			if !isRunning {
				// 容器没有运行，如果有旧的监控，取消它
				if val, exists := containerInfos.Load(name); exists {
					info := val.(*containerInfo)
					slog.Info("容器已停止，取消监控", "container", name, "id", info.id[:12])
					info.cancel()
					info.wg.Wait() // 等待 goroutine 完全退出
					containerInfos.Delete(name)
				}
				continue
			}

			// 容器正在运行
			val, exists := containerInfos.Load(name)

			if !exists || val.(*containerInfo).id != newID || !val.(*containerInfo).run.Load() {
				if exists {
					info := val.(*containerInfo)
					if info.id != newID {
						slog.Info("检测到容器重启", "container", name, "old_id", info.id[:12], "new_id", newID[:12])
					}
					info.cancel() // 取消旧的 goroutine
					// 等待旧 goroutine 完全退出，避免资源竞争
					info.wg.Wait()
				} else {
					slog.Info("发现新容器，开始监控", "container", name, "id", newID[:12])
				}
				ctx, cancel := context.WithCancel(context.Background())

				info := &containerInfo{id: newID, cancel: cancel, wg: &sync.WaitGroup{}}
				containerInfos.Store(name, info)
				info.wg.Add(1)
				info.run.Store(true)
				go func(info *containerInfo) {
					defer func() {
						info.wg.Done()
						info.run.Store(false)
					}()

					handleWithContext(ctx, name, newID, c, limit)
				}(info)
			}
		}

		time.Sleep(10 * time.Second)
	}
}

// addNanosecond 给 RFC3339Nano 格式的时间戳加上1纳秒
// 这样可以避免 Docker API 的 Since 参数返回已记录的最后一行日志
func addNanosecond(timestamp string) (string, error) {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "", err
	}
	// 加上1纳秒
	t = t.Add(1 * time.Nanosecond)
	return t.Format(time.RFC3339Nano), nil
}

func ReadLastLine(file *os.File) (string, error) {
	// 获取文件大小
	stat, err := file.Stat()
	if err != nil {
		return "", err
	}

	size := stat.Size()
	if size == 0 {
		return "", nil // 空文件
	}

	// 使用缓冲区读取，每次读取 4KB
	const bufSize = 4096
	// 限制最多读取 1MB，避免单行过大导致内存问题
	const maxReadSize = 1024 * 1024

	var result []byte
	var totalRead int64

	for offset := size; offset > 0 && totalRead < maxReadSize; {
		// 计算本次读取的大小
		readSize := int64(bufSize)
		if offset < readSize {
			readSize = offset
		}

		buf := make([]byte, readSize)
		readPos := offset - readSize

		_, err := file.ReadAt(buf, readPos)
		if err != nil {
			return "", err
		}

		// 在缓冲区中从后向前查找换行符
		for i := len(buf) - 1; i >= 0; i-- {
			// 如果是第一次读取且在文件末尾，跳过文件末尾的换行符
			// 注意：只跳过文件最后一个字符，如果有多个连续换行符，会返回最后一个非空行
			if offset == size && readPos+int64(i) == size-1 && buf[i] == '\n' {
				continue
			}

			if buf[i] == '\n' {
				// 找到换行符，返回之后的内容
				result = append(buf[i+1:], result...)
				return string(result), nil
			}
		}

		// 没找到换行符，将整个缓冲区添加到结果前面
		result = append(buf, result...)
		offset -= readSize
		totalRead += readSize
	}

	// 整个文件（或前1MB）都是一行
	return string(result), nil
}
