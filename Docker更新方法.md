# 脚本管理：Docker 更新方法（飞牛 NAS）

适用环境：

| 项目 | 值 |
| --- | --- |
| 部署目录 | `/vol1/1000/docker/script-manager` |
| 镜像 | `script-manager:2.0` |
| 容器名 | `script-manager` |
| 启动方式 | `docker compose` |
| 数据目录（挂载到容器 `/data`） | `/vol2/路径/脚本管理` |
| 剪映草稿目录（挂载到容器 `/drafts`） | `/vol2/1001/备份/草稿` |
| 访问端口 | `8686` |

更新一次代码的顺序：**留旧镜像 → 换文件 → 重建 → 验证**。数据目录全程不动。

---

## 1. 更新前：给旧镜像留个标签

```bash
cd /vol1/1000/docker/script-manager

# 看看当前用的是哪个镜像
docker images | grep script-manager

# 留一份旧镜像，回滚时直接用（日期按当天改）
sudo docker tag script-manager:2.0 script-manager:backup-20261117

# 备份 compose，防止这次改了挂载/环境变量后想还原
cp docker-compose.yml docker-compose.yml.bak-20261117
```

数据目录 `/vol2/路径/脚本管理` **不用动、不用备份到别处**，所有产品、脚本、`_历史.json`、上传素材都在里面，重建容器不会碰它。

---

## 2. 换文件：直接复制到 U 盘映射目录（主方式）

`U:\script-manager` 就是 NAS 上的 `/vol1/1000/docker/script-manager`。在 Windows 资源管理器里把新文件复制进去即可，不需要 scp，也不需要 `docker cp`——`docker compose build` 读的就是这个目录。

先确认映射没指错：打开 `U:\script-manager`，能看到 `Dockerfile`、`docker-compose.yml`、`web` 文件夹。

### 操作

1. 打开本地目录 `V:\traecode\文案编写素材管控\2026-11-17\script-manager\outputs`。
2. 选中下面 13 个文件，`Ctrl+C`。
3. 打开 `U:\script-manager`，`Ctrl+V`，弹出覆盖提示选「替换目标中的文件」。

```text
Dockerfile
docker-compose.yml
go.mod
main.go
api.go
tools.go
collab.go
history.go
jianying.go
export.go
perm.go
umask_unix.go
umask_windows.go
```

`perm.go`、`umask_unix.go`、`umask_windows.go` 是这次修「目录权限 0000」新增的，**缺一个都会编译失败**（`ensureDir` / `applyUmask` 未定义）。`umask_unix.go` 与 `umask_windows.go` 只有构建标签不同，两个都要在。

4. 处理 `web\`：进入本地 `outputs\web`，选中 `index.html`、`app.js`、`collab.js`、`styles.css` 四个文件复制，粘贴到 `U:\script-manager\web`，全部选「替换」。
   - 如果本地 `web` 里还多了别的文件，同样复制过去。
   - **不要**把 `web` 文件夹拖到 `U:\script-manager\web` 里面（会变成 `web\web`）；要覆盖就进到目标 `web` 目录里粘贴，或在上一级删除整个旧 `web` 后再复制新的 `web` 进来。

### 不要覆盖、不要删

- 数据目录 `/vol2/路径/脚本管理`（产品、脚本、`_历史.json`、上传素材）——它不在 `U:\script-manager` 里，正常复制不会碰到，别手动去动它。
- `U:\script-manager` 里原有的 `go.sum`、`.env`、日志、你自己加的脚本等文件：本次不更新它们，**不要为了「干净」把目录清空**。
- `docker-compose.yml` 里的两处挂载和环境变量是部署时改过的，新文件会整份覆盖它。粘贴前先打开旧的看一眼，确认新 compose 里仍保留这三项：

```yaml
environment:
  SM_ADDR: ":8686"
  SM_DATA: "/data"
  TZ: "Asia/Shanghai"
  SM_DRAFT_ROOT: "/drafts/JianyingPro Drafts"
