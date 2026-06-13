# 部署到 Linux 服务器

以 ice-server 为例（`103.91.219.4`，SSH 端口 `22122`，用户 `iec`）。

## 目录结构

服务统一部署到 `~/deploy`，与 claude-relay-server 保持一致：

```
~/deploy/
  bin/cliproxyapi               # 当前版本软链
  bin/cliproxyapi.20260402-...  # 历史版本（保留最近 10 个）
  etc/cliproxyapi.yaml          # 生产配置
~/CLIProxyAPI/
  auths/                        # OAuth 认证文件（codex-*.json）
  auths/logs/main.log           # 服务日志
  static/management.html        # 管理面板（自动下载）
  server-deploy-all.sh          # 多机统一发布脚本（在控制机执行）
```

## 首次部署

### 1. 在服务器上编译

服务器本身为 Linux amd64，直接编译即可：

```bash
cd ~/CLIProxyAPI
go build -o /tmp/cliproxyapi ./cmd/server/
```

### 2. 部署二进制

```bash
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
cp /tmp/cliproxyapi ~/deploy/bin/cliproxyapi.$TIMESTAMP
ln -sfn cliproxyapi.$TIMESTAMP ~/deploy/bin/cliproxyapi
```

### 3. 生产配置

将 `config.yaml` 复制到 `~/deploy/etc/cliproxyapi.yaml`，修改以下字段：

| 字段 | 本地值 | 服务器值 |
|------|--------|----------|
| `auth-dir` | `"./auths"` | `"/home/iec/CLIProxyAPI/auths"` |
| `host` | `"127.0.0.1"` | `""` （如需对外暴露） |
| `debug` | `true` | `false` |
| `pprof.enable` | `true` | `false` |

### 4. systemd 服务

```bash
sudo tee /etc/systemd/system/cliproxyapi.service > /dev/null <<'EOF'
[Unit]
Description=CLIProxyAPI
After=network.target

[Service]
Type=simple
User=iec
WorkingDirectory=/home/iec/CLIProxyAPI
ExecStart=/home/iec/deploy/bin/cliproxyapi -config /home/iec/deploy/etc/cliproxyapi.yaml
Restart=always
RestartSec=5

# 优雅关闭配置
TimeoutStopSec=35
KillMode=mixed
KillSignal=SIGTERM

StandardOutput=null
StandardError=null

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now cliproxyapi
sudo systemctl status cliproxyapi
```

### 5. Nginx 代理（管理面板）

在 `admin.ieasycode.cc` 的 Nginx 配置中添加：

```nginx
# CLIProxyAPI 管理面板入口（隐藏在安全前缀下）
location = /jiovaonfgiudiaj323u48934tjhnfdigfidbnxibcv/cliproxy/management.html {
    proxy_pass http://127.0.0.1:8317/management.html;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}

# CLIProxyAPI 管理 API（由 secret-key 自身保护）
location ^~ /v0/management/ {
    proxy_pass http://127.0.0.1:8317/v0/management/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

管理面板访问地址：
```
https://admin.ieasycode.cc/jiovaonfgiudiaj323u48934tjhnfdigfidbnxibcv/cliproxy/management.html
```

### 6. 接入 ai-relay 上游

在 clients.db 的 `upstreams` 表中添加：

| 字段 | 值 |
|------|----|
| `name` | `ice` |
| `base_url` | `http://127.0.0.1:8317` |
| `api_key` | 来自 `config.yaml` 的 `api-keys[0]` |

注意：`base_url` 不要带 `/v1` 后缀，ai-relay 转发时会自动拼接请求路径。

## 日常更新

在**控制机**（`cheery-taste`）上执行,统一发布到清单中的所有目标机（含本机 self 与 `ice-server`）：

```bash
cd ~/CLIProxyAPI
git pull
./server-deploy-all.sh                 # dry-run，先看将要做什么
./server-deploy-all.sh --execute       # 真正发布到所有目标机
./server-deploy-all.sh --execute --target ice-server   # 只发某一台
./server-deploy-all.sh --list          # 打印目标机清单
```

脚本会自动完成：控制机编译一次 → 分发到每台目标机 → 版本化部署 → 重启 → 健康检查 → 失败时按机自动回滚 → 清理旧版本（保留 10 个）。新增机器只需在脚本的 `TARGETS` 数组追加一行。

## 常用命令

```bash
# 服务管理
sudo systemctl status cliproxyapi
sudo systemctl restart cliproxyapi

# 查看日志
tail -f ~/CLIProxyAPI/auths/logs/main.log

# 快速回滚
ln -sfn cliproxyapi.<旧版本号> ~/deploy/bin/cliproxyapi
sudo systemctl restart cliproxyapi

# 查看版本列表
ls -lt ~/deploy/bin/cliproxyapi.*
```
