#pragma once

#include <array>
#include <atomic>
#include <charconv>
#include <chrono>
#include <csignal>
#include <cstdio>
#include <cstring>
#include <deque>
#include <iostream>
#include <mutex>
#include <string>
#include <thread>
#include <utility>

namespace jiajing {

inline volatile std::sig_atomic_t stop = 0;

inline void interrupt(int) {
    stop = 1;
}

inline void install_signals() {
    std::signal(SIGTERM, interrupt);
    std::signal(SIGINT, interrupt);
}

inline void output(const char* event) {
    std::printf("JIAJING\t1\t%s\n", event);
    std::fflush(stdout);
}

inline int failure(unsigned long code) {
    std::printf("JIAJING\t1\tERROR\t%lu\n", code);
    std::fflush(stdout);
    return 1;
}

inline bool number(const std::string& field, int min, int max, int& out) {
    const char* end = field.data() + field.size();
    const auto parsed = std::from_chars(field.data(), end, out);
    return parsed.ec == std::errc() && parsed.ptr == end && out >= min && out <= max;
}

struct Login {
    std::string host;
    int port = 0;
    std::string user;
    std::string password;
    int channel = 0;
    bool nvr = false;
};

inline bool read_login(Login& login) {
    std::array<std::string, 7> fields;
    for (auto& field : fields) {
        if (!std::getline(std::cin, field) || field.size() > 512 ||
            field.find_first_of("\r\0", 0, 2) != std::string::npos) {
            return false;
        }
    }
    if (fields[0] != "JIAJING/1" || fields[1].empty() || fields[3].empty() ||
        !number(fields[2], 1, 65535, login.port) || !number(fields[5], 1, 4096, login.channel) ||
        (fields[6] != "ipc" && fields[6] != "nvr")) {
        return false;
    }
    login.host = std::move(fields[1]);
    login.user = std::move(fields[3]);
    login.password = std::move(fields[4]);
    login.nvr = fields[6] == "nvr";
    return true;
}

template <typename Byte, size_t N>
bool copy_text(Byte (&dest)[N], const std::string& text) {
    if (text.size() >= N) {
        return false;
    }
    std::memcpy(dest, text.c_str(), text.size() + 1);
    return true;
}

struct Events {
    int channel = 0;
    bool nvr = false;
    std::atomic<bool> disconnected{false};

    void push(const char* event) {
        std::lock_guard<std::mutex> guard(mutex);
        if (queue.size() == 64) {
            disconnected = true;
        } else {
            queue.push_back(event);
        }
    }

    void stream() {
        using namespace std::chrono_literals;
        output("READY");
        auto heartbeat = std::chrono::steady_clock::now() + 3s;
        while (!stop) {
            if (disconnected) {
                output("DISCONNECTED");
                break;
            }
            std::deque<const char*> pending;
            {
                std::lock_guard<std::mutex> guard(mutex);
                pending.swap(queue);
            }
            for (const char* event : pending) {
                output(event);
            }
            const auto now = std::chrono::steady_clock::now();
            if (now >= heartbeat) {
                output("HEARTBEAT");
                heartbeat = now + 3s;
            }
            std::this_thread::sleep_for(100ms);
        }
    }

private:
    std::mutex mutex;
    std::deque<const char*> queue;
};

}  // namespace jiajing
