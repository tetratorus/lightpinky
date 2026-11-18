#import "AppleBridge.h"
#include <zlib.h>
#include <arpa/inet.h>
#include <sys/socket.h>
#include <unistd.h>
#include <fcntl.h>
#include <poll.h>
#include <netinet/tcp.h>

NSData *LPCompress(NSData *data) {
    uLongf count = compressBound(data.length);
    NSMutableData *out = [NSMutableData dataWithLength:count];
    if (compress2(out.mutableBytes, &count, data.bytes, data.length, 9) != Z_OK) return [NSData data];
    out.length = count;
    return out;
}

int LPMediaSocket(const char *peer, char *host, uint16_t *port) {
    struct sockaddr_in6 address = {.sin6_len=sizeof(address), .sin6_family=AF_INET6, .sin6_port=htons(9)};
    if (inet_pton(AF_INET6,peer,&address.sin6_addr) != 1) return -1;
    int route = socket(AF_INET6,SOCK_DGRAM,0);
    if (route < 0) return -1;
    if (connect(route,(struct sockaddr *)&address,sizeof(address)) < 0) { close(route); return -1; }
    socklen_t length = sizeof(address);
    if (getsockname(route,(struct sockaddr *)&address,&length) < 0) { close(route); return -1; }
    close(route);
    if (!inet_ntop(AF_INET6,&address.sin6_addr,host,INET6_ADDRSTRLEN)) return -1;
    address.sin6_port=0;
    int fd=socket(AF_INET6,SOCK_DGRAM,0);
    if (fd<0) return -1;
    fcntl(fd,F_SETFD,FD_CLOEXEC);
    if (bind(fd,(struct sockaddr *)&address,sizeof(address)) < 0 ||
        getsockname(fd,(struct sockaddr *)&address,&length) < 0) { close(fd); return -1; }
    *port=ntohs(address.sin6_port);
    fcntl(fd,F_SETFL,O_NONBLOCK);
    return fd;
}

void LPMediaSend(int fd, const char *peer, uint16_t port, NSData *data) {
    struct sockaddr_in6 address = {.sin6_len=sizeof(address), .sin6_family=AF_INET6, .sin6_port=htons(port)};
    if (inet_pton(AF_INET6,peer,&address.sin6_addr) == 1)
        sendto(fd,data.bytes,data.length,0,(struct sockaddr *)&address,sizeof(address));
}

int LPConnectTCP(const char *peer, uint16_t port) {
    struct sockaddr_in6 address = {.sin6_len=sizeof(address), .sin6_family=AF_INET6, .sin6_port=htons(port)};
    if (inet_pton(AF_INET6,peer,&address.sin6_addr) != 1) return -1;
    int fd=socket(AF_INET6,SOCK_STREAM,0);
    if (fd<0) return -1;
    fcntl(fd,F_SETFD,FD_CLOEXEC);
    fcntl(fd,F_SETNOSIGPIPE,1);
    int one=1;
    setsockopt(fd,IPPROTO_TCP,TCP_NODELAY,&one,sizeof(one));
    fcntl(fd,F_SETFL,O_NONBLOCK);
    int connected=connect(fd,(struct sockaddr *)&address,sizeof(address));
    if (connected<0) {
        if (errno!=EINPROGRESS) { close(fd); return -1; }
        struct pollfd ready={.fd=fd,.events=POLLOUT};
        if (poll(&ready,1,8000)<=0) { close(fd); return -1; }
        int error=0; socklen_t len=sizeof(error);
        if (getsockopt(fd,SOL_SOCKET,SO_ERROR,&error,&len)<0 || error) { close(fd); errno=error; return -1; }
    }
    fcntl(fd,F_SETFL,0);
    return fd;
}