volumes:
  - "/vol2/路径/脚本管理:/data"
  - "/vol2/1001/备份/草稿:/drafts"
```

  如果 NAS 上的 compose 有你本地没有的改动（端口、路径、额外环境变量），先备份 `docker-compose.yml`，粘贴新文件后把这几项补回去，再继续第 3 步。

  本次新 compose 去掉了 `args: WITH_FFMPEG`——`Dockerfile` 已改成**固定安装 ffmpeg**，不再需要构建参数。旧 compose 里如果还留着这一行不会报错（未使用的构建参数会被忽略），但建议一起换成新文件，免得以后误以为是开关。

  **`SM_DATA` 和 `/data` 挂载两个都不能少**。少任何一个，数据都会写进容器内部（`/app/脚本管理`），宿主机和 Windows 映射盘上就什么都看不到，重建容器即丢。新版 compose 这两项都在。
- 不要复制 `script-manager.exe`、`script-manager`（Linux 二进制）、`work\`，镜像构建用不到。

### 备选方式 1：scp

映射盘不可用时再用：

```powershell
cd "V:\traecode\文案编写素材管控\2026-11-17\script-manager\outputs"
scp main.go api.go tools.go collab.go history.go jianying.go export.go go.mod Dockerfile docker-compose.yml <用户名>@<NAS的IP>:/vol1/1000/docker/script-manager/
scp -r web <用户名>@<NAS的IP>:/vol1/1000/docker/script-manager/
```

### 备选方式 2：飞牛「文件管理」网页上传

飞牛桌面 →「文件管理」→ 进入 `/vol1/1000/docker/script-manager`，上传同样的 10 个文件 + `web` 内 4 个文件，覆盖同名文件。

---

## 3. 重建镜像和容器

文件复制到 `U:\script-manager` 后，到 NAS 上（SSH 或终端）执行即可，不需要再 `docker cp`。

```bash
cd /vol1/1000/docker/script-manager

# 常规更新：重新编译镜像并替换容器
sudo docker compose up -d --build
```

`up -d --build`：先按 Dockerfile 重新编译，再启动。镜像 ID 变了，容器会自动重建。

`--force-recreate`：不管配置有没有变，强制删掉旧容器再建一个新的。**以下情况必须加**：

- 改了 `docker-compose.yml` 里的 `volumes`（这次新增了 `/vol2/1001/备份/草稿:/drafts`）
- 改了 `environment`（如 `SM_DRAFT_ROOT`、`SM_ADDR`、`TZ`）
- 改了 `ports`
- 执行完 `up -d --build` 后发现 `docker inspect` 里的挂载还是旧的

```bash
sudo docker compose up -d --build --force-recreate
```

只改前端也要重建：前端文件通过 `//go:embed web` 编进二进制，改完不 `--build` 页面不会变。

---

## 4. 浏览器强刷

重建完成后，浏览器 `Ctrl + F5`（或 `Ctrl + Shift + R`）强刷；协作中的同事也要各自刷新一次，否则还在跑旧 JS。

判断是不是新版本：页面顶部有「历史」按钮、导出按钮、「剪辑素材」常驻在右侧，就是这版。

---

## 5. 验证

```bash
# 1) 容器在跑、镜像和重启次数
sudo docker compose ps

# 2) 健康接口，应返回 {"ok":true}
curl -s http://127.0.0.1:8686/api/health

# 3) 最近日志，确认没有 panic / 端口占用 / 权限错误
sudo docker logs --tail=100 script-manager

# 4) 容器实际挂载，确认两条都在
sudo docker inspect script-manager --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}'

# 5) 容器里的启动参数
sudo docker inspect script-manager --format '{{range .Config.Env}}{{println .}}{{end}}' | grep SM_

# 6) ffmpeg / ffprobe 是否在镜像里（两条都应打印版本行）
sudo docker exec script-manager ffmpeg -version | head -n 1
sudo docker exec script-manager ffprobe -version | head -n 1

# 7) 数据目录到底落在哪：日志里应是「数据目录: /data」
sudo docker logs script-manager 2>&1 | grep 数据目录
sudo docker exec script-manager ls -la /data
```

