#include "video.h"

#include <dhnetsdk.h>
#include <cstddef>

namespace {

void CALLBACK disconnected(LLONG, char*, LONG, LDWORD opaque) {
    reinterpret_cast<jiajing::Events*>(opaque)->disconnected = true;
}

BOOL CALLBACK alarm_message(LONG command, LLONG, char* payload, DWORD size, char*, LONG, LDWORD opaque) {
    auto& events = *reinterpret_cast<jiajing::Events*>(opaque);
    constexpr auto channel_offset = offsetof(ALARM_MOTIONDETECT_INFO, nChannelID);
    constexpr auto action_offset = offsetof(ALARM_MOTIONDETECT_INFO, nEventAction);
    if (command != DH_EVENT_MOTIONDETECT || payload == nullptr || size < action_offset + sizeof(int)) {
        return TRUE;
    }
    int channel = -1;
    int action = -1;
    std::memcpy(&channel, payload + channel_offset, sizeof(channel));
    std::memcpy(&action, payload + action_offset, sizeof(action));
    if (channel != events.channel - 1) {
        return TRUE;
    }
    switch (action) {
        case 0: events.push("MOTION"); break;
        case 1: events.push("MOTION_ON"); break;
        case 2: events.push("MOTION_OFF"); break;
        default: break;
    }
    return TRUE;
}

struct SDK {
    ~SDK() {
        if (preview != 0) {
            CLIENT_StopRealPlayEx(preview);
        }
        if (listening) {
            CLIENT_StopListen(user);
        }
        if (user != 0) {
            CLIENT_Logout(user);
        }
        CLIENT_Cleanup();
    }
    LLONG user = 0;
    LLONG preview = 0;
    bool listening = false;
};

void CALLBACK video_data(LLONG, DWORD type, BYTE* data, DWORD size, LONG, LDWORD opaque) {
    if (type == NET_DATA_CALL_BACK_VALUE + EM_REAL_DATA_TYPE_PS) {
        reinterpret_cast<jiajing::VideoBuffer*>(opaque)->push(data, size);
    }
}
}  // namespace

int main(int argc, char** argv) {
    const auto mode = jiajing::mode(argc, argv);
    jiajing::VideoOutput output;
    if (mode == jiajing::Mode::invalid || (jiajing::video_mode(mode) && !output.open())) {
        return 2;
    }
    jiajing::Events events;
    jiajing::VideoBuffer video;
    const auto opaque = reinterpret_cast<LDWORD>(&events);
    if (!CLIENT_Init(disconnected, opaque)) {
        return jiajing::failure(CLIENT_GetLastError());
    }
    SDK sdk;
    if (mode == jiajing::Mode::self_test) {
        std::printf("Dahua NetSDK version 0x%lx\n", static_cast<unsigned long>(CLIENT_GetSDKVersion()));
        return 0;
    }
    jiajing::Login config;
    NET_IN_LOGIN_WITH_HIGHLEVEL_SECURITY login{};
    if (!jiajing::read_login(config) || !jiajing::copy_text(login.szIP, config.host) ||
        !jiajing::copy_text(login.szUserName, config.user) || !jiajing::copy_text(login.szPassword, config.password)) {
        return 2;
    }
    login.dwSize = sizeof(login);
    login.nPort = config.port;
    login.emSpecCap = EM_LOGIN_SPEC_CAP_TCP;
    events.channel = config.channel;
    jiajing::install_signals();
    CLIENT_SetConnectTime(3200, 1);
    NET_OUT_LOGIN_WITH_HIGHLEVEL_SECURITY device{};
    device.dwSize = sizeof(device);
    sdk.user = CLIENT_LoginWithHighLevelSecurity(&login, &device);
    if (sdk.user == 0) {
        return jiajing::failure(CLIENT_GetLastError());
    }
    if (jiajing::video_mode(mode)) {
        NET_IN_REALPLAY_BY_DATA_TYPE request{};
        request.dwSize = sizeof(request);
        request.nChannelID = config.channel - 1;
        request.rType = mode == jiajing::Mode::sub_stream ? DH_RType_Realplay_1 : DH_RType_Realplay;
        request.emDataType = EM_REAL_DATA_TYPE_PS;
        request.cbRealData = video_data;
        request.dwUser = reinterpret_cast<LDWORD>(&video);
        NET_OUT_REALPLAY_BY_DATA_TYPE response{};
        response.dwSize = sizeof(response);
        sdk.preview = CLIENT_RealPlayByDataType(sdk.user, &request, &response, 5000);
        if (sdk.preview == 0) {
            return jiajing::failure(CLIENT_GetLastError());
        }
        return video.write(output.file, "mpeg", events.disconnected);
    }
    CLIENT_SetDVRMessCallBack(alarm_message, opaque);
    if (!CLIENT_StartListenEx(sdk.user)) {
        return jiajing::failure(CLIENT_GetLastError());
    }
    sdk.listening = true;
    events.stream();
    return 0;
}
