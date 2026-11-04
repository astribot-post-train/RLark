#define _GNU_SOURCE
#include <arpa/inet.h>
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <pthread.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

/* Preview: synchronous, bounded connect interception for explicit remote peers.
 * Only the explicitly injected process and its dynamically linked children use
 * this shim. No socket, routing table, proxy variable or network namespace is
 * modified outside that process tree. */
static int (*next_connect)(int, const struct sockaddr *, socklen_t);
static pthread_once_t once = PTHREAD_ONCE_INIT;
enum { MAX_REMOTE_IPS = 64 };
static struct in_addr remote_ips[MAX_REMOTE_IPS];
static int remote_count;
static int port_min, port_max, connector_port, configured;

static int parse_port(const char *value, int *port) {
    unsigned int parsed = 0;
    if (!value || !*value) return 0;
    for (const char *p = value; *p; p++) {
        if (*p < '0' || *p > '9') return 0;
        parsed = parsed * 10 + (unsigned int)(*p - '0');
        if (parsed > 65535) return 0;
    }
    if (!parsed) return 0;
    *port = (int)parsed;
    return 1;
}

static void setup(void) {
    next_connect = dlsym(RTLD_NEXT, "connect");
    const char *ip = getenv("RLARK_MUX_REMOTE_IP");
    const char *ips = getenv("RLARK_MUX_REMOTE_IPS");
    const char *lo = getenv("RLARK_MUX_PORT_MIN");
    const char *hi = getenv("RLARK_MUX_PORT_MAX");
    const char *cp = getenv("RLARK_MUX_LOCAL_PORT");
    if (!ip && !ips && !lo && !hi && !cp) return;
    configured = -1;
    if ((!ip && !ips) || (ip && ips) || !parse_port(lo, &port_min) ||
        !parse_port(hi, &port_max) || !parse_port(cp, &connector_port) || port_max < port_min) return;
    if (ip && strchr(ip, ',')) return;
    const char *start = ip ? ip : ips;
    while (*start) {
        const char *end = strchr(start, ',');
        size_t len = end ? (size_t)(end - start) : strlen(start);
        char address[INET_ADDRSTRLEN];
        if (!len || len >= sizeof(address) || remote_count == MAX_REMOTE_IPS) return;
        memcpy(address, start, len);
        address[len] = '\0';
        struct in_addr parsed;
        if (inet_pton(AF_INET, address, &parsed) != 1 || !parsed.s_addr) return;
        for (int i = 0; i < remote_count; i++)
            if (remote_ips[i].s_addr == parsed.s_addr) return;
        remote_ips[remote_count++] = parsed;
        if (!end) break;
        start = end + 1;
        if (!*start) return;
    }
    if (remote_count) configured = 1;
}

static int64_t monotonic_ms(void) {
    struct timespec now;
    if (clock_gettime(CLOCK_MONOTONIC, &now) < 0) return -1;
    return (int64_t)now.tv_sec * 1000 + now.tv_nsec / 1000000;
}

static int wait_fd(int fd, short events, int64_t deadline) {
    for (;;) {
        int64_t now = monotonic_ms();
        if (now < 0) return -1;
        int64_t remaining = deadline - now;
        if (remaining <= 0) { errno = ETIMEDOUT; return -1; }
        struct pollfd item = {.fd = fd, .events = events};
        int result = poll(&item, 1, (int)remaining);
        if (result < 0 && errno == EINTR) continue;
        if (result <= 0) { if (!result) errno = ETIMEDOUT; return -1; }
        if (item.revents & POLLNVAL) { errno = EBADF; return -1; }
        return 0;
    }
}

static int transfer(int fd, void *buf, size_t count, int sending, int64_t deadline) {
    char *p = buf;
    while (count) {
        ssize_t n = sending ? send(fd, p, count, MSG_NOSIGNAL) : recv(fd, p, count, 0);
        if (n < 0 && errno == EINTR) continue;
        if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
            if (wait_fd(fd, sending ? POLLOUT : POLLIN, deadline) < 0) return -1;
            continue;
        }
        if (n <= 0) { if (!n) errno = ECONNRESET; return -1; }
        p += n;
        count -= (size_t)n;
    }
    return 0;
}