界面上再确认四处：

- 右侧「剪辑素材」面板一进页面就在，可以直接把片段拖进表格；
- 顶部有「历史」，列表项/编辑器里能打开修改记录；
- 导出菜单里有「剪映草稿」和「CSV + 时间码文案」；
- 右上角「ffmpeg」设置里 `ffmpeg` / `ffprobe` 都是绿色就绪，来源「系统 PATH」，路径 `/usr/bin/ffmpeg`、`/usr/bin/ffprobe`。

宿主机上再确认一次数据真的落在挂载盘里：

```bash
sudo ls -la "/vol2/路径/脚本管理/"
```

应该能看到产品目录、`_脚本状态.json`、`设置.json` 等。Windows 的 `Z:` 看不到时重新打开一次映射盘（SMB 会缓存目录列表）。

以上都对，说明新版本真的换上了。

---

## 6. 回滚

### 6.1 镜像回滚（最快，代码不用动）

```bash
cd /vol1/1000/docker/script-manager

# 把旧镜像重新指回 2.0
sudo docker tag script-manager:backup-20261117 script-manager:2.0

# 强制用这个镜像重建容器
sudo docker compose up -d --force-recreate
```

### 6.2 代码回滚

部署目录如果用了 git：

```bash
cd /vol1/1000/docker/script-manager
git status
git log --oneline -5
git checkout <上一个正常的提交>
sudo docker compose up -d --build --force-recreate
```

没有 git 就用备份文件：

```bash
cp docker-compose.yml.bak-20261117 docker-compose.yml
# 再把旧版 .go / web 文件复制回 U:\script-manager（覆盖同名文件）
sudo docker compose up -d --build --force-recreate
```

数据目录不用回滚：这次更新没有改数据结构，历史记录多出来的文件不影响旧版本读取。

---

## 7. 清理

确认新版本稳定运行一段时间后再清：

```bash
# 清构建缓存（下次构建会变慢，但能释放较多空间）
sudo docker builder prune

# 只清悬空镜像（没有标签的中间层），最安全
sudo docker image prune

# 连没有容器使用的镜像一起清（会删掉 backup 标签，回滚前别执行）
sudo docker image prune -a
```

**这些时候不要清**：

- 刚更新完还没验证，或还在观察期（保留 `backup-*` 标签用于回滚）；
- 准备马上再改代码重建（缓存能明显加速）；
- 不确定哪些镜像还被其他容器使用（先 `docker ps -a` 看清楚）。

---

## 8. 常见失败对照

### 8.1 构建报 registry 401 / 拉取镜像失败

现象：`failed to authorize`、`401 Unauthorized`、`golang:1.23-alpine: not found`。

判断：访问 Docker Hub 的镜像源失效或被限流。

处理：

```bash
# 先手动拉一次，看是不是镜像源问题
sudo docker pull golang:1.23-alpine
sudo docker pull alpine:3.20

# 换可用的镜像源后重新构建
sudo docker compose build --no-cache
sudo docker compose up -d --force-recreate
```

如果一直拉不下来，用本机已有的 Go 编译 Linux 二进制，改成「只拷贝二进制」的 Dockerfile（联系我出一版 `Dockerfile.bin`）。

### 8.2 端口被占用

现象：`bind: address already in use`，容器起不来。

```bash
sudo ss -lntp | grep 8686
# 或
sudo lsof -i:8686
```

处理：停掉占用进程，或把 `docker-compose.yml` 的端口改成 `8687:8686`，再 `up -d --force-recreate`。

