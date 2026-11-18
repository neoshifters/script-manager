# 飞牛 NAS（fnOS）Docker 安装指南 · 脚本管理 script-manager 2.0

本文按工作区里现有的 `Dockerfile`、`docker-compose.yml`、`main.go`、`api.go` 实际内容编写，不额外发明参数。所有路径、端口、环境变量都与现有配置一致；凡是需要你替换的地方，都用 `<尖括号>` 标出。

---

## 0. 怎么在飞牛上执行这些命令（先看这一节）

本文所有 `sudo ...`、`cd ...`、`docker ...` 命令都是 **Linux 命令，要在 NAS 上执行**，不是在 Windows 的 CMD/PowerShell 里执行。Windows 只负责两件事：用浏览器传文件、用 SSH 连到 NAS 敲命令。

两条路，选一条：

- 路线 A（推荐）：在飞牛上打开 SSH，Windows 用 PowerShell 连上去，命令可以复制粘贴。
- 路线 B：完全不开 SSH，用飞牛网页里的「文件管理 + Docker」图形界面点选（见 0.5，功能有取舍）。

### 0.1 在飞牛上打开 SSH

1. 浏览器登录飞牛桌面：`http://<NAS的IP>:5666`（5666 是默认端口，你平时怎么进就怎么进）。
2. 进「系统设置」，找 **「终端与远程」**（有的版本写作「远程访问」「终端」或直接叫「SSH」），把 **「SSH 功能」** 开关打开。
3. 端口保持默认 **22** 即可（如果这里显示的是别的数字，记下来，后面连接要加 `-p 端口号`）。
4. **登录账号用安装飞牛时创建的那个管理员账号，不是 `root`。** 飞牛默认禁止 root 直接远程登录；这个管理员账号在 `sudo` 组里，所以命令前面要加 `sudo`，输的密码就是平时登录飞牛网页的那个密码。

> 个别版本的飞牛自带网页版「终端」应用，能直接在浏览器里敲命令，效果等同 SSH，可以省掉 0.2。没有这个应用就用 SSH。

### 0.2 在 Windows 上连接 NAS

1. 按 `Win + R`，输入 `powershell` 回车（用 PowerShell，不要用 CMD）。
2. 输入连接命令，用户名和 IP 换成你自己的：

```powershell
ssh 你的管理员用户名@<NAS的IP>
```

3. 第一次连接会显示一段指纹和一句 `Are you sure you want to continue connecting (yes/no/[fingerprint])?`，输入 **`yes`** 回车。只问这一次。
4. 提示 `password:` 时输入飞牛管理员密码。**光标不会动、不显示星号，这是正常的**，输完直接回车。
5. 成功后会看到类似提示符，说明已经站在 NAS 的 Linux 里了：

```
<用户名>@fnos:~$
```

6. 用完输入 `exit` 回车断开。断开 SSH 不会影响已经在跑的容器。

Windows 提示 `ssh 不是内部或外部命令`：去「设置 → 系统 → 可选功能 → 添加功能」，勾选 **OpenSSH 客户端** 安装，再重开 PowerShell。

### 0.3 逐行执行的规矩

- **一次粘一条命令**，回车，等提示符重新出现（`<用户名>@fnos:~$`）再粘下一条。一次粘一大段，中间某条失败了你也不好定位。
- 命令里 `#` 后面的中文是注释，**不要一起复制**。例如只复制 `uname -m`，不要复制 `# 确认是 x86_64`。
- 以 `sudo` 开头的命令，第一次会再要一次密码：`[sudo] password for <用户名>:`，同样不显示字符。同一个终端里短时间内不会重复问。
- 反斜杠 `\` 结尾表示下一行是续行，这种整段一起复制。
- 路径里有中文、`@`、`+`、空格时，**双引号要一起复制**，例如 `"/vol2/@team/.../脚本管理"`。
- 命令没有任何输出通常代表成功（Linux 的习惯是「没消息就是好消息」）。报错会打印 `xxx: command not found` / `No such file or directory` 之类的红字或英文提示。
- 想少打几次 sudo：执行 `sudo -i` 进入 root shell（提示符变成 `#`），之后命令可以去掉 `sudo`；干完 `exit` 退出。不熟悉就先别用，按本文原样加 `sudo` 最稳。

### 0.4 把你手头这 5 条命令逐条拆开

| 命令 | 在哪执行 | 预期结果 |
|---|---|---|
| `uname -m` | SSH 里 | 输出 `x86_64`（不是就直接停，见 9.2） |
| `sudo mkdir -p /vol1/1000/docker/script-manager` | SSH 里 | 无输出即成功，可能问一次密码 |
| `sudo mkdir -p "/vol2/路径/脚本管理"` | SSH 里 | 无输出即成功；路径要按你机器上的真实目录改 |
| 用「文件管理」上传源码到 `/vol1/1000/docker/script-manager` | **浏览器网页**，不是命令 | 目标目录里出现 `Dockerfile`、`main.go`、`web/` 等 |
| `cd /vol1/1000/docker/script-manager` | SSH 里 | 提示符前的路径变化；`ls` 能列出刚上传的文件 |
| `sudo docker compose up -d --build` | SSH 里 | 滚动大量构建日志，最后出现 `Container script-manager Started` |

执行完最后一条，用这两条确认：

```bash
sudo docker ps --filter name=script-manager      # 应看到 Up ... (healthy)
ls -l                                            # 应看到 Dockerfile、docker-compose.yml、web 等
```

### 0.5 不想开 SSH：用飞牛图形界面

能完成，但「从源码构建」这一步在图形界面上不如命令行稳。建议组合使用：

1. **建目录**：桌面「文件管理」→ 进 `docker` 目录 → 新建文件夹 `script-manager`；数据目录 `脚本管理` 同样方式新建（或让它第一次启动时自动创建）。
2. **上传文件**：文件管理里点「上传」，选 `Dockerfile`、`docker-compose.yml`、`go.mod`、`main.go`、`api.go`、`tools.go`、`collab.go`、`history.go`、`jianying.go`、`export.go`，`web` 文件夹整个上传。
3. **构建镜像**（可选）：「应用中心 → Docker」→ 左侧「镜像」→ 找「构建镜像 / 从 Dockerfile 构建」，目录选 `/vol1/1000/docker/script-manager`，镜像名填 `script-manager`，标签填 `2.0`，构建参数加 `WITH_FFMPEG=1`。没有这个入口就直接跳到第 4 步用现成镜像/二进制。
4. **创建容器**：「Docker → 容器 → 新增」，按下表填（字段名可能略有差异，意思一样）：

