#import <Foundation/Foundation.h>
#import <xpc/xpc.h>

NS_ASSUME_NONNULL_BEGIN
void LPRemoteActivate(id connection, void (^handler)(xpc_object_t));
void LPRemoteSend(id connection, xpc_object_t message);
void LPRemoteRequest(id connection, xpc_object_t message, dispatch_queue_t queue, void (^reply)(xpc_object_t));
void LPRemoteCancel(id connection);
int LPConnectTCP(const char *peer, uint16_t port);
NSData * _Nullable LPAXRequestData(NSUInteger type, NSDictionary * _Nullable object, NSDictionary * _Nullable parameters, NSError **error);
NSDictionary * _Nullable LPAXDecode(NSData *data, NSError **error);
NSData *LPCompress(NSData *data);
int LPMediaSocket(const char *peer, char *host, uint16_t *port);
void LPMediaSend(int fd, const char *peer, uint16_t port, NSData *data);
NS_ASSUME_NONNULL_END