### 8.3 挂载路径不存在 / 权限不足

现象：日志里 `no such file or directory`、`permission denied`，或界面看不到数据。

```bash
ls -ld "/vol2/路径/脚本管理"
ls -ld "/vol2/1001/备份/草稿"

# 目录不存在就建
sudo mkdir -p "/vol2/1001/备份/草稿/JianyingPro Drafts"

# 权限按飞牛实际用户改（先看 ls -ld 的属主）
sudo chown -R 1000:1000 "/vol2/路径/脚本管理"
```

确认 compose 里路径拼写、引号、大小写完全一致；改完 `up -d --force-recreate`。

### 8.4 编译报错

现象：`docker compose up -d --build` 在 `go build` 阶段失败。

```bash
# 看完整报错行号
sudo docker compose build --progress=plain
```

常见原因：

- 少传了 `.go` 文件（本次共 10 个：`main.go api.go tools.go collab.go history.go jianying.go export.go perm.go umask_unix.go umask_windows.go`）；
- `Dockerfile` 里 `COPY` 没包含新文件；
- 代码复制不完整（在 `U:\script-manager` 里核对文件大小，别是 0 字节或漏了 `history.go`、`jianying.go`、`export.go`）。

### 8.5 容器反复重启

```bash
sudo docker compose ps
sudo docker logs --tail=200 script-manager
sudo docker inspect script-manager --format '{{.State.ExitCode}} {{.State.Error}}'
```

按日志处理：端口冲突改端口，挂载问题改路径/权限，`panic` 就把日志发我。临时停掉自动重启便于排查：

```bash
sudo docker update --restart=no script-manager
sudo docker stop script-manager
```

### 8.6 数据看不到：`Z:\路径\脚本管理` 是空的

现象：页面里产品/脚本都在，但 Windows 映射盘（NAS 上 `/vol2/路径/脚本管理`）里什么都没有。

原因有两个：

1. 容器把数据写进了自己的内部目录（`/app/脚本管理`），即 `SM_DATA` 没生效或 `/data` 挂载没生效。程序在 `SM_DATA` 为空时会退回到「程序旁边的 `脚本管理`」——不可见、重建即丢。
2. 目录/文件被建成了 **0000 权限**（`ls -la` 显示 `d---------`、`----------`）。容器以 root 运行，自己读写正常，但宿主机和 SMB 映射盘看不到里面的条目，看起来就是空的。这是已经踩到过的真实故障。

```bash
# 1) 启动参数里有没有 SM_DATA=/data
sudo docker inspect script-manager --format '{{range .Config.Env}}{{println .}}{{end}}' | grep SM_

# 2) 挂载里有没有 宿主机目录 -> /data
sudo docker inspect script-manager --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}'

# 3) 进程实际用的数据目录
sudo docker logs script-manager 2>&1 | grep 数据目录

# 4) 两边各有什么
sudo docker exec script-manager ls -la /data
sudo docker exec script-manager ls -la /app/脚本管理
```

对照处理：

| 现象 | 处理 |
| --- | --- |
| 第 1、2 项都对，日志是 `数据目录: /data` | 数据在宿主机上，只是种子或映射盘缓存问题：`sudo ls -la "/vol2/路径/脚本管理/"` 确认，Windows 重开 `Z:` |
| 日志是 `数据目录: /app/脚本管理` | compose 里 `SM_DATA` 丢了，补 `SM_DATA: "/data"` 后 `--force-recreate` |
| 挂载列表里没有 `/data` | compose 的 `volumes` 丢了，补回去再 `--force-recreate` |
| 宿主机目录路径拼错（大小写、`+` 号） | Docker 会静默新建一个空目录并挂进去，用 `ls -ld` 核对真实路径 |
| 第 4 项显示 `d---------` / `----------` | 权限被写成 0000，按下面的 `chown` + `chmod` 修 |