| 界面字段 | 填什么 |
|---|---|
| 容器名称 | `script-manager` |
| 镜像 | `script-manager:2.0` |
| 端口映射 | 本地 `8686` → 容器 `8686`，协议 TCP |
| 存储/路径映射 | 宿主机 `/vol2/路径/脚本管理` → 容器 `/data`，权限选**读写** |
| 环境变量 | `SM_ADDR=:8686`、`SM_DATA=/data`、`TZ=Asia/Shanghai` |
| 重启策略 | `always`（或「总是重启」） |
| 网络 | 默认 bridge 即可 |

5. **用 Compose 项目**：Docker 左侧「项目」→「新增」→ 名称 `script-manager` → 粘贴第 10.5 节那份最小 compose（只有 `image`，没有 `build`）→ 启动。这条最适合「镜像已经有了」的情况。
6. **看日志**：容器列表里点 `script-manager` → 「日志」，能看到 `脚本管理 已启动` 和 `ffmpeg:` 一行，效果同 `docker logs`。

图形界面做不了的：批量修文件、`chown` 改属主、`docker exec` 进容器排查。这些仍需 SSH，或改用第 4 章的二进制方案绕开。

### 0.6 常见报错对照

| 报错 | 原因 | 处理 |
|---|---|---|
| `ssh: connect to host ... port 22: Connection refused` | SSH 没开，或端口不是 22 | 回 0.1 打开「SSH 功能」；端口不是 22 就 `ssh -p 端口 用户名@IP` |
| `Connection timed out` | IP 写错、不在同一网段、NAS 防火墙拦截 | 先用浏览器打开 `http://IP:5666` 验证 IP 正确 |
| `Permission denied (publickey,password).` | 用户名或密码错；或用了 `root` | 改用管理员账号，注意大小写；密码连错会直接断开，重新 `ssh` |
| `ssh: command not found` / 不是内部或外部命令 | Windows 没装 OpenSSH 客户端 | 设置 → 可选功能 → 添加 OpenSSH 客户端 |
| `docker: command not found` | Docker 应用没装/没启动；或当前 PATH 找不到 | 应用中心安装并启动 Docker；用 `sudo docker ...`；`which docker` 确认路径 |
| `permission denied while trying to connect to the Docker daemon socket` | 当前用户不在 docker 组，权限不够 | 命令前统一加 `sudo`；长期解决：`sudo usermod -aG docker $USER` 后重新登录 SSH |
| `Cannot connect to the Docker daemon ... Is the docker daemon running?` | Docker 服务没起来 | 应用中心里启动 Docker，或 `sudo systemctl start docker` |
| `cd: /vol1/...: No such file or directory` | 路径不存在、存储池编号不对、大小写不符 | `ls /vol1` 看真实目录名，复制粘贴不要手敲 |
| `sudo: command not found` | 用错了终端（在 Windows 里执行） | 确认提示符是 `用户名@fnos`，不是在 PowerShell 里 |
| `failed to resolve source metadata ... 401 Unauthorized`（地址里带 `docker.fnas.com`） | 飞牛默认镜像加速源失效/需鉴权 | 不是 Dockerfile 问题，见 0.8 |
| `bind: address already in use` | 8686 被占用 | 改 compose 端口左侧，见 9.6 |
| 中文显示成乱码 | 终端编码 | 执行 `export LANG=C.UTF-8`，PowerShell 里先 `chcp 65001` |

### 0.7 关键参数速查（以现有配置文件为准）

| 项目 | 值 | 来源 |
|---|---|---|
| 容器内监听端口 | `8686`（`SM_ADDR=:8686`） | `Dockerfile`、`docker-compose.yml`、`main.go` |
| 宿主机映射端口 | `8686`（可改左侧） | `docker-compose.yml`：`"8686:8686"` |
| 镜像名 / 标签 | `script-manager:2.0` | `docker-compose.yml` |
| 容器名 | `script-manager` | `docker-compose.yml` |
| 数据目录（容器内） | `/data`（`SM_DATA=/data`） | `Dockerfile`、`docker-compose.yml` |
| 数据目录（NAS 上） | `/vol2/路径/脚本管理` | `docker-compose.yml` |
| 时区 | `Asia/Shanghai`（镜像内已装 tzdata） | `Dockerfile`、compose |
| ffmpeg | 构建参数 `WITH_FFMPEG: "1"`，镜像自带（约 +80MB） | compose 第 8 行 |
| 健康检查 | `GET /api/health` → `{"ok":true}` | `main.go`、`Dockerfile` |
| 实时协作 | WebSocket `GET /api/collab/ws`（被拦时自动降级 HTTP 轮询） | `main.go` |
| 单文件上传上限 | 4GB（`maxUpload = 4<<30`） | `api.go` |
| 静态资源 | 已通过 `go:embed` 打进二进制，**不需要** Node、不需要额外挂 `web/` | `main.go` |

> 结论：这是一个单文件 Go 服务，容器里只需要一个可执行文件 + 一个可写的数据卷。**没有数据库、没有外部依赖**，备份就是复制数据目录。

### 0.8 构建失败：registry 401 与镜像源失效

报错原文：

```
failed to solve: golang:1.23-alpine: failed to resolve source metadata for docker.io/library/golang:1.23-alpine:
unexpected status from HEAD request to https://docker.fnas.com/v2/library/golang/manifests/1.23-alpine?ns=docker.io: 401 Unauthorized
```

**先看懂它，不是你的 Dockerfile 写错了：**

- `docker.fnas.com` 是飞牛的默认镜像加速站，`ns=docker.io` 表示它替你去 Docker Hub 取镜像。
- `401 Unauthorized` 是这个加速站返回的，意思是它现在不再允许匿名拉取（或服务已停用/限流）。你的网络、架构、`Dockerfile` 都没问题。
- 日志里先后出现 `alpine:3.20` 和 `golang:1.23-alpine`，最终断在第 6 行 `FROM golang:1.23-alpine AS build`，说明**两个基础镜像都没拉到**，构建在第一步就失败了。
- 因为镜像没构建出来、容器自然没创建，所以 `docker ps --filter name=script-manager` 是空的，这是预期结果，不是容器挂了。

修法有四种，从「改配置」到「绕开编译」，任选一种。

#### 方案 A：换掉失效的加速源（一次配置，长期有效）

