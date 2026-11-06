#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

/* Run in a separate dynamically linked process: a Go socket would bypass libc. */
static int ready(int fd, short events) {
    struct pollfd item = {.fd = fd, .events = events};
    return poll(&item, 1, 8000) > 0 ? 0 : -1;
}

static int exchange(int fd) {
    char reply[4];
    size_t sent = 0, received = 0;
    while (sent < 4) {
        if (ready(fd, POLLOUT)) return -1;
        ssize_t n = send(fd, "test" + sent, 4 - sent, MSG_NOSIGNAL);
        if (n < 0 && (errno == EINTR || errno == EAGAIN)) continue;
        if (n <= 0) return -1;
        sent += (size_t)n;
    }
    while (received < 4) {
        if (ready(fd, POLLIN)) return -1;
        ssize_t n = recv(fd, reply + received, 4 - received, 0);
        if (n < 0 && (errno == EINTR || errno == EAGAIN)) continue;
        if (n <= 0) return -1;
        received += (size_t)n;
    }
    return memcmp(reply, "test", 4);
}

int main(int argc, char **argv) {
    if (argc != 5) return 2;
    int family = strchr(argv[1], ':') ? AF_INET6 : AF_INET;
    int fd = socket(family, SOCK_STREAM, 0);
    if (fd < 0) return 3;
    int flags = fcntl(fd, F_GETFL);
    if (atoi(argv[3])) flags |= O_NONBLOCK;
    if (fcntl(fd, F_SETFL, flags) < 0) return 4;
    struct sockaddr_storage storage = {0};
    socklen_t size;
    if (family == AF_INET) {
        struct sockaddr_in *v = (void *)&storage;
        v->sin_family = family;
        v->sin_port = htons((unsigned short)atoi(argv[2]));
        if (inet_pton(family, argv[1], &v->sin_addr) != 1) return 5;
        size = sizeof(*v);
    } else {
        struct sockaddr_in6 *v = (void *)&storage;
        v->sin6_family = family;
        v->sin6_port = htons((unsigned short)atoi(argv[2]));
        if (inet_pton(family, argv[1], &v->sin6_addr) != 1) return 5;
        size = sizeof(*v);
    }
    int result = connect(fd, (struct sockaddr *)&storage, size);
    int error = errno;
    if (fcntl(fd, F_GETFL) != flags) return 6;
    if (strcmp(argv[4], "success")) {
        int expected = !strcmp(argv[4], "unsupported") ? EOPNOTSUPP :
                       !strcmp(argv[4], "invalid") ? EINVAL :
                       !strcmp(argv[4], "timeout") ? ETIMEDOUT :
                       !strcmp(argv[4], "reset") ? ECONNRESET : ECONNREFUSED;
        if (result != -1 || error != expected) {
            fprintf(stderr, "connect result=%d errno=%d expected=%d\n", result, error, expected);
            return 7;
        }
    } else {
        if (result == -1 && error == EINPROGRESS && (flags & O_NONBLOCK)) {
            int pending = 0;
            socklen_t pending_size = sizeof(pending);
            if (ready(fd, POLLOUT) || getsockopt(fd, SOL_SOCKET, SO_ERROR, &pending, &pending_size) || pending) return 10;
            result = 0;
        }
        if (result) { errno = error; perror("connect"); return 8; }
        if (exchange(fd)) return 9;
    }
    close(fd);
    return 0;
}