**权限是 0000 时手动修**（已在真实环境确认过，数据都在，不用重建容器）：

```bash
sudo chown -R 1000:1000 "/vol2/路径/脚本管理"
sudo chmod -R u=rwX,g=rwX,o=rX "/vol2/路径/脚本管理"

# 期望看到 drwxrwxr-x（目录）和 -rw-rw-r--（文件）
ls -la "/vol2/路径/脚本管理"
```

修完 Windows 上刷新 `Z:` 就能看到 `_剪辑素材`、`_脚本状态.json`、产品目录、`设置.json`。

程序侧也修了（新建目录 0775、文件 0664，启动显式 `umask 0002`，并会自动把历史遗留的 0 权限条目改回来），但**必须重建镜像才生效**：`sudo docker compose up -d --build --force-recreate`。重建前先用上面的 `chmod` 手动修一次即可。

**草稿目录是 0007（`d------rwx`）时**：这是旧 umask 故障的另一副面孔——属主和同组没有权限，只有 other 是 `rwx`。容器以 root 运行通常还能写（root 不受普通权限位限制，只受 `:ro` 挂载限制），但属主和 SMB 侧会读写异常。用同一条命令修：

```bash
sudo chown -R 1000:1000 "/vol2/1001/备份/草稿/JianyingPro Drafts"
sudo chmod -R u=rwX,g=rwX,o=rX "/vol2/1001/备份/草稿/JianyingPro Drafts"
ls -la "/vol2/1001/备份/草稿/JianyingPro Drafts" | head   # 应为 drwxrwxr-x
```

修完后在页面点「导出」，预检的「草稿根目录」应显示「（可写）」。

**把容器内的数据救出来**（容器还在的时候做，别先 `docker rm`）：

```bash
sudo docker cp script-manager:/app/脚本管理/. "/vol2/路径/脚本管理/"
sudo ls -la "/vol2/路径/脚本管理/"
cd /vol1/1000/docker/script-manager
sudo docker compose up -d --build --force-recreate
```

`docker cp` 只新增/覆盖，不删已有文件。宿主机目录里若已有新数据，先备份再拷。

### 8.7 界面里 ffmpeg / ffprobe 都「未找到」

新版镜像固定 `apk add --no-cache ffmpeg`，路径 `/usr/bin/ffmpeg`、`/usr/bin/ffprobe`，程序通过 `PATH` 自动识别。界面上还显示未找到，按顺序查：

```bash
# 1) 镜像是什么时候建的（不是今天，说明没换上新镜像）
sudo docker image inspect script-manager:2.0 --format '{{.Created}}'

# 2) 容器跑的是不是这个镜像（Image 与上面的 Id 要一致）
sudo docker inspect script-manager --format '{{.Image}}'
sudo docker images --no-trunc | grep script-manager

# 3) 容器内能不能跑
sudo docker exec script-manager ffmpeg -version | head -n 1
sudo docker exec script-manager ffprobe -version | head -n 1
```

- 第 1 项时间旧 → `sudo docker compose up -d --build --force-recreate`；
- 第 3 项报 `not found`，但镜像是今天建的 → 构建时 `apk add` 没成功（构建日志里应有 `ffmpeg version ...` 两行）；看 `sudo docker compose build --progress=plain` 的输出，多半是 apk 源网络问题，换源后重建；
- 容器内能跑、界面仍显示未找到 → 之前手动填过不存在的路径并存在 `设置.json` 里，清空「ffmpeg 设置」里的两个输入框再保存；查找结果有 10 秒缓存，点一次「重新检测」。

兜底（一般用不到）：

```bash
# 临时装进正在运行的容器（重建后需重装，不推荐长期用）
sudo docker exec -it script-manager apk add --no-cache ffmpeg
```

或把 Linux amd64 静态版 `ffmpeg` / `ffprobe` 放进 `<数据目录>/bin/`，程序会自动优先用它，容器重建也不丢。
