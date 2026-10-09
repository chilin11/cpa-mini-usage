#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <dlfcn.h>
#include <unistd.h>

typedef struct { void* ptr; size_t len; } buffer;
typedef int (*host_cb)(void*,const char*,const uint8_t*,size_t,buffer*);
typedef void (*host_free)(void*,size_t);
typedef struct {uint32_t abi_version;void* host_ctx;host_cb call;host_free free_buffer;} host_api;
typedef int (*plugin_cb)(char*,uint8_t*,size_t,buffer*);
typedef void (*plugin_free)(void*,size_t);
typedef void (*plugin_shutdown)(void);
typedef struct {uint32_t abi_version;plugin_cb call;plugin_free free_buffer;plugin_shutdown shutdown;} plugin_api;
static int calls=0;
static int hostcall(void* ctx, const char* name, const uint8_t* body, size_t n, buffer* out){
 (void)ctx;(void)body;(void)n;
 const char* result="{\"ok\":true,\"result\":{}}";
 if(!strcmp(name,"host.auth.list")) result="{\"ok\":true,\"result\":{\"files\":[{\"auth_index\":\"idx\",\"id\":\"id123\",\"provider\":\"codex\",\"name\":\"demo\"}]}}";
 if(!strcmp(name,"host.auth.get")) result="{\"ok\":true,\"result\":{\"Content\":\"{\\\"access_token\\\":\\\"test_token\\\",\\\"chatgpt_account_id\\\":\\\"test_account\\\"}\"}}";
 if(!strcmp(name,"host.http.do")) result="{\"ok\":true,\"result\":{\"StatusCode\":200,\"Body\":\"eyJyYXRlX2xpbWl0Ijp7InByaW1hcnlfd2luZG93Ijp7InVzZWRfcGVyY2VudCI6MzUsImxpbWl0X3dpbmRvd19zZWNvbmRzIjoxODAwMCwicmVzZXRfYWZ0ZXJfc2Vjb25kcyI6MzYwMH0sInNlY29uZGFyeV93aW5kb3ciOnsidXNlZF9wZXJjZW50Ijo1MCwibGltaXRfd2luZG93X3NlY29uZHMiOjYwNDgwMCwicmVzZXRfYWZ0ZXJfc2Vjb25kcyI6ODY0MDB9fX0=\"}}";
 out->len=strlen(result);out->ptr=malloc(out->len);memcpy(out->ptr,result,out->len);calls++;return 0;
}
static void hfree(void* p,size_t n){(void)n;free(p);}
static int invoke(plugin_api* api,char* name,const char* json){
 buffer result={0};int rc=api->call(name,(uint8_t*)json,strlen(json),&result);
 if(rc || !result.ptr){fprintf(stderr,"RPC fail %s\n",name);return 1;}
 char* text=calloc(result.len+1,1);memcpy(text,result.ptr,result.len);
 int bad=!strstr(text,"\"ok\":true");
 if(!strcmp(name,"management.handle") && !strstr(text,"\"StatusCode\":200")) bad=1;
 if(bad) fprintf(stderr,"Bad %s: %s\n",name,text);
 else printf("PASS %-23s  bytes=%zu\n",name,result.len);
 free(text);api->free_buffer(result.ptr,result.len);return bad;
}
int main(int argc,char** argv){
 if(argc!=2){fprintf(stderr,"usage: abi_smoke lib.so\n");return 2;}
 setenv("CPA_MINI_USAGE_DATA_DIR","/tmp/cpa-mini-usage-smoke",1);
 void* lib=dlopen(argv[1],RTLD_NOW|RTLD_LOCAL);if(!lib){fprintf(stderr,"%s\n",dlerror());return 1;}
 int (*init)(host_api*,plugin_api*)=dlsym(lib,"cliproxy_plugin_init");if(!init){fprintf(stderr,"missing ABI entrypoint\n");return 1;}
 host_api h={1,NULL,hostcall,hfree};plugin_api p={0};if(init(&h,&p)!=0||p.abi_version!=1||!p.call)return 1;
 if(invoke(&p,"plugin.register","{}"))return 1;
 if(invoke(&p,"management.register","{}"))return 1;
 if(invoke(&p,"usage.handle","{\"Provider\":\"codex\",\"AuthID\":\"id123\",\"Model\":\"gpt-5.5\",\"Detail\":{\"InputTokens\":500,\"OutputTokens\":50,\"TotalTokens\":550}}"))return 1;
 sleep(4);
 if(invoke(&p,"management.handle","{\"Method\":\"GET\",\"Path\":\"/v0/management/plugins/cpa-mini-usage/data\"}"))return 1;
 if(invoke(&p,"management.handle","{\"Method\":\"GET\",\"Path\":\"/v0/resource/plugins/cpa-mini-usage/dashboard\"}"))return 1;
 p.shutdown();printf("Host callback calls: %d\n",calls);dlclose(lib);
 return 0;
}
