#import "AppleBridge.h"
#import <dlfcn.h>

static void *remoteSymbol(const char *name) {
    static void *library;
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        library = dlopen("/System/Library/PrivateFrameworks/RemoteXPC.framework/RemoteXPC", RTLD_NOW);
    });
    return dlsym(library, name);
}

void LPRemoteActivate(id connection, void (^handler)(xpc_object_t)) {
    ((void (*)(id, id))remoteSymbol("xpc_remote_connection_set_event_handler"))(connection, handler);
    ((void (*)(id))remoteSymbol("xpc_remote_connection_activate"))(connection);
}
void LPRemoteSend(id connection, xpc_object_t message) {
    ((void (*)(id, xpc_object_t))remoteSymbol("xpc_remote_connection_send_message"))(connection, message);
}
void LPRemoteCancel(id connection) {
    ((void (*)(id))remoteSymbol("xpc_remote_connection_cancel"))(connection);
}
void LPRemoteRequest(id connection, xpc_object_t message, dispatch_queue_t queue, void (^reply)(xpc_object_t)) {
    ((void (*)(id, xpc_object_t, dispatch_queue_t, id))remoteSymbol("xpc_remote_connection_send_message_with_reply"))(connection, message, queue, reply);
}