int connect(int fd, const struct sockaddr *addr, socklen_t len) {
    int entry_errno = errno;
    pthread_once(&once, setup);
    if (!next_connect) { errno = ENOSYS; return -1; }
    if (configured < 0) { errno = EINVAL; return -1; }
    uint32_t ip = 0;
    uint16_t netport = 0;
    int family = addr && len >= sizeof(sa_family_t) ? addr->sa_family : AF_UNSPEC;
    if (configured && family == AF_INET && len >= sizeof(struct sockaddr_in)) {
        const struct sockaddr_in *v = (const void *)addr;
        ip = v->sin_addr.s_addr;
        netport = v->sin_port;
    } else if (configured && family == AF_INET6 && len >= sizeof(struct sockaddr_in6)) {
        const struct sockaddr_in6 *v = (const void *)addr;
        if (IN6_IS_ADDR_V4MAPPED(&v->sin6_addr)) {
            memcpy(&ip, v->sin6_addr.s6_addr + 12, 4);
            netport = v->sin6_port;
        }
    }
    int port = ntohs(netport);
    int matches = 0;
    for (int i = 0; i < remote_count; i++)
        if (ip == remote_ips[i].s_addr) { matches = 1; break; }
    if (!configured || !matches || port < port_min || port > port_max) {
        errno = entry_errno;
        return next_connect(fd, addr, len);
    }
    int kind = 0;
    socklen_t kind_len = sizeof(kind);
    if (getsockopt(fd, SOL_SOCKET, SO_TYPE, &kind, &kind_len) < 0) return -1;
    if (kind != SOCK_STREAM) {
        errno = entry_errno;
        return next_connect(fd, addr, len);
    }

    struct sockaddr_storage connector = {0};
    socklen_t connector_len;
    if (family == AF_INET6) {
        struct sockaddr_in6 *v = (void *)&connector;
        v->sin6_family = AF_INET6;
        v->sin6_port = htons((uint16_t)connector_port);
        if (inet_pton(AF_INET6, "::ffff:127.0.0.1", &v->sin6_addr) != 1) {
            errno = EINVAL;
            return -1;
        }
        connector_len = sizeof(*v);
    } else {
        struct sockaddr_in *v = (void *)&connector;
        v->sin_family = AF_INET;
        v->sin_port = htons((uint16_t)connector_port);
        v->sin_addr.s_addr = htonl(INADDR_LOOPBACK);
        connector_len = sizeof(*v);
    }
    int flags = fcntl(fd, F_GETFL);
    if (flags < 0) return -1;
    int64_t now = monotonic_ms();
    if (now < 0) return -1;
    int64_t deadline = now + 5000;
    /* The control connection is separate from the application's descriptor.
     * This keeps O_NONBLOCK/EINPROGRESS semantics intact on the descriptor
     * that Ray owns, while the short setup exchange remains bounded. */
    int control = socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC | SOCK_NONBLOCK, 0);
    if (control < 0) return -1;
    struct sockaddr_in control_address = {0};
    control_address.sin_family = AF_INET;
    control_address.sin_port = htons((uint16_t)connector_port);
    control_address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    int result = next_connect(control, (const struct sockaddr *)&control_address, sizeof(control_address));
    if (result < 0 && errno == EINPROGRESS && wait_fd(control, POLLOUT, deadline) == 0) {
        int error = 0;
        socklen_t size = sizeof(error);
        result = getsockopt(control, SOL_SOCKET, SO_ERROR, &error, &size);
        if (result == 0 && error) { errno = error; result = -1; }
    }
    if (result == 0) {
        unsigned char header[6];
        memcpy(header, &ip, 4);
        memcpy(header + 4, &netport, 2);
        unsigned char response[3] = {1, 0, 0};
        if (transfer(control, header, sizeof(header), 1, deadline) < 0 ||
            transfer(control, response, 1, 0, deadline) < 0) {
            result = -1;
        } else if (response[0] != 0) {
            errno = ECONNREFUSED;
            result = -1;
        } else if (transfer(control, response + 1, 2, 0, deadline) < 0) {
            result = -1;
        } else {
            uint16_t local_port;
            memcpy(&local_port, response + 1, 2);
            if (!local_port) {
                close(control);
                errno = EPROTO;
                return -1;
            }
            if (family == AF_INET6)
                ((struct sockaddr_in6 *)&connector)->sin6_port = local_port;
            else
                ((struct sockaddr_in *)&connector)->sin_port = local_port;
            /* The original call below is deliberately made with the caller's
             * flags. A nonblocking caller receives EINPROGRESS and completes
             * through its normal poll/SO_ERROR path. */
            result = next_connect(fd, (const struct sockaddr *)&connector, connector_len);
        }
    }
    int saved = result == 0 ? entry_errno : errno;
    close(control);
    errno = saved;
    return result;
}