**图形界面**：应用中心 → Docker → 右上角「设置」（齿轮）→「镜像仓库 / Registry mirrors」→ 删掉 `docker.fnas.com`，加入下面几个 → 保存（界面通常会自动重启 Docker）。

**命令行**（效果一样）：

```bash
sudo cp /etc/docker/daemon.json /etc/docker/daemon.json.bak 2>/dev/null
sudo tee /etc/docker/daemon.json >/dev/null <<'EOF'
{
  "registry-mirrors": [
    "https://docker.1ms.run",
    "https://docker.xuanyuan.me",
    "https://docker.m.daocloud.io",
    "https://dockerproxy.net"
  ]
}
EOF
```

改完先验 JSON 合法（写错会导致 Docker 起不来，所有容器一起挂），再重启：

```bash
sudo python3 -m json.tool /etc/docker/daemon.json >/dev/null && echo "JSON OK"
sudo systemctl restart docker
sudo docker info | grep -A4 "Registry Mirrors"
```

如果 `docker info` 报错或 Docker 没起来，看日志并回滚备份：

```bash
sudo journalctl -u docker -n 50 --no-pager
sudo cp /etc/docker/daemon.json.bak /etc/docker/daemon.json && sudo systemctl restart docker
```

加速站会失效，用下面这段先探活，`200` 的优先用（`401` 表示仍需鉴权、`000`/超时表示不可达）：

```bash
for m in docker.1ms.run docker.xuanyuan.me docker.m.daocloud.io dockerproxy.net; do
  printf '%-28s ' "$m"
  curl -s -o /dev/null -w '%{http_code}\n' --max-time 8 "https://$m/v2/library/alpine/manifests/3.20"
done
```

> `systemctl restart docker` 会重启 NAS 上所有容器，尽量挑没人用的时候做。

#### 方案 B：不改配置，直接按镜像站全名拉取再 `docker tag` 回原名（最快）

`FROM golang:1.23-alpine` 会优先使用本地已有的同名镜像，所以只要先把镜像拉下来、改回标准标签，构建就不再依赖加速源：

```bash
sudo docker pull docker.1ms.run/library/golang:1.23-alpine
sudo docker tag  docker.1ms.run/library/golang:1.23-alpine golang:1.23-alpine

sudo docker pull docker.1ms.run/library/alpine:3.20
sudo docker tag  docker.1ms.run/library/alpine:3.20 alpine:3.20

sudo docker images | grep -E 'golang|alpine'
cd /vol1/1000/docker/script-manager
sudo docker compose up -d --build
```

- `library/` 是官方镜像的命名空间，不能省；某些镜像站不要求，报 `not found` 时去掉 `library/` 再试。
- 这条命令绕过了 `daemon.json` 里的 mirror，镜像站能通就行，`docker.fnas.com` 失效不影响。
- 若这个站也 401，把域名换成方案 A 里探测到 `200` 的任意一个。

#### 方案 C：跳过 Go 编译，直接用现成二进制（最稳，推荐用它兜底）

工作区里的 `script-manager-linux-amd64` 是编好的单文件，**构建时只需要 `alpine:3.20` 一个基础镜像，不需要 golang，也不需要联网编译**。完整步骤见第 4.1 节，最短版本：

```bash
cd /vol1/1000/docker/script-manager
mkdir -p sm-bin
cp script-manager-linux-amd64 sm-bin/script-manager
chmod +x sm-bin/script-manager
sudo docker build -f Dockerfile.bin -t script-manager:2.0 .
sudo docker compose up -d
```

> 前提是已经按第 4.1 节第 2 步生成过 `Dockerfile.bin`；没生成就先建那个文件，几行内容而已。

**如果连 `alpine:3.20` 也拉不到**，按顺序试：

1. 换镜像站拉取后改标签（只需 alpine 一个）：

```bash
sudo docker pull docker.xuanyuan.me/library/alpine:3.20
sudo docker tag  docker.xuanyuan.me/library/alpine:3.20 alpine:3.20
```

2. 复用 NAS 上已有的基础镜像（飞牛其他应用通常带 alpine/busybox）：

```bash
sudo docker images | grep -Ei 'alpine|busybox'
# 例如看到 alpine:3.19，就把 Dockerfile.bin 第一行改成 FROM alpine:3.19 再 build
```

3. 从你的 Windows 电脑导出基础镜像再导入：

```powershell
# Windows 上（装了 Docker Desktop）
docker pull alpine:3.20
docker save alpine:3.20 | gzip > alpine320.tar.gz
scp alpine320.tar.gz 用户名@NAS的IP:/tmp/
```

```bash
# NAS 上
sudo docker load < /tmp/alpine320.tar.gz
```

4. 最后手段：完全不用容器，直接跑这个静态二进制（飞牛本身就是 Linux x86_64）：

```bash
cd /vol1/1000/docker/script-manager
chmod +x script-manager-linux-amd64
sudo nohup ./script-manager-linux-amd64 -addr :8686 -data "/vol2/路径/脚本管理" > /tmp/script-manager.log 2>&1 &
curl -s http://127.0.0.1:8686/api/health
```

> 这条不受 Docker 影响，但没有自动重启、日志在 `/tmp`，且要自己装 ffmpeg（或用 `SM_FFMPEG` 指向静态版），只适合临时验证。

#### 方案 D：把整个镜像从别处搬过来

如果只是「先把镜像弄到本地」，不限于基础镜像，也可以在任意能正常拉取的机器上构建好，再整体搬运：

```bash
# 能构建的机器上
docker build --build-arg WITH_FFMPEG=1 -t script-manager:2.0 .
docker save script-manager:2.0 | gzip > script-manager-2.0.tar.gz
```

```bash
# NAS 上
sudo docker load < script-manager-2.0.tar.gz
cd /vol1/1000/docker/script-manager
sudo docker compose up -d          # 不要加 --build
```

（架构必须同为 amd64，见 9.2。）

#### 清理与重试

构建失败会留下半成品缓存，重试前清一下（不删业务数据）：

```bash
cd /vol1/1000/docker/script-manager

sudo docker compose down          # 清掉可能残留的容器（数据目录不动）
sudo docker builder prune -f      # 清构建缓存
sudo docker system df             # 看镜像/缓存/卷占了多少空间

sudo docker compose up -d --build # 重新构建
```

想连没被引用的镜像一起清（会删掉其他项目未使用的镜像，谨慎）：

```bash
sudo docker system prune -f
```

成功后确认：

