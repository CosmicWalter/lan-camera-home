# 海康、大华、宇视原生 SDK 告警

已有 RTSP 录像的设备可以选用原生 SDK 接收移动告警。家镜将设备告警写入 Moonfire Signals，再在控制台告警页显示。每台设备可在 ONVIF 告警和品牌 SDK 告警中选择一种事件源。

| 品牌 | 接口 | 转发的事件 |
| --- | --- | --- |
| 海康威视 | HCNetSDK 设备登录和 V41 布防 | `COMM_ALARM_V30` 移动告警。收到事件时开始计时，可配置保持秒数。 |
| 大华 | NetSDK 高安全登录和 `CLIENT_StartListenEx` | `DH_EVENT_MOTIONDETECT` 移动脉冲、开始和结束。 |
| 宇视 | NetDEVSDK 私有协议登录和普通告警订阅 | `NETDEV_ALARM_MOVE_DETECT` 和对应的恢复事件。 |

## 安装适配器

准备能访问摄像头和 Moonfire 的 Linux 主机。根据设备和 CPU 架构，从厂商获取**同一版本的头文件、动态库和随包依赖**。部分下载需要登录厂商平台并确认许可。家镜开源仓库和发行包提供适配器源码。

- [海康威视设备网络 SDK](https://open.hikvision.com/osp)
- [大华 DEPP 开发平台](https://depp.dahuasecurity.com/)
- [宇视 SDK 下载中心](https://cn.uniview.com/Service/Service_Training/Download/SDK/2/201603/796481_194214_0.htm)

以下以海康为例。在家镜源目录运行，把 `./vendor-sdk` 改成 SDK 的实际位置。大华和宇视分别用 `DAHUA_SDK_DIR`、`UNIVIEW_SDK_DIR`，生成的适配器对应为 `jiajing-dahua` 和 `jiajing-uniview`。可以同时指定多个 SDK 目录。

```sh
cmake -S adapters/native -B build-native -DCMAKE_BUILD_TYPE=Release -DHIKVISION_SDK_DIR=./vendor-sdk
cmake --build build-native
./build-native/jiajing-hikvision --self-test
```

`--self-test` 初始化厂商库并显示 SDK 版本。若找不到库或其依赖，请按厂商文档设置动态库搜索路径。SDK 包的布局不同时，可直接指定 `hikvision_INCLUDE` 和 `hikvision_LIBRARY` 的位置，大华和宇视对应前缀为 `dahua_`、`uniview_`。

## 配对 Moonfire 信号

按 [ONVIF 告警文档的 Moonfire 信号配置](onvif-alerts.md#在-moonfire-配置信号) 创建移动信号和 `updateSignals` 会话。记下信号 ID。然后创建权限为 `0600` 的配置文件，例如 `sdk-motion.json`。

```json
{
  "host": "192.168.1.50",
  "port": 8000,
  "username": "device-viewer",
  "password": "替换为设备密码",
  "channel": 1,
  "deviceKind": "ipc",
  "signalId": 1,
  "stillState": 1,
  "motionState": 2,
  "motionHoldSeconds": 12
}
```

- `port` 是设备的厂商 SDK 服务端口。示例 `8000` 是常见的海康端口。大华经常使用 `37777`，宇视以设备配置为准。
- `channel` 从 1 开始。独立摄像头一般填 1。连接 NVR 时填写对应的告警通道号，并将 `deviceKind` 设置成 `nvr`。
- `motionHoldSeconds` 用于没有明确结束通知的移动脉冲。上表示例以外的厂商告警类型和机型需单独验证。

在设备端启用移动检测，再启动转发。应用在本机启动适配器，由标准输入传递设备凭据。设备连接断开后会重试订阅。SDK 在登录或布防阶段返回错误时，进程会停止并打印数值错误码，方便用厂商手册排查。

```sh
./lan-camera-home sdk-events -adapter ./build-native/jiajing-hikvision -config sdk-motion.json -moonfire 127.0.0.1:8080 -cookie-file moonfire-signals.cookies
```

打开家镜的「告警事件」页并触发一次移动检测。桥接通过已配置的 RTSP 和 Moonfire 继续提供实时预览、录像、回放与下载。
