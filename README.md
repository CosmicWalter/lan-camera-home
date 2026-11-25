# 家镜

在浏览器里看家里的摄像头。家镜以 Moonfire NVR 为录像后端，提供简体中文的实时预览、录像回放、MP4 下载和移动告警列表。

**[打开 GitHub Pages](https://cosmicwalter.github.io/lan-camera-home/)** · **[下载家镜桥接程序](https://github.com/CosmicWalter/lan-camera-home/releases/latest)**

首次打开会显示标有「演示」的合成画面。完成配对后，浏览器从本机桥接获取你家里的视频。录像写入 Moonfire 配置的本地磁盘，摄像头账号保持在 Moonfire 内。

## 快速开始

1. 在家里能访问摄像头的设备上安装 Moonfire NVR。家镜当前以 **Moonfire v0.7.33** 为兼容基线。启动录像前，在 Moonfire 的 `config` 终端界面里添加存储目录，再添加摄像头。
   - 填写摄像头的 RTSP 地址和账号。建议使用 H.264，开启主码流录像，并为其分配存储目录和保留空间。
   - 如需低带宽预览，也可以配置并启用子码流。家镜优先预览子码流。Moonfire v0.7.33 通过已启用录像的码流提供实时视频。
   - Moonfire 会按上游配置持续录像。家镜显示录像配置和录像片段。 [Moonfire 安装指南](https://github.com/scottlamb/moonfire-nvr/blob/v0.7.33/guide/install.md)
2. 为桥接准备 Moonfire 服务连接。按部署场景选择下面一种方式。
3. 下载、解压对应平台的桥接程序，在运行浏览器的电脑上启动它。macOS 和 Linux 可以用终端执行 `./lan-camera-home`，Windows 可在 PowerShell 执行 `.\lan-camera-home.exe`。终端会显示本地控制台地址、端口和配对码。
4. 使用新版桌面 Chrome 或 Edge 打开 GitHub Pages，点击「连接本地设备」，粘贴配对码。浏览器弹出本地网络访问提示时，选择允许。也可打开终端显示的本机控制台，使用同一个配对码。
5. 在「实时预览」查看画面。在「录像回放」中按摄像头、码流和日期查询。点击「播放」或「下载」。

### Moonfire 和浏览器在同一台电脑

可在 Moonfire TOML 文件中追加一个用于桥接的 Unix 套接字配置。将目录替换成仅 Moonfire 和当前桥接用户可访问的目录，并确保它存在。

```toml
[[binds]]
unix = "/var/lib/moonfire-nvr/jiajing.sock"
allowUnauthenticatedPermissions = { viewVideo = true }
```

重启 Moonfire，并启动桥接。

```sh
./lan-camera-home -moonfire unix:/var/lib/moonfire-nvr/jiajing.sock
```

另一种方式是使用 Moonfire 的本机 TCP 监听，例如 `127.0.0.1:8080`，然后提供仅有 `viewVideo` 权限的专用会话。Moonfire CLI 生成 cookie 文件时需要独占数据库，先停止 Moonfire，生成后再启动。将 `viewer` 替换为已建账户，并在桥接电脑上妥善保管该文件。此选项读取 Unix 文件权限，适用于 Linux 和 macOS。

```sh
moonfire-nvr login --permissions '{"viewVideo":true}' --domain 127.0.0.1 --curl-cookie-jar moonfire-viewer.cookies viewer
./lan-camera-home -moonfire 127.0.0.1:8080 -cookie-file moonfire-viewer.cookies
```

### Moonfire 在家里的另一台服务器

使用 SSH 将 Moonfire 在服务器上的回环监听转发到浏览器所在电脑。如果在服务器上配置了仅有 `viewVideo` 权限的 Unix 套接字，可直接转发该套接字。SSH 用户需要能访问它。

```sh
ssh -N -o ExitOnForwardFailure -L 127.0.0.1:8080:/var/lib/moonfire-nvr/jiajing.sock nvr-host
```

保持 SSH 会话开启，在另一个终端启动桥接。已使用鉴权 TCP 端口的部署，也可以将转发目标换成 `127.0.0.1:8080` 并使用前面的专用 cookie 方式。

### 告警与品牌接入

- 告警页面读取 Moonfire Signals，将进入移动状态的信号显示为告警。连接本地设备后，实时预览和告警页面每 30 秒刷新一次；需已有事件源向 Moonfire 写入信号。 [Moonfire Signals API](https://github.com/scottlamb/moonfire-nvr/blob/v0.7.33/ref/api.md#get-apisignals)
- 海康威视、大华、宇视的设备可按型号手动配置 RTSP。ONVIF 自动发现、设备事件订阅及品牌 SDK 接入在后续版本中实现。

## 浏览器和本地访问

桥接只监听 `127.0.0.1`。公开网页只向本机桥接请求设备列表和视频，桥接使用配对码授权。配对码保存在当前网页内存里，刷新后需重新输入。视频播放和下载使用 30 分钟有效的单片段链接。每次重启桥接会生成新配对码，除非通过 `-pairing-file` 指定权限为 `0600` 的文件。

浏览器可能因设置或受管设备策略拒绝 HTTPS 网页访问本机桥接。此时请从当前电脑打开终端显示的 `http://127.0.0.1:8787/` 并配对。手机上的回环地址指向手机本身。当前桥接发行包面向桌面和 Linux，手机可体验 Pages 演示。

## 开发

需要 Go 1.23 或更高版本。静态界面与播放器在 `web/`，同一套文件用于 GitHub Pages 和桥接程序内置控制台。

```sh
go test -race ./...
go run . -moonfire 127.0.0.1:8080
```

Moonfire 持续录制 RTSP，桥接只代理允许列表里的只读 API，并将 Moonfire 实时 WebSocket 分片通过本地 HTTP 流转发给浏览器。

## 许可

家镜使用 GPL-3.0-or-later。感谢 [Moonfire NVR](https://github.com/scottlamb/moonfire-nvr) 项目。Moonfire NVR 按其 [上游许可证](https://github.com/scottlamb/moonfire-nvr/blob/v0.7.33/LICENSE.txt) 单独分发。演示素材是生成的住宅画面。