```bash
sudo docker ps --filter name=script-manager --format '{{.Names}} {{.Status}}'
sudo docker exec script-manager wget -qO- http://127.0.0.1:8686/api/health
```

#### 现在就照着做（最短序列）

```bash
# 1) 绕开失效的 docker.fnas.com，直接按镜像站全名拉两个基础镜像
sudo docker pull docker.1ms.run/library/golang:1.23-alpine
sudo docker tag  docker.1ms.run/library/golang:1.23-alpine golang:1.23-alpine
sudo docker pull docker.1ms.run/library/alpine:3.20
sudo docker tag  docker.1ms.run/library/alpine:3.20 alpine:3.20

# 2) 清缓存后重新构建
cd /vol1/1000/docker/script-manager
sudo docker builder prune -f
sudo docker compose up -d --build

# 3) 确认
sudo docker ps --filter name=script-manager
```

如果第 1 步拉取仍然 401，直接跳到方案 C（二进制 + `Dockerfile.bin`），那条路只需要 alpine，一条 `docker build` 几十秒就能完成。

---

## 1. 开工前确认三件事

按第 0.1、0.2 节连上 NAS（浏览器打开 `http://<NAS的IP>:5666` → 系统设置 → 终端与远程 → 打开 SSH 功能 → Windows PowerShell 里 `ssh 用户名@NAS的IP`），然后逐条执行：

```bash
# 1) 架构必须是 x86_64（飞牛 fnOS 官方只支持 x86_64）
uname -m

# 2) Docker 与 compose 插件版本
docker version --format '{{.Server.Version}}'
docker compose version

# 3) 存储池与共享目录（确认 /vol1、/vol2 哪个存在）
ls -d /vol1 /vol2 2>/dev/null
df -h | grep -E '^/dev|vol1|vol2'
```

判断标准：

- `uname -m` 输出 `x86_64` → 直接用现有 `script-manager-linux-amd64`，无需重编。
- 若输出 `aarch64` / `arm64`（非官方机器）→ 必须重编 arm64 版本，见第 4.4 节。
- 如果提示 `docker compose: command not found`，把下文所有 `docker compose` 换成 `docker-compose`，或在飞牛「应用中心 → Docker」里升级到自带 compose v2 的版本。

**端口占用检查**（8686 被别的应用占了就换宿主端口）：

```bash
sudo ss -ltnp | grep 8686 || echo "8686 空闲"
```

---

## 2. 把文件放到 NAS 上

### 2.1 先建目录（推荐放 Docker 数据盘，不要放团队共享盘）

飞牛约定：Docker 相关放 `/vol1/1000/docker/<项目名>/`（`1000` 是首个管理员用户 ID，你的机器可能是别的数字，用 `ls /vol1` 确认）。

```bash
sudo mkdir -p /vol1/1000/docker/script-manager
sudo chown -R "$(id -u):$(id -g)" /vol1/1000/docker/script-manager
```

### 2.2 上传文件（两种方式任选）

**方式一：飞牛「文件管理」网页上传（最省事）**

1. 打开飞牛桌面 →「文件管理」→ 进入 `docker/script-manager`。
2. 上传以下文件（源码构建必需，缺一不可）：

```
Dockerfile
docker-compose.yml
go.mod
main.go
api.go
tools.go
collab.go
web/            ← 整个文件夹（内含 index.html、app.js、collab.js、styles.css）
```

> 注意：`collab.go`、`tools.go` 虽然不在 `部署说明.md` 的清单里，但 `Dockerfile` 的 `COPY main.go api.go tools.go collab.go ./` 要求它们必须存在，少一个构建就会失败。

**方式二：从 Windows 用 scp 上传（保结构，适合文件夹）**

在 Windows PowerShell 里执行（把 IP 和用户名换成你的）：

```powershell
scp -r "v:\traecode\文案编写素材管控\2026-11-17\script-manager\outputs\Dockerfile" `
       "v:\traecode\文案编写素材管控\2026-11-17\script-manager\outputs\docker-compose.yml" `
       "v:\traecode\文案编写素材管控\2026-11-17\script-manager\outputs\go.mod" `
       "v:\traecode\文案编写素材管控\2026-11-17\script-manager\outputs\main.go" `
       "v:\traecode\文案编写素材管控\2026-11-17\script-manager\outputs\api.go" `
       "v:\traecode\文案编写素材管控\2026-11-17\script-manager\outputs\tools.go" `
       "v:\traecode\文案编写素材管控\2026-11-17\script-manager\outputs\collab.go" `
       <用户名>@<NAS的IP>:/vol1/1000/docker/script-manager/

scp -r "v:\traecode\文案编写素材管控\2026-11-17\script-manager\outputs\web" `
       <用户名>@<NAS的IP>:/vol1/1000/docker/script-manager/
```

上传后核对：

```bash
cd /vol1/1000/docker/script-manager
ls -l
ls web/
```

必须能看到 `main.go api.go tools.go collab.go history.go jianying.go export.go go.mod Dockerfile docker-compose.yml` 和 `web/` 里的 4 个前端文件。

---

## 3. 方案一：在 NAS 上从源码构建镜像再启动（推荐首次安装）

### 3.1 改数据目录和端口

编辑 compose：

```bash
cd /vol1/1000/docker/script-manager
nano docker-compose.yml
```

现有内容里只有两处常改：

```yaml
    ports:
      - "8686:8686"          # 左边是 NAS 端口，被占用就改成 "8687:8686"
    volumes:
      # 你机器上的真实业务目录（必须绝对路径 + 引号；含中文、+、空格都要引号）
      - "/vol2/路径/脚本管理:/data"
```

如果要换数据目录，**先在 NAS 上建好并授权**：

```bash
sudo mkdir -p "/vol2/路径/脚本管理"
sudo chown -R 1000:1000 "/vol2/路径/脚本管理"
ls -ld "/vol2/路径/脚本管理"
```

> 为什么用引号：这个路径同时含中文、`@`、`+`，YAML 里不加引号解析会出问题，shell 里不加引号 `+` 会当字面量但也容易被误读，统一加双引号最稳。

### 3.2 构建并启动

```bash
cd /vol1/1000/docker/script-manager
sudo docker compose up -d --build
```

- `WITH_FFMPEG: "1"` 已在 compose 里，构建时会 `apk add ffmpeg`，镜像约 90MB，**首次构建要联网拉 `golang:1.23-alpine` 和 `alpine:3.20`**。
- 想省 80MB、不要 ffmpeg：把 compose 第 8 行改成 `WITH_FFMPEG: "0"`，再 `sudo docker compose up -d --build`。
- 构建慢或卡在拉基础镜像，看第 9.1 节镜像加速。

