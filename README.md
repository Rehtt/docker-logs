# Docker Log Collector

一个用于收集和管理Docker容器日志的Go工具，支持日志轮转和大小限制。

## 功能特性

- 🐳 **Docker容器日志收集**: 实时收集指定容器的stdout和stderr日志
- 📁 **自动日志轮转**: 当日志文件达到指定大小时自动轮转
- 🔄 **断点续传**: 支持从上次中断的位置继续收集日志，避免重复
- 🔁 **容器重启检测**: 自动检测容器重启/重建，立即重新连接
- 🛑 **优雅的资源管理**: 使用Context优雅地取消和清理资源
- 📊 **大小限制**: 可配置日志文件大小限制
- 🎯 **多容器支持**: 同时监控多个容器，独立处理
- 🛡️ **并发安全**: 使用sync.Map和读写锁保证并发安全

## 安装

### 前置要求

- Go 1.25.1 或更高版本
- Docker 环境

### 编译

```bash
git clone https://github.com/Rehtt/docker-logs.git
cd docker-logs
go mod tidy
go build -o docker-logs
```

## 使用方法

### 基本用法

```bash
./docker-logs -container-names=container1,container2 -log-path=/var/log -limit=50MB
```

### 参数说明

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-container-names` | 必填 | 要监控的容器名称，多个容器用逗号分隔 |
| `-log-path` | `/var/log` | 日志文件输出路径 |
| `-limit` | `50MB` | 单个日志文件大小限制 |

### 示例

```bash
# 监控单个容器
./docker-logs -container-names=nginx -log-path=/var/log -limit=100MB

# 监控多个容器
./docker-logs -container-names=nginx,mysql,redis -log-path=/var/log -limit=50MB

# 使用不同的日志路径
./docker-logs -container-names=app -log-path=/home/user/logs -limit=200MB
```

## 日志文件结构

程序会在指定的日志路径下为每个容器创建目录，日志文件按以下结构组织：

```
/var/log/
├── nginx/
│   ├── nginx.log          # 当前日志文件
│   ├── nginx.log.1        # 轮转后的日志文件
│   └── nginx.log.2        # 更早的轮转日志文件
├── mysql/
│   ├── mysql.log
│   └── mysql.log.1
└── redis/
    ├── redis.log
    └── redis.log.1
```

## 日志轮转机制

- 当日志文件大小超过限制时，会自动进行轮转
- 轮转后的文件会添加数字后缀（如 `.1`, `.2` 等）
- 程序会查找已存在的轮转文件，使用下一个可用的数字
- 轮转过程中会按行分割，确保日志完整性

## 断点续传与去重

程序支持智能的断点续传功能：

- 程序启动时会读取日志文件的最后一行
- 解析最后一行的时间戳，并**加上1纳秒**
- 使用新时间戳作为 Docker API 的 `Since` 参数
- **完全避免日志重复**：由于 Docker API 的 `Since` 参数是包含性的（>=），加1纳秒确保只获取新日志
- 如果无法读取最后一行，会从头开始收集
- 即使程序多次重启，也不会产生重复的日志记录

## 容器监控机制

程序采用智能的容器状态监控：

- **定期检查**：每10秒检查一次容器列表
- **状态追踪**：使用 `sync.Map` 记录每个容器的 ID 和取消函数
- **自动检测**：
  - 容器首次启动：立即开始监控
  - 容器重启（ID改变）：取消旧监控，启动新监控
  - 容器停止：优雅地取消监控，清理资源
- **Context 管理**：每个容器监控使用独立的 Context，支持优雅取消
- **资源隔离**：每个容器的日志处理完全独立，互不影响

## 并发安全

- 使用 `sync.Map` 实现线程安全的容器状态管理
- 使用读写锁（`sync.RWMutex`）保证日志文件写入的并发安全
- 每个容器使用独立的 goroutine 处理日志
- Context 机制确保 goroutine 能够及时响应取消信号

## 错误处理

程序包含完善的错误处理机制：

- **Docker API 失败**：调用失败时等待5秒后重试
- **文件操作失败**：记录详细的错误日志，不影响其他容器
- **网络中断**：自动检测并重新连接
- **容器失联**：检测到容器断开后，等待容器重启自动重连
- **Context 取消**：区分正常停止和异常错误，记录相应日志
- **时间戳解析失败**：自动降级使用原时间戳，确保服务可用

## 技术实现

### 核心机制

#### 1. 避免日志重复
```go
// 读取最后一行时间戳
lastLine := "2024-01-01T10:00:00.123456789Z ..."
timestamp := "2024-01-01T10:00:00.123456789Z"

