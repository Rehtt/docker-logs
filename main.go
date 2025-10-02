package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Rehtt/Kit/util/size"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

var (
	logPath        = flag.String("log-path", "/var/log", "out log path")
	logLimitSize   = flag.String("limit", "50MB", "log limit size")
	containerNames = flag.String("container-names", "", "container names. eg: name1,name2")
	compression    = flag.Bool("compression", false, "log file compression")
)

func main() {
	flag.Parse()

	// 初始化日志记录器
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	limit, err := size.ParseFromString(*logLimitSize)
	if err != nil {
		slog.Error("解析日志限制大小失败", "error", err, "limit", *logLimitSize)
		os.Exit(1)
	}

	c, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		slog.Error("创建 Docker 客户端失败", "error", err)
		os.Exit(1)
	}
	defer c.Close()

	names := strings.Split(*containerNames, ",")
	if len(names) == 0 || (len(names) == 1 && names[0] == "") {
		slog.Error("未指定容器名称", "container-names", *containerNames)
		os.Exit(1)
	}

	slog.Info("开始监控容器日志", "containers", names, "log-path", *logPath, "limit", *logLimitSize)

	// 跟踪每个容器的当前 ID 和取消函数，用于检测容器重启
	type containerInfo struct {
		id     string
		cancel context.CancelFunc
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
					containerInfos.Delete(name)
				}
				continue
			}

			// 容器正在运行
			val, exists := containerInfos.Load(name)

			if !exists || val.(*containerInfo).id != newID {
				if exists {
					info := val.(*containerInfo)
					slog.Info("检测到容器重启", "container", name, "old_id", info.id[:12], "new_id", newID[:12])
					info.cancel() // 取消旧的 goroutine
				} else {
					slog.Info("发现新容器，开始监控", "container", name, "id", newID[:12])
				}
				ctx, cancel := context.WithCancel(context.Background())
				containerInfos.Store(name, &containerInfo{id: newID, cancel: cancel})
				go handleWithContext(ctx, name, newID, c, limit)
			}
		}

		time.Sleep(10 * time.Second)
	}
}

func handleWithContext(ctx context.Context, name string, id string, c *client.Client, limit size.ByteSize) {
	slog.Info("开始处理容器日志", "container", name, "id", id[:12])

	logfile, err := NewLogFile(name, limit, *compression)
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

	// 从文件末尾开始向前读
	var line []byte
	buf := make([]byte, 1) // 每次读一个字节
	for offset := int64(1); offset <= size; offset++ {
		_, err := file.ReadAt(buf, size-offset)
		if err != nil {
			return "", err
		}

		if buf[0] == '\n' && offset != 1 {
			// 遇到换行，说明一行结束（忽略最后一个换行符本身）
			break
		}
		line = append([]byte{buf[0]}, line...)
	}

	return string(line), nil
}