构建成功后确认：

```bash
sudo docker images | grep script-manager
sudo docker ps | grep script-manager
```

### 3.3 不构建、只跑（compose 已有镜像时）

如果 `script-manager:2.0` 这个镜像已经存在（比如你从别的机器 `docker load` 过来），**不要加 `--build`**，直接：

```bash
sudo docker compose up -d
```

compose 只有在镜像不存在时才会构建，所以这条命令会复用已有镜像，秒起。

---

## 4. 方案二：用已有的二进制或已构建的镜像

### 4.1 情况 A：已有 `script-manager-linux-amd64` 二进制（最快）

工作区里已有一个 5,968,024 字节的 Linux amd64 二进制（按 `Dockerfile` 的 `CGO_ENABLED=0` 方式构建，正常可在 alpine 上直接运行；若启动报缺少动态库，改用第 3 章的源码构建）。

**关键坑**：`Dockerfile` 同目录的 `.dockerignore` 第 2 行写了 `script-manager-linux-amd64`，同名文件会被排除在构建上下文之外。所以要改个名字再 COPY。

第 1 步：上传二进制并改名（在 NAS 上执行）：

```bash
cd /vol1/1000/docker/script-manager
mkdir -p sm-bin
# 把 script-manager-linux-amd64 用文件管理上传到本目录后：
cp script-manager-linux-amd64 sm-bin/script-manager
chmod +x sm-bin/script-manager
```

第 2 步：新建一个只打包二进制的 Dockerfile（带 ffmpeg）：

```bash
cat > Dockerfile.bin <<'EOF'
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata ffmpeg
WORKDIR /app
COPY sm-bin/script-manager /app/script-manager
RUN chmod +x /app/script-manager
ENV SM_ADDR=:8686 \
    SM_DATA=/data \
    TZ=Asia/Shanghai
EXPOSE 8686
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8686/api/health || exit 1
ENTRYPOINT ["/app/script-manager"]
EOF
```

第 3 步：构建成本地镜像（几秒钟，不编译 Go）：

```bash
cd /vol1/1000/docker/script-manager
sudo docker build -f Dockerfile.bin -t script-manager:2.0 .
```

第 4 步：用 compose 启动（镜像名一致，compose 不会重建）：

```bash
sudo docker compose up -d
```

或者完全不用 compose，直接 `docker run`：

```bash
sudo docker run -d \
  --name script-manager \
  --restart always \
  -p 8686:8686 \
  -e TZ=Asia/Shanghai \
  -e SM_ADDR=:8686 \
  -e SM_DATA=/data \
  -v "/vol2/路径/脚本管理:/data" \
  script-manager:2.0
```

### 4.2 情况 B：已有构建好的镜像（另一台机器导出）

**在已有镜像的机器上导出**：

```bash
docker save script-manager:2.0 | gzip > script-manager-2.0.tar.gz
```

把 `script-manager-2.0.tar.gz` 上传到 NAS 的 `/vol1/1000/docker/script-manager/`，然后：

```bash
cd /vol1/1000/docker/script-manager
sudo docker load < script-manager-2.0.tar.gz
sudo docker images | grep script-manager      # 应看到 script-manager:2.0
sudo docker compose up -d                     # 注意：不要加 --build
```

> 若 `docker load` 出来的名字不是 `script-manager:2.0`，先改标签再启动：
> ```bash
> sudo docker tag <实际镜像名> script-manager:2.0
> ```
> 跨架构导出必看：在 x86_64 机器上做的镜像搬到 x86_64 NAS 没问题；ARM 机器导出的镜像在飞牛上会报 `exec format error`。

### 4.3 情况 C：只想跑二进制、不想建镜像（无 ffmpeg）

把二进制直接挂进 alpine 容器，省掉 build：

```bash
sudo docker run -d \
  --name script-manager \
  --restart always \
  -p 8686:8686 \
  -e TZ=Asia/Shanghai \
  -e SM_ADDR=:8686 \
  -e SM_DATA=/data \
  -v "/vol2/路径/脚本管理:/data" \
  -v /vol1/1000/docker/script-manager/script-manager-linux-amd64:/app/script-manager:ro \
  --entrypoint /app/script-manager \
  alpine:3.20
```

此时镜像里**没有 ffmpeg**，智能裁切不可用。补装（容器重建后需重装）：

```bash
sudo docker exec -it script-manager apk add --no-cache ffmpeg
```

### 4.4 没有 amd64 二进制、需要自己编

在任意有 Go 1.23 的机器上交叉编译（Windows PowerShell）：

```powershell
cd "v:\traecode\文案编写素材管控\2026-11-17\script-manager\outputs"
$env:CGO_ENABLED=0
$env:GOOS="linux"
$env:GOARCH="amd64"
go build -trimpath -ldflags="-s -w" -o script-manager-linux-amd64 .
```

Linux / macOS：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o script-manager-linux-amd64 .
```

ARM 设备把 `GOARCH` 换成 `arm64`。编出来的产物按 4.1 的流程打包。

---

## 5. 端口映射与访问地址

| 场景 | 地址 |
|---|---|
| NAS 本机 | `http://127.0.0.1:8686` |
| 局域网其他电脑 | `http://<NAS的IP>:8686` |
| 改了宿主端口（如 `8787:8686`） | `http://<NAS的IP>:8787` |

容器内部端口 `8686` 由 `SM_ADDR=:8686` 决定，**改端口只改 compose 冒号左边**：

```yaml
ports:
  - "8787:8686"
```

只想让本机访问（不给局域网用）：`ports: - "127.0.0.1:8686:8686"`。

查看 NAS 实际 IP 与监听状态：

```bash
ip -4 addr show | grep 'inet '
sudo ss -ltnp | grep 8686
```

> 安全提醒：程序**没有登录和权限控制**（`部署说明.md` 第六节已写明），只适合放在可信内网。不要把 8686 直接映射到公网，也不要在路由器上做端口转发。需要外网用，走飞牛的 VPN/内网穿透并加一层认证。

---

## 6. 数据持久化与权限

### 6.1 数据存在哪

所有业务数据都在宿主机映射目录里，容器删了也不丢：

