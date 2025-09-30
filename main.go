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

	for {
		infos, err := c.ContainerList(context.Background(), container.ListOptions{})
		if err != nil {
			slog.Error("获取容器列表失败", "error", err)
			time.Sleep(5 * time.Second) // 等待后重试
			continue
		}

		var wait sync.WaitGroup
		for _, info := range infos {
			for _, cname := range info.Names {
				cname = strings.TrimPrefix(cname, "/")
				if slices.Contains(names, cname) {
					containerName := cname
					containerID := info.ID
					wait.Go(func() {
						handle(containerName, containerID, c, limit)
					})
					break
				}
			}
		}
		wait.Wait()
		time.Sleep(10 * time.Second)
	}
}

func handle(name string, id string, c *client.Client, limit size.ByteSize) {
	slog.Info("开始处理容器日志", "container", name, "id", id[:12])

	logfile, err := NewLogFile(name, limit)
	if err != nil {
		slog.Error("创建日志文件失败", "container", name, "error", err)
		return
	}
	defer func() {
		if err := logfile.Close(); err != nil {
			slog.Error("关闭日志文件失败", "container", name, "error", err)
		}
	}()

	line, err := ReadLastLine(logfile.f)
	if err != nil {
		slog.Warn("读取最后一行失败，从头开始", "container", name, "error", err)
		line = ""
	}

	var since string
	if line != "" {
		parts := strings.Split(line, " ")
		if len(parts) > 0 {
			since = parts[0]
		}
	}

	slog.Debug("获取容器日志", "container", name, "since", since)

	r, err := c.ContainerLogs(context.Background(), id, container.LogsOptions{
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
	if err != nil {
		slog.Error("复制日志数据失败", "container", name, "error", err)
	}

	slog.Info("容器日志处理完成", "container", name)
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
