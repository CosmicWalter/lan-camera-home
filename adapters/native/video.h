#pragma once

#include "common.h"
#include <condition_variable>
#include <vector>
#include <unistd.h>

namespace jiajing {

enum class Mode { invalid, events, self_test, main_stream, sub_stream };

inline Mode mode(int argc, char** argv) {
    if (argc == 1) {
        return Mode::events;
    }
    if (argc == 2) {
        if (std::strcmp(argv[1], "--self-test") == 0) return Mode::self_test;
        if (std::strcmp(argv[1], "--stream-main") == 0) return Mode::main_stream;
        if (std::strcmp(argv[1], "--stream-sub") == 0) return Mode::sub_stream;
    }
    return Mode::invalid;
}

inline bool video_mode(Mode kind) {
    return kind == Mode::main_stream || kind == Mode::sub_stream;
}

class VideoOutput {
public:
    bool open() {
        // Give the video its own descriptor so a vendor library's stdout
        // output cannot corrupt the media. SDK errors still reach stderr.
        const int video = dup(STDOUT_FILENO);
        if (video < 0) {
            return false;
        }
        if (dup2(STDERR_FILENO, STDOUT_FILENO) < 0) {
            close(video);
            return false;
        }
        file = fdopen(video, "wb");
        if (file == nullptr) {
            close(video);
            return false;
        }
        std::signal(SIGPIPE, SIG_IGN);
        return true;
    }

    ~VideoOutput() {
        if (file != nullptr) std::fclose(file);
    }

    FILE* file = nullptr;
};

class VideoBuffer {
public:
    void push(const void* data, size_t size) {
        if (data == nullptr || size == 0) {
            return;
        }
        std::lock_guard<std::mutex> lock(mutex);
        if (overflow || queue.size() == 1024 || size > max_bytes - queued_bytes) {
            overflow = true;
            ready.notify_one();
            return;
        }
        const auto* begin = static_cast<const unsigned char*>(data);
        queue.emplace_back(begin, begin + size);
        queued_bytes += size;
        ready.notify_one();
    }

    int write(FILE* file, const char* format, const std::atomic<bool>& disconnected) {
        using namespace std::chrono_literals;
        if (std::fprintf(file, "JIAJING-VIDEO/1 %s\n", format) < 0 || std::fflush(file) != 0) {
            return 1;
        }
        while (!stop && !disconnected) {
            std::vector<unsigned char> data;
            {
                std::unique_lock<std::mutex> lock(mutex);
                ready.wait_for(lock, 200ms, [this] { return overflow || !queue.empty(); });
                if (overflow) {
                    return 1;
                }
                if (queue.empty()) {
                    continue;
                }
                data = std::move(queue.front());
                queue.pop_front();
                queued_bytes -= data.size();
            }
            if (std::fwrite(data.data(), 1, data.size(), file) != data.size() || std::fflush(file) != 0) {
                return 1;
            }
        }
        return 0;
    }

private:
    static constexpr size_t max_bytes = 8 << 20;
    size_t queued_bytes = 0;
    bool overflow = false;
    std::mutex mutex;
    std::condition_variable ready;
    std::deque<std::vector<unsigned char>> queue;
};

}  // namespace jiajing
