#include "common.h"

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
        if (listening) {
            CLIENT_StopListen(user);
        }
        if (user != 0) {
            CLIENT_Logout(user);
        }
        CLIENT_Cleanup();
    }
    LLONG user = 0;
    bool listening = false;
};

}  // namespace

int main(int argc, char** argv) {
    const bool self_test = argc == 2 && std::strcmp(argv[1], "--self-test") == 0;
    if (argc != 1 && !self_test) {
        return 2;
    }
    jiajing::Events events;
    const auto opaque = reinterpret_cast<LDWORD>(&events);
    if (!CLIENT_Init(disconnected, opaque)) {
        return jiajing::failure(CLIENT_GetLastError());
    }
    SDK sdk;
    if (self_test) {
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
    CLIENT_SetDVRMessCallBack(alarm_message, opaque);
    NET_OUT_LOGIN_WITH_HIGHLEVEL_SECURITY device{};
    device.dwSize = sizeof(device);
    sdk.user = CLIENT_LoginWithHighLevelSecurity(&login, &device);
    if (sdk.user == 0) {
        return jiajing::failure(CLIENT_GetLastError());
    }
    if (!CLIENT_StartListenEx(sdk.user)) {
        return jiajing::failure(CLIENT_GetLastError());
    }
    sdk.listening = true;
    events.stream();
    return 0;
}
