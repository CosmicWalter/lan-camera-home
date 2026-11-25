# ONVIF 移动告警

在摄像头上启用移动检测。家镜订阅设备的 ONVIF PullPoint 服务，将移动开始及结束映射到 Moonfire 的信号。家镜控制台显示过去 24 小时内的移动开始事件。

## 在 Moonfire 配置信号

已有状态带 `motion: true` 的 Moonfire 信号时，记下信号 ID 和状态值即可。若需要新建信号，下列示例适用于 Moonfire v0.7.33。先停止 Moonfire，再以录像服务的用户运行 `moonfire-nvr sql`。如果使用了自定义数据库目录，请加上 `--db-dir`。

在 SQLite 界面运行下列命令。先查询摄像头，再把示例中的 `1` 改成要关联的摄像头 ID。最后一行返回为这次配置分配的信号 ID。

```sql
SELECT id, short_name FROM camera;
.parameter set :camera_id 1
BEGIN IMMEDIATE;
CREATE TEMP TABLE jiajing_motion AS
  SELECT id AS camera_id, randomblob(16) AS type_uuid FROM camera WHERE id = :camera_id;
INSERT INTO signal_type (uuid, config)
  SELECT type_uuid, '{"values":{"1":{"name":"静止"},"2":{"name":"移动","motion":true}}}'
  FROM jiajing_motion;
INSERT INTO signal (uuid, type_uuid, config)
  SELECT randomblob(16), type_uuid,
    json_object('shortName', '摄像头移动', 'cameraAssociations', json_object(CAST(camera_id AS TEXT), 'direct'))
  FROM jiajing_motion
  RETURNING id AS signal_id;
DROP TABLE jiajing_motion;
COMMIT;
```

退出 SQLite 界面。在启动 Moonfire 前，为家镜告警进程建立只有 `updateSignals` 权限的会话。这里的 `viewer` 是 Moonfire 中已存在的本地账号。

```sh
moonfire-nvr login --permissions '{"updateSignals":true}' --domain 127.0.0.1 --curl-cookie-jar moonfire-signals.cookies viewer
```

启动 Moonfire。告警进程和浏览器桥接可使用各自的 cookie 文件。另一个连接方式是为告警进程配备单独的本地 Unix 套接字，并在该套接字配置中给出 `allowUnauthenticatedPermissions = { updateSignals = true }`。

## 启动告警转发

创建一个权限为 `0600` 的文件，例如 `camera-motion.json`。写入该设备的 ONVIF 账号、密码，以及前一步获得的信号 ID。若设备包含多个视频通道，可指定可选的 `sourceToken`，仅转发 ONVIF 通知中源标记匹配的事件。

```json
{
  "endpoint": "http://192.168.1.50/onvif/device_service",
  "username": "onvif-viewer",
  "password": "替换为设备密码",
  "signalId": 1,
  "stillState": 1,
  "motionState": 2
}
```

在可连接摄像头和 Moonfire 的 Linux 或 macOS 设备上运行。已有 Unix 套接字时，可将 `-moonfire` 改成对应的 `unix:` 地址并省略 `-cookie-file`。

```sh
./lan-camera-home onvif-events -config camera-motion.json -moonfire 127.0.0.1:8080 -cookie-file moonfire-signals.cookies
```

告警进程断线后会重试订阅。现有状态最多保留约一分钟，之后由 Moonfire 记为未知。每台摄像头建一个信号并启动一个告警进程。当前转发使用 ONVIF 常见的移动事件主题和布尔状态字段，设备需要提供 PullPoint 服务。

[Moonfire Signals API](https://github.com/scottlamb/moonfire-nvr/blob/v0.7.33/ref/api.md#post-apisignals) · [ONVIF Events WSDL](https://www.onvif.org/ver10/events/wsdl/event.wsdl)
