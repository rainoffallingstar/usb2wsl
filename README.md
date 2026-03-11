# usb2wsl

Windows 后台程序：检测 USB 设备出现后，自动用 `usbipd-win` 转接到 WSL2（而不是让 Windows 挂载盘符），并触发 WSL 内的挂载脚本。

## CI（GitHub Actions）

仓库包含 Windows 构建工作流：push/PR 时在 `windows-latest` 上编译 `amd64`/`arm64`，产物在 Actions 的 artifacts 中下载。

## 依赖

- Windows 11
- WSL2（建议开启 systemd）
- `usbipd-win`（Windows 侧）

## 使用

### 1) 安装 WSL 挂载脚本

在目标 WSL distro 里执行（把 `<path-to-repo>` 换成你的仓库路径）：

```bash
sudo install -m 0755 /mnt/c/<path-to-repo>/scripts/wsl-mount-media.sh /usr/local/bin/wsl-mount-media.sh
```

脚本默认把介质挂载到 `/mnt/usbipd`，并把“成功挂载的目录”逐行输出（用于 Windows 侧自动打开对应目录）。

### 2) 准备配置

复制并编辑 `config.json`（从 `config.example.json` 复制修改）：

- `wslDistro`: 你的 distro 名称（`wsl -l -v` 可看）
- `allowVIDPID`: 允许转接的 VID:PID 列表（`usbipd list` 可看）
- `wslPostAttachBash`: attach 成功后在 WSL 中执行的命令（默认 `/usr/local/bin/wsl-mount-media.sh`）
- `autoStartDistro`: 启动/初始化 distro（默认建议 `true`）
- `autoInstallUsbipd`: 若未安装 `usbipd-win`，尝试用 `winget` 自动安装（需要 Windows 侧可用 winget）
- `openExplorer`: 挂载成功后自动打开资源管理器到具体挂载目录（脚本输出的路径）；无输出则回退到 `explorerOpenWslPath`
- `promptOnAttach`: 检测到不在允许列表内的设备时弹窗确认（是=仅本次，否=跳过，取消=记住该 VID:PID）
- `statePath`: 记住设备的状态文件（默认在 config 同目录 `usb2wsl.state.json`）
- `logPath`: 记录日志文件（同时输出到 stdout + 文件）

如果你不想手填 `allowVIDPID`，也可以保持为空并开启 `promptOnAttach=true`：第一次插入设备时点“取消(记住)”，会自动写入 `statePath`，以后同 VID:PID 不再询问。

### 3) 构建与运行（手动）

在 Windows（建议管理员 PowerShell）里执行：

```powershell
go build -o usb2wsl.exe .\cmd\usb2wsl
.\usb2wsl.exe -config .\config.json -v
```

常用调试命令：

```powershell
usbipd list
wsl -l -v
```

## 登录自启（最高权限，不弹 UAC）

推荐用子命令直接安装计划任务（脚本已内置在 `usb2wsl.exe` 内）。在管理员 PowerShell 里执行（会创建计划任务，以登录触发，权限为最高）：

```powershell
.\usb2wsl.exe task install
```

（仓库里的 `scripts/install-schtask.ps1` 仅用于查看/参考；`usb2wsl.exe` 实际使用的是内置脚本。）

如果希望计划任务写日志文件：

```powershell
.\usb2wsl.exe task install -log .\usb2wsl.log
```

确认任务：

```powershell
schtasks /Query /TN usb2wsl /V /FO LIST
```

也可改为开机自启（SYSTEM 最高权限）：

```powershell
.\usb2wsl.exe task install -onstart
```

## 容器读取方式（推荐）

Docker 容器无法在运行中“动态新增挂载”。推荐做法是：

- 让 WSL 把介质统一挂载到 `/mnt/usbipd`
- 启动容器时把该目录 bind-mount 进去，例如：

```bash
docker run --rm -it -v /mnt/usbipd:/media/usbipd:ro <image> bash
```

这样新插入的 U 盘/光驱被脚本挂载后，容器会立刻在 `/media/usbipd` 下可见。

## 注意

- `usbipd attach --wsl` 会把 USB 设备独占给 WSL，Windows 侧通常就不再出现盘符/可用设备。
- 光驱通常是 `/dev/sr0`；U 盘通常是 `/dev/sdX1`。脚本做了基于 `TRAN=usb` 的保守挂载。
- 如果资源管理器无法打开 `\\\\wsl$\\...`，先确认 WSL distro 正常运行（`wsl -d <distro> -- true`）。