```
脚本管理/
├── 产品A/脚本一/文案.json       # 物化导出（表格 + 剪辑面板状态）
├── 产品A/脚本一/协同.json       # 协作权威状态，不要手改
├── 产品A/脚本一/1.mp4           # 按镜头序号切片
├── _剪辑素材/                   # 拖进来的原片（临时区，不会自动清理）
├── bin/                         # 可选：手放的 ffmpeg / ffprobe
└── 设置.json                    # 页面里手填 ffmpeg 路径后才会生成
```

上传时的临时文件也写在 `/data` 内（`/data/_剪辑素材/.upload-*`），再 `rename` 落盘，所以**同一块盘上不会跨设备失败**，但要留足空间：4K 素材 + 切片会占不少。

### 6.2 权限

- 容器默认以 **root** 运行，写出来的文件属主是 `root:root`。
- 飞牛「文件管理」用普通用户操作时可能删不掉这些文件，遇到就：

```bash
sudo chown -R 1000:1000 "/vol2/路径/脚本管理"
```

- 目录必须可写，否则启动日志会报 `无法创建数据目录 /data`：

```bash
sudo mkdir -p "/vol2/路径/脚本管理"
sudo chmod 775 "/vol2/路径/脚本管理"
sudo docker restart script-manager
```

- 数据目录是**团队文件夹（`@team`）**时要留意：团队目录的 ACL 可能只对特定用户组开放，Docker 以 root 访问一般没问题，但如果启动后 `/data` 建不出文件，先换到普通共享目录（如 `/vol1/1000/docker/script-manager/data`）验证，再切回团队目录。
- 不要给数据目录设 `:ro`（只读）。

---

## 7. 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `SM_ADDR` | `:8686` | 容器内监听地址。改了必须同步改 compose 右侧端口 |
| `SM_DATA` | `/data` | 数据目录。改了要同步改 volume 右侧挂载点 |
| `TZ` | `Asia/Shanghai` | 容器时区（镜像已装 tzdata，直接生效） |
| `SM_FFMPEG` | 空 | 指定 ffmpeg 路径，如 `/data/bin/ffmpeg` |
| `SM_FFPROBE` | 空 | 指定 ffprobe 路径，如 `/data/bin/ffprobe` |
| `SM_NO_OPEN` | 空 | 非空时启动不自动开浏览器。Linux 下本来就不会开，容器里可不设 |
| `SM_DRAFT_ROOT` | `/vol2/1001/备份/草稿/JianyingPro Drafts` | 剪映草稿根目录（容器内路径）。compose 里已设为 `/drafts/JianyingPro Drafts` |
| `SM_DRAFT_NAS` | 空 | NAS 路径前缀，如 `/drafts`；通常在页面「设置」里填，不必写进 compose |
| `SM_DRAFT_WIN` | 空 | Windows 路径前缀，如 `Z:`；同上，建议在页面里填 |

ffmpeg 查找顺序（`tools.go`）：页面设置里的路径 → `SM_FFMPEG`/`SM_FFPROBE` → 程序同目录及其 `bin/` → 数据目录及其 `bin/` → 系统 `PATH` → 常见安装位置。

镜像自带 ffmpeg 时，compose 里**不需要**再设 `SM_FFMPEG`；想换成自己放进数据目录的静态版，再加：

```yaml
    environment:
      SM_ADDR: ":8686"
      SM_DATA: "/data"
      TZ: "Asia/Shanghai"
      SM_FFMPEG: "/data/bin/ffmpeg"
      SM_FFPROBE: "/data/bin/ffprobe"
```

改完 compose 后统一执行：

```bash
sudo docker compose up -d
```

### 7.1 剪映草稿导出：新增挂载

导出剪映草稿需要把 NAS 上的草稿目录**读写**挂进容器，compose 里已加：

```yaml
    environment:
      SM_DRAFT_ROOT: "/drafts/JianyingPro Drafts"
    volumes:
      - "/vol2/路径/脚本管理:/data"
      - "/vol2/1001/备份/草稿:/drafts"     # 新增，读写，不要加 :ro
```

改了 `volumes` 必须重建容器才生效：

```bash
sudo docker compose up -d --force-recreate
```

然后在 Windows 上把 `/vol2/1001/备份/草稿` 映射成盘符（例如 `Z:`），并在页面「设置 → 剪映草稿导出」填三项：

- 草稿根目录（容器内路径）：`/drafts/JianyingPro Drafts`
- NAS 路径前缀：`/drafts`
- Windows 路径前缀：`Z:`

三项必须能对上，否则导出会提示「不在映射前缀之下」，不会生成打不开的草稿。首次导出前，请先在该目录里放一份自己在剪映里保存过的草稿作模板；导出完成后打开剪映草稿页点刷新即可看到「产品名-脚本名」草稿。

---

## 8. 首次启动后的验证（逐条过一遍）

### 8.1 容器状态

```bash
sudo docker ps --filter name=script-manager
```

应看到 `Up ... (healthy)`。`STATUS` 里出现 `unhealthy` 或 `Restarting` 就先看日志。

### 8.2 健康接口（容器内自检，不经过网络）

```bash
sudo docker exec script-manager wget -qO- http://127.0.0.1:8686/api/health
# 期望输出：{"ok":true}
```

宿主机侧：

```bash
curl -s http://127.0.0.1:8686/api/health
```

### 8.3 启动日志

```bash
sudo docker logs --tail 30 script-manager
```

正常应看到类似：

```
脚本管理 已启动
  本机访问: http://127.0.0.1:8686
  局域网:   http://192.168.x.x:8686
  数据目录: /data
  ffmpeg:   /usr/bin/ffmpeg
```

`ffmpeg` 显示「未找到」说明用了精简镜像且没装 ffmpeg，见第 9.5 节。

### 8.4 时区与写入权限

```bash
sudo docker exec script-manager date
# 应显示 CST +0800，而不是 UTC

sudo docker exec script-manager sh -c 'touch /data/.wtest && ls -l /data/.wtest && rm /data/.wtest'
```

### 8.5 页面与功能

1. 浏览器打开 `http://<NAS的IP>:8686`，页面能加载。
2. 右上角显示 **「ffmpeg 就绪」**（镜像带 ffmpeg 时必然就绪）。
3. 新建一个产品、一个脚本，随便填两行镜头内容，页面右上角出现 **「已保存 HH:MM」**。
4. 回到 NAS 检查落盘：

```bash
ls -lR "/vol2/路径/脚本管理" | head -30
# 应出现 <产品>/<脚本>/文案.json 与 协同.json
```

