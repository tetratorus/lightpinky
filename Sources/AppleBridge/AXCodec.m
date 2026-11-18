#import "AppleBridge.h"
#import <dlfcn.h>

@interface NSObject (LPAXCodec)
+ (id)requestWithTranslation:(id)object;
+ (NSSet *)allowedDecodableClasses;
- (void)setRequestType:(NSUInteger)type;
- (void)setClientType:(NSUInteger)type;
- (void)setParameters:(NSDictionary *)parameters;
- (id)resultData;
- (id)associatedTranslationObject;
- (NSUInteger)associatedRequestType;
- (NSUInteger)objectID;
- (int)pid;
@end

static void LoadAX(void) {
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        dlopen("/System/Library/PrivateFrameworks/AccessibilityPlatformTranslation.framework/AccessibilityPlatformTranslation",RTLD_NOW|RTLD_GLOBAL);
    });
}
static NSError *AXError(NSString *message) {
    return [NSError errorWithDomain:@"lightpinky.accessibility" code:1
        userInfo:@{NSLocalizedDescriptionKey:message}];
}

static id AXJSON(id object, unsigned depth) {
    if (!object || depth>60) return NSNull.null;
    if ([object isKindOfClass:NSString.class] || [object isKindOfClass:NSNumber.class]) return object;
    if ([object isKindOfClass:NSAttributedString.class]) return [object string];
    if ([object isKindOfClass:NSDictionary.class]) {
        NSMutableDictionary *result=[NSMutableDictionary dictionary];
        for (id key in object) result[[key description]]=AXJSON(object[key],depth+1);
        return result;
    }
    if ([object isKindOfClass:NSArray.class]) {
        NSMutableArray *result=[NSMutableArray array];
        for (id item in object) [result addObject:AXJSON(item,depth+1)];
        return result;
    }
    if ([object isKindOfClass:NSClassFromString(@"AXPTranslatorResponse")]) {
        return @{@"type":@([object associatedRequestType]),
            @"notification":[object valueForKey:@"notification"] ?: @0,
            @"error":[object valueForKey:@"error"] ?: @0,
            @"object":AXJSON([object associatedTranslationObject],depth+1),
            @"result":AXJSON([object resultData],depth+1)};
    }
    if ([object isKindOfClass:NSClassFromString(@"AXPTranslationObject")]) {
        return @{@"pid":@([object pid]),@"id":@([object objectID]),
                 @"isApp":[object valueForKey:@"isApplicationElement"] ?: @NO};
    }
    if ([object isKindOfClass:NSValue.class] && strcmp([object objCType],@encode(CGRect))==0) {
        CGRect rect=[object rectValue];
        return @{@"x":@(rect.origin.x),@"y":@(rect.origin.y),@"width":@(rect.size.width),@"height":@(rect.size.height)};
    }
    return [object description];
}

NSData *LPAXRequestData(NSUInteger type, NSDictionary *object, NSDictionary *parameters, NSError **error) {
    LoadAX();
    @try {
        Class requestClass=NSClassFromString(@"AXPTranslatorRequest");
        if (!requestClass) { if (error) *error=AXError(@"Apple accessibility codecs are unavailable"); return nil; }
        id translation=nil;
        if (object) {
            translation=[NSClassFromString(@"AXPTranslationObject") new];
            [translation setValue:object[@"pid"] forKey:@"pid"];
            [translation setValue:object[@"id"] forKey:@"objectID"];
            [translation setValue:object[@"isApp"] ?: @NO forKey:@"isApplicationElement"];
        }
        id request=[requestClass requestWithTranslation:translation];
        [request setRequestType:type];
        [request setClientType:2];
        [request setParameters:parameters];
        return [NSKeyedArchiver archivedDataWithRootObject:request requiringSecureCoding:YES error:error];
    } @catch (NSException *exception) {
        if (error) *error=AXError(exception.reason ?: exception.name);
        return nil;
    }
}

NSDictionary *LPAXDecode(NSData *data, NSError **error) {
    LoadAX();
    @try {
        Class responseClass=NSClassFromString(@"AXPTranslatorResponse");
        if (!responseClass) { if (error) *error=AXError(@"Apple accessibility codecs are unavailable"); return nil; }
        NSMutableSet *classes=[[responseClass allowedDecodableClasses] mutableCopy];
        [classes addObject:responseClass];
        // App-change notifications contain the application's bundle URL.
        [classes addObject:NSURL.class];
        id response=[NSKeyedUnarchiver unarchivedObjectOfClasses:classes fromData:data error:error];
        if (![response isKindOfClass:responseClass]) {
            if (error && !*error) *error=AXError(@"Unexpected accessibility response");
            return nil;
        }
        return AXJSON(response,0);
    } @catch (NSException *exception) {
        if (error) *error=AXError(exception.reason ?: exception.name);
        return nil;
    }
}
