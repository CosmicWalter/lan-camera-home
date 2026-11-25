#include <HCNetSDK.h>

#include "video.h"

namespace {

BOOL CALLBACK alarm_message(LONG command, NET_DVR_ALARMER*, char* payload, DWORD size, void* opaque) {
    auto& events = *static_cast<jiajing::Events*>(opaque);
    if (command != COMM_ALARM_V30 || payload == nullptr || size < sizeof(NET_DVR_ALARMINFO_V30)) {
        return TRUE;
    }
    NET_DVR_ALARMINFO_V30 alarm{};
    std::memcpy(&alarm, payload, sizeof(alarm));
    // HCNetSDK numbers alarm channels from 1. V30 supplies a channel bitmap
    // for motion type 3 and doesn't include an explicit motion-end flag.
    if (alarm.dwAlarmType == 3 && alarm.byChannel[events.channel - 1] != 0) {
        events.push("MOTION");
    }
    return TRUE;
}

void CALLBACK exception_message(DWORD type, LONG, LONG, void* opaque) {
    if (type == EXCEPTION_ALARM || type == EXCEPTION_PREVIEW) {
        static_cast<jiajing::Events*>(opaque)->disconnected = true;
    }
}

void CALLBACK video_data(LONG, DWORD type, BYTE* data, DWORD size, void* opaque) {
    if (type == NET_DVR_STREAMDATA) {
        static_cast<jiajing::VideoBuffer*>(opaque)->push(data, size);
    }
}

struct SDK {
    ~SDK() {
        if (preview >= 0) {
            NET_DVR_StopRealPlay(preview);
        }
        if (subscription >= 0) {
            NET_DVR_CloseAlarmChan_V30(subscription);
        }
        if (user >= 0) {
            NET_DVR_Logout(user);
        }
        NET_DVR_Cleanup();
    }
    LONG user = -1;
    LONG subscription = -1;
    LONG preview = -1;
};

}  // namespace

int main(int argc, char** argv) {
    const auto mode = jiajing::mode(argc, argv);
    jiajing::VideoOutput output;
    if (mode == jiajing::Mode::invalid || (jiajing::video_mode(mode) && !output.open())) {
        return 2;
    }
    jiajing::Events events;
    jiajing::VideoBuffer video;
    if (!NET_DVR_Init()) {
        return jiajing::failure(NET_DVR_GetLastError());
    }
    SDK sdk;
    if (mode == jiajing::Mode::self_test) {
        std::printf("HCNetSDK build 0x%lx\n", static_cast<unsigned long>(NET_DVR_GetSDKBuildVersion()));
        return 0;
    }
    jiajing::Login config;
    NET_DVR_USER_LOGIN_INFO login{};
    if (!jiajing::read_login(config) || (!jiajing::video_mode(mode) && config.channel > MAX_CHANNUM_V30) ||
        !jiajing::copy_text(login.sDeviceAddress, config.host) ||
        !jiajing::copy_text(login.sUserName, config.user) ||
        !jiajing::copy_text(login.sPassword, config.password)) {
        return 2;
    }
    events.channel = config.channel;
    login.wPort = static_cast<WORD>(config.port);
    jiajing::install_signals();
    NET_DVR_SetConnectTime(3200, 1);
    if (!NET_DVR_SetExceptionCallBack_V30(0, nullptr, exception_message, &events)) {
        return jiajing::failure(NET_DVR_GetLastError());
    }
    NET_DVR_DEVICEINFO_V40 device{};
    sdk.user = NET_DVR_Login_V40(&login, &device);
    if (sdk.user < 0) {
        return jiajing::failure(NET_DVR_GetLastError());
    }
    if (jiajing::video_mode(mode)) {
        NET_DVR_PREVIEWINFO preview{};
        preview.lChannel = config.channel;
        preview.dwStreamType = mode == jiajing::Mode::sub_stream ? 1 : 0;
        preview.dwLinkMode = 0;  // TCP with HCNetSDK's native protocol.
        preview.bBlocked = 1;
        sdk.preview = NET_DVR_RealPlay_V40(sdk.user, &preview, video_data, &video);
        if (sdk.preview < 0) {
            return jiajing::failure(NET_DVR_GetLastError());
        }
        return video.write(output.file, "mpeg", events.disconnected);
    }
    if (!NET_DVR_SetDVRMessageCallBack_V31(alarm_message, &events)) {
        return jiajing::failure(NET_DVR_GetLastError());
    }
    NET_DVR_SETUPALARM_PARAM params{};
    params.dwSize = sizeof(params);
    params.byLevel = 1;
    params.byAlarmInfoType = 0;  // Request the fixed-size COMM_ALARM_V30 payload.
    sdk.subscription = NET_DVR_SetupAlarmChan_V41(sdk.user, &params);
    if (sdk.subscription < 0) {
        return jiajing::failure(NET_DVR_GetLastError());
    }
    events.stream();
    return 0;
}