5. 多人协作：用两台电脑打开同一脚本，两边右上角都显示「协作已连接」，并出现两个彩色头像；A 改「镜头内容」、B 改「口播」，双方 1 秒内互相可见。
6. 视频链路：右侧面板拖入一个 mp4 → 点「智能裁切」→ 有进度百分比 → 片段拖进「视频片段」格 → 单元格先显示「切片中…」→ 变成可播放卡片，脚本目录里出现 `1.mp4`。
7. 重启不丢数据：

```bash
sudo docker restart script-manager
# 等 5 秒后刷新页面，内容与剪辑面板状态都还在
```

### 8.6 一键验收脚本

```bash
echo "--- 容器 ---"; sudo docker ps --filter name=script-manager --format '{{.Status}}'
echo "--- 健康 ---"; sudo docker exec script-manager wget -qO- http://127.0.0.1:8686/api/health; echo
echo "--- 时区 ---"; sudo docker exec script-manager date
echo "--- ffmpeg ---"; sudo docker exec script-manager sh -c 'ffmpeg -version | head -1'
echo "--- 数据 ---"; ls -l "/vol2/路径/脚本管理"
```

---

## 9. 飞牛环境下常见的坑

### 9.1 拉基础镜像慢 / 构建卡住（最高频）

`Dockerfile` 有两个阶段，需要拉 `golang:1.23-alpine` 和 `alpine:3.20`，在国内直连经常几分钟不动。

**办法一：给飞牛 Docker 配镜像加速**

飞牛「应用中心 → Docker → 设置 → 镜像仓库 / Registry mirrors」里添加加速地址，或在 SSH 里改：

```bash
sudo mkdir -p /etc/docker
sudo tee /etc/docker/daemon.json >/dev/null <<'EOF'
{
  "registry-mirrors": [
    "https://docker.1ms.run",
    "https://dockerproxy.net",
    "https://hub.rat.dev"
  ]
}
EOF
sudo systemctl restart docker
sudo docker info | grep -A3 "Registry Mirrors"
```

> 加速站会变动，若某个失效换下一个；`systemctl restart docker` 会重启所有容器，建议在没人用时操作。

**办法二：构建时兜底换 alpine 软件源**（`apk add ffmpeg` 卡住时）

现有 `Dockerfile` 没有换源参数，可以临时生成一份改过源的副本来构建（不改原文件）：

```bash
cd /vol1/1000/docker/script-manager
sed 's#RUN apk add --no-cache ca-certificates tzdata#RUN sed -i "s|dl-cdn.alpinelinux.org|mirrors.tuna.tsinghua.edu.cn|g" /etc/apk/repositories \&\& apk add --no-cache ca-certificates tzdata#' \
  Dockerfile > Dockerfile.cn
sudo docker build -f Dockerfile.cn --build-arg WITH_FFMPEG=1 -t script-manager:2.0 .
```

构建前可先看一眼改动是否生效：

```bash
grep -n "mirrors.tuna" Dockerfile.cn
```

（注意：基础镜像 `golang:1.23-alpine`、`alpine:3.20` 本身还是要拉，由上面的镜像加速解决。）

**办法三：别在 NAS 上编译，改在 PC 上编好再传**（最省心）

见第 4.4 节，Windows 上用 `GOOS=linux` 交叉编译出 5.7MB 单文件，NAS 上只跑第 4.1 节的 `docker build -f Dockerfile.bin`。

### 9.2 架构必须是 x86_64

- 飞牛 fnOS 官方仅支持 x86_64，`uname -m` 应为 `x86_64`。
- 若在别处 `docker save` 的镜像来自 Apple Silicon / ARM 服务器，`docker run` 会报：
  `exec /app/script-manager: exec format error`。
  解决：在 ARM 机器上 `GOARCH=arm64` 重编二进制，或直接用第 4 章按目标架构重做镜像。
- 验证镜像架构：`sudo docker inspect script-manager:2.0 --format '{{.Architecture}}'`，应为 `amd64`。

### 9.3 路径大小写与中文路径

- Linux 区分大小写，Windows 上能打开的 `\vol2\@team\...\脚本管理` 在 NAS 上写成小写目录就找不到。**统一用 `ls` 里看到的真实大小写**，不要手敲。
- 路径含中文、`@`、`+`、空格时：
  - compose 里的 `volumes` **必须加双引号**（现有文件已经加了，别删）。
  - shell 命令里也必须加双引号：`-v "/vol2/@team/.../脚本管理:/data"`。
- 从 Windows 上传目录时注意：如果曾经在同一共享目录里创建过 `产品A` 和 `产品a`，Linux 上会变成两个不同目录，页面上看起来「重复」。合并前先清理。
- 挂载路径建议用共享目录的绝对路径，不要用 `~` 或相对路径；`docker compose` 会以 compose 文件所在目录为基准解析相对路径，容易挂到意料之外的位置。

### 9.4 时区不对（时间戳 / 保存时间差 8 小时）

- 现有 compose 已设 `TZ: "Asia/Shanghai"`，镜像也装了 `tzdata`，正常就生效。
- 校验：`sudo docker exec script-manager date`。
- 若显示 UTC，检查是否有人删了 `TZ`，或者用了 `alpine` 精简版但没装 `tzdata`（第 4.3 节的自建镜像已包含）。
- 浏览器端显示的时间来自本机，与容器时区无关；`文案.json` 的 `updatedAt` 才是容器时间。

### 9.5 容器里没有 ffmpeg

三种处理，按成本排序：

1. **重新构建一体镜像**（推荐）：确认 compose 里 `WITH_FFMPEG: "1"`，`sudo docker compose up -d --build`。
2. **把静态版 ffmpeg 放进数据目录**（重建容器也不丢）：下载 johnvansickle 的 Linux amd64 static build，解压后放到：

```bash
sudo mkdir -p "/vol2/路径/脚本管理/bin"
# 上传 ffmpeg、ffprobe 到该目录后：
sudo chmod +x "/vol2/路径/脚本管理/bin/"*
sudo docker restart script-manager
```

程序会自动识别 `/data/bin/ffmpeg`；也可以显式设 `SM_FFMPEG` / `SM_FFPROBE`。

3. **容器内临时装**：`sudo docker exec -it script-manager apk add --no-cache ffmpeg`（容器一重建就没了）。

### 9.6 8686 端口被占

```bash
sudo ss -ltnp | grep 8686
# 报错：listen tcp :8686: bind: address already in use
# 处理：compose 改成 "8787:8686"，或停掉占用端口的飞牛应用
```

### 9.7 `.dockerignore` 把你的文件排除了

现有 `.dockerignore` 排除了：

