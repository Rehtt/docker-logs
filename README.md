# Docker Log Collector

一个用于收集和管理Docker容器日志的Go工具，支持日志轮转和大小限制。

## 功能特性

- 🐳 **Docker容器日志收集**: 实时收集指定容器的stdout和stderr日志
- 📁 **自动日志轮转**: 当日志文件达到指定大小时自动轮转
- 🔄 **断点续传**: 支持从上次中断的位置继续收集日志
- 📊 **大小限制**: 可配置日志文件大小限制
- 🎯 **多容器支持**: 同时监控多个容器
- 🛡️ **并发安全**: 使用读写锁保证并发安全

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

## 断点续传

程序支持断点续传功能：

- 程序启动时会读取日志文件的最后一行
- 从最后一行的时间戳开始继续收集日志
- 如果无法读取最后一行，会从头开始收集

## 并发处理

- 使用 `sync.WaitGroup` 并发处理多个容器
- 每个容器的日志处理是独立的
- 使用读写锁保证日志文件写入的并发安全

## 错误处理

程序包含完善的错误处理机制：

- Docker API 调用失败时会重试
- 文件操作失败时会记录错误日志
- 网络中断时会自动重连

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
3. **容器状态**: 程序会持续监控容器状态，容器重启后会自动重新连接
4. **日志格式**: 收集的日志包含时间戳，格式为Docker标准格式

## 许可证

MIT License

## 贡献

欢迎提交Issue和Pull Request！

## 更新日志

### v1.0.0
- 初始版本
- 支持Docker容器日志收集
- 支持日志轮转
- 支持断点续传
- 支持多容器监控
