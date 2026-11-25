#include "common.h"

#include <NetDEVSDK.h>

namespace {

void STDCALL alarm_message(LPVOID, INT32 channel, NETDEV_ALARM_INFO_S alarm, LPVOID, INT32, LPVOID opaque) {
    auto& events = *static_cast<jiajing::Events*>(opaque);
    if (alarm.dwChannelID > 0) {
        channel = alarm.dwChannelID;
    }
    if (channel == 0 && !events.nvr) {
        channel = 1;  // Standalone IPCs can omit the NVR channel field.
    }
    if (channel != events.channel) {
        return;
    }
    switch (alarm.dwAlarmType) {
        case NETDEV_ALARM_MOVE_DETECT: events.push("MOTION_ON"); break;
        case NETDEV_ALARM_MOVE_DETECT_RECOVER: events.push("MOTION_OFF"); break;
        default: break;
    }
}

void STDCALL exception_message(LPVOID, INT32 type, LPVOID, LPVOID opaque, LPNETDEV_EXCEPTION_OUTPUT_INFO_S) {
    if (type == NETDEV_EXCEPTION_EXCHANGE || type == NETDEV_EXCEPTION_REPORT_ALARM_INTERRUPT) {
        static_cast<jiajing::Events*>(opaque)->disconnected = true;
    }
}

struct SDK {
    ~SDK() {
        if (listening) {
            NETDEV_Unsubscribe(user);
        }
        if (user != nullptr) {
            NETDEV_Logout(user);
        }
        NETDEV_Cleanup();
    }
    LPVOID user = nullptr;
    bool listening = false;
};

int error() {
    return jiajing::failure(static_cast<unsigned int>(NETDEV_GetLastError()));
}

}  // namespace

int main(int argc, char** argv) {
    const bool self_test = argc == 2 && std::strcmp(argv[1], "--self-test") == 0;
    if (argc != 1 && !self_test) {
        return 2;
    }
    jiajing::Events events;
    if (!NETDEV_Init()) {
        return error();
    }
    SDK sdk;
    if (self_test) {
        std::printf("Uniview NetDEVSDK version 0x%x\n", static_cast<unsigned int>(NETDEV_GetSDKVersion()));
        return 0;
    }
    jiajing::Login config;
    NETDEV_DEVICE_LOGIN_INFO_S login{};
    if (!jiajing::read_login(config) || !jiajing::copy_text(login.szIPAddr, config.host) ||
        !jiajing::copy_text(login.szUserName, config.user) || !jiajing::copy_text(login.szPassword, config.password)) {
        return 2;
    }
    login.dwPort = config.port;
    login.dwLoginProto = NETDEV_LOGIN_PROTO_PRIVATE;
    login.dwDeviceType = config.nvr ? NETDEV_DTYPE_NVR : NETDEV_DTYPE_IPC;
    events.channel = config.channel;
    events.nvr = config.nvr;
    jiajing::install_signals();
    if (!NETDEV_SetExceptionCallBack(exception_message, &events)) {
        return error();
    }
    NETDEV_SELOG_INFO_S info{};
    sdk.user = NETDEV_Login_V30(&login, &info);
    if (sdk.user == nullptr) {
        return error();
    }
    if (!NETDEV_SetAlarmCallBack(sdk.user, alarm_message, &events) ||
        !NETDEV_Subscribe(sdk.user, NETDEV_SUBSCRIBE_ALARM_TYPE_COM)) {
        return error();
    }
    sdk.listening = true;
    events.stream();
    return 0;
}