```
script-manager.exe
script-manager-linux-amd64      ← 直接 COPY 二进制会失败
script-manager-linux-arm64
api_test.go
部署说明.md
*.md                             ← 所有 markdown（含本指南）都不会进镜像，正常
.git
```

所以第 4.1 节一定要把二进制复制成 `sm-bin/script-manager` 再 COPY；`*.md` 被排除没关系，镜像里不需要文档。

### 9.8 compose 命令不存在 / 版本差异

```bash
docker compose version      # v2，飞牛新系统自带
docker-compose version      # v1，老系统
```

- 报 `unknown flag: --build`：说明用的是 v1，`docker-compose up -d --build` 依然可用，只是提示不同。
- 报 `no configuration file provided`：不在 `docker-compose.yml` 所在目录。必须 `cd /vol1/1000/docker/script-manager` 再执行。

### 9.9 飞牛 Docker UI 与 SSH 混用

- 用 SSH 的 `docker compose` 建好的项目，在飞牛 Docker「项目」页面里也可能显示；但**用 UI 编辑 YAML 后要以 UI 保存的为准**，两边同时改会互相覆盖。
- UI 新建「项目」时对 `build:` 支持有限，源码构建建议走 SSH；纯运行（已有镜像）可以用 UI 粘贴第 5 章的最小 compose。

### 9.10 上传大视频失败

- 单文件上限 **4GB**（`api.go` 的 `maxUpload`），超过直接报「上传失败或文件超过 4GB」。
- 上传时浏览器会把整段从电脑传到 NAS，走的是 **你的电脑 → NAS** 的网络，不是 NAS 内部拷贝，4K 长片要等。
- 磁盘不足时写入 `.upload-*` 临时文件会失败，先 `df -h` 看数据盘余量。

### 9.11 团队共享盘（`@team`）的特殊行为

飞牛团队文件夹底层是权限受控的共享目录，个别版本上 Docker 以 root 绑定挂载后会遇到：

- 启动后 `/data` 不可写 → 日志 `无法创建数据目录 /data`；
- 文件能写但飞牛「文件管理」里看不到/删不掉 → 属主为 `root`。

处理顺序：`chown -R 1000:1000` → 改挂到普通共享目录验证 → 确认后再切回团队目录。若团队目录始终不可写，就把数据放 `/vol1/1000/docker/script-manager/data`，再用飞牛的「共享」或定时任务做同步。

---

## 10. 日常运维

### 10.1 更新版本

源码在 NAS 上：

```bash
cd /vol1/1000/docker/script-manager
# 用文件管理覆盖新的 main.go / api.go / tools.go / collab.go / web/
sudo docker compose up -d --build
```

只有新二进制：

```bash
cp -f 新文件 /vol1/1000/docker/script-manager/sm-bin/script-manager
cd /vol1/1000/docker/script-manager
sudo docker build -f Dockerfile.bin -t script-manager:2.0 .
sudo docker compose up -d --force-recreate
```

### 10.2 日志

```bash
sudo docker logs -f --tail 100 script-manager
```

### 10.3 备份

停不停服都能备，但建议停 3 秒确保 `协同.json` 落盘：

```bash
sudo docker stop script-manager
sudo tar czf /vol1/1000/docker/backup-script-manager-$(date +%F).tar.gz \
  -C "/vol2/路径" "脚本管理"
sudo docker start script-manager
```

### 10.4 停止 / 卸载（保留数据）

```bash
cd /vol1/1000/docker/script-manager
sudo docker compose stop        # 暂停
sudo docker compose down        # 删除容器，数据目录不受影响
sudo docker rmi script-manager:2.0   # 需要时再删镜像
```

### 10.5 最小化运行版 compose（可选）

不想保留 `build:` 段、只想稳定运行已有镜像时，另存一个文件：

```bash
cd /vol1/1000/docker/script-manager
cat > docker-compose.run.yml <<'EOF'
services:
  script-manager:
    image: script-manager:2.0
    container_name: script-manager
    restart: always
    ports:
      - "8686:8686"
    environment:
      SM_ADDR: ":8686"
      SM_DATA: "/data"
      TZ: "Asia/Shanghai"
    volumes:
      - "/vol2/路径/脚本管理:/data"
EOF

sudo docker compose -f docker-compose.run.yml up -d
```

---

## 11. 速查：第一次装的 6 条命令

> 这些命令在 NAS 的 SSH 终端里执行，怎么连、怎么粘贴见第 0 章；不想开 SSH 见 0.5。

已在 NAS 上放好源码文件的前提下（假设目录 `/vol1/1000/docker/script-manager`，数据目录用现有配置里的团队路径）：

```bash
# 1. 建数据目录（路径按你机器实际情况替换，注意双引号）
sudo mkdir -p "/vol2/路径/脚本管理"

# 2. 进项目目录
cd /vol1/1000/docker/script-manager

# 3. 构建并启动（镜像内自带 ffmpeg）
sudo docker compose up -d --build

# 4. 看状态，应为 Up (healthy)
sudo docker ps --filter name=script-manager

# 5. 健康检查，应返回 {"ok":true}
sudo docker exec script-manager wget -qO- http://127.0.0.1:8686/api/health

# 6. 浏览器打开
echo "http://$(hostname -I | awk '{print $1}'):8686"
```

只有二进制、不编译的情况，把第 3 步替换为：

```bash
cp script-manager-linux-amd64 sm-bin/script-manager
sudo docker build -f Dockerfile.bin -t script-manager:2.0 .
sudo docker compose up -d
```

---

## 12. 仍需你确认的信息（已按最常见的默认值写进上文）

| 待确认 | 本文采用的默认值 | 怎么改 |
|---|---|---|
| NAS 架构 | x86_64（飞牛官方唯一支持） | 若是 arm64，按 4.4 重编 |
| 是否已有镜像 | 假设**没有**，走源码构建（第 3 章） | 有镜像走 4.2，有二进制走 4.1 |
| 宿主端口 | `8686` | 改 compose `ports` 左侧 |
| 数据目录 | 沿用 compose 里的 `/vol2/路径/脚本管理` | 改 compose `volumes`，先建目录并授权 |
| 存储池编号 | `/vol1` 放 Docker 项目，`/vol2` 放业务数据 | `ls /vol1 /vol2` 确认真实编号 |
| 是否要 ffmpeg | 要（`WITH_FFMPEG: "1"`，镜像约 90MB） | 改 `"0"` 可省 80MB，但智能裁切不可用 |