// 加上1纳秒
newTimestamp := "2024-01-01T10:00:00.123456790Z"

// Docker API 只返回 >= newTimestamp 的日志
// 完全避免了已记录的最后一行被重复获取
```

#### 2. 容器状态监控
```go
type containerInfo struct {
    id     string                // 容器ID，用于检测重启
    cancel context.CancelFunc   // 取消函数，优雅停止监控
}

// 每10秒检查一次
for {
    // 检测容器状态变化
    if 容器ID改变 {
        旧监控.cancel()  // 立即取消旧的goroutine
        启动新监控
    }
    time.Sleep(10 * time.Second)
}
```

#### 3. 资源生命周期
```
容器启动 -> 创建Context -> 启动goroutine -> 收集日志
                                             ↓
容器重启 -> 取消Context -> goroutine退出 -> 创建新Context -> 新goroutine
                                             ↓
容器停止 -> 取消Context -> goroutine退出 -> 清理资源
```

### 工作流程

```
[主循环] (每10秒)
   ↓
[检查容器列表]
   ↓
[比对容器ID] ──→ ID相同 ──→ 继续监控
   ↓
   ID不同 ──→ [取消旧监控] ──→ [启动新监控]
   ↓
   容器停止 ──→ [取消监控] ──→ [清理资源]
```

## 开发

### 项目结构

```
docker-logs/
├── main.go          # 主程序入口
├── logfile.go       # 日志文件管理
├── go.mod           # Go模块文件
├── go.sum           # 依赖校验文件
└── README.md        # 项目说明
```

### 依赖

- `github.com/docker/docker` - Docker API客户端
- `github.com/Rehtt/Kit` - 工具库（大小解析等）
- `github.com/gogf/gf/v2` - GoFrame框架工具

### 构建

```bash
# 安装依赖
go mod tidy

# 运行测试
go test ./...

# 构建二进制文件
go build -o docker-logs

# 交叉编译（Linux）
GOOS=linux GOARCH=amd64 go build -o docker-logs-linux
```

## 部署建议

### 作为系统服务

创建systemd服务文件 `/etc/systemd/system/docker-logs.service`：

```ini
[Unit]
Description=Docker Log Collector
After=docker.service
Requires=docker.service

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/docker-logs -container-names=nginx,mysql -log-path=/var/log -limit=100MB
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

启用服务：

```bash
sudo systemctl daemon-reload
sudo systemctl enable docker-logs
sudo systemctl start docker-logs
```

### Docker部署

```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY . .
RUN go mod tidy && go build -o docker-logs

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/docker-logs .
ENTRYPOINT ["./docker-logs"]
```

## 注意事项

1. **权限要求**: 程序需要访问Docker socket，通常需要root权限或docker组权限
2. **磁盘空间**: 确保有足够的磁盘空间存储日志文件
3. **容器状态**: 程序会持续监控容器状态，容器重启后会在10秒内自动重新连接
4. **日志格式**: 收集的日志包含Docker标准时间戳（RFC3339Nano格式）
5. **断点续传**: 程序重启时从日志文件最后一行的时间戳继续，不会重复记录
6. **容器重启**: 容器重启后（容器ID改变），程序会从日志文件的断点继续收集新容器的日志
7. **资源清理**: 容器停止后，相应的监控 goroutine 会被自动取消和清理

## 许可证

MIT License

## 贡献

欢迎提交Issue和Pull Request！

## 更新日志

### v1.1.0 (最新)
- 🔧 **修复日志重复问题**：时间戳加1纳秒，彻底避免断点续传时的日志重复
- 🔁 **增强容器重启检测**：实时检测容器重启/停止，最快10秒内重新连接
- 🛑 **优雅资源管理**：使用 Context 机制优雅地取消和清理 goroutine
- 📊 **改进状态追踪**：使用 `sync.Map` 实现高效的容器状态管理
- 📝 **完善日志输出**：区分容器首次启动、重启、停止等不同状态
- ⚡ **性能优化**：独立的 Context 确保容器监控相互独立，互不影响

### v1.0.0
- 初始版本
- 支持Docker容器日志收集
- 支持日志轮转
- 支持断点续传
- 支持多容器监控
