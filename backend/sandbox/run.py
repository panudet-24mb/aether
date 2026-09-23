#!/usr/bin/env python3
# No Python callables, OS, network, filesystem or timers are exposed to JavaScript.
import json, sys, resource
sys.path.insert(0, '/app/python')
import quickjs
resource.setrlimit(resource.RLIMIT_CPU,(1,1))
resource.setrlimit(resource.RLIMIT_AS,(192*1024*1024,192*1024*1024))
try:
 raw=sys.stdin.buffer.read(100001)
 if len(raw)>100000:raise ValueError()
 req=json.loads(raw)
 if req['function'] not in ('decodeUplink','render','decodeHistory'):raise ValueError()
 ctx=quickjs.Context();ctx.set_memory_limit(16*1024*1024);ctx.set_max_stack_size(256*1024);ctx.set_time_limit(0.2)
 # A fresh context per request; no host functions, std/os modules or dynamic imports.
 code='"use strict";\n'+req['code']+'\nJSON.stringify('+req['function']+'('+json.dumps(req['input'],ensure_ascii=True)+'))'
 if req['function']=='decodeHistory':
  if not isinstance(req['input'],list) or len(req['input'])>200:raise ValueError()
  code='"use strict";\n'+req['code']+'\nJSON.stringify({samples:('+json.dumps(req['input'],ensure_ascii=True)+').map(function(raw){try{return decodeUplink({bytes:raw.match(/.{2}/g).map(x=>parseInt(x,16)),fPort:0});}catch(e){return null;}})})'
 value=ctx.eval(code)
 if not isinstance(value,str) or len(value)>65536:raise ValueError()
 result=json.loads(value)
 if not isinstance(result,dict):raise ValueError()
 print(json.dumps({'result':result},allow_nan=False))
except BaseException:
 print('{"error":"javascript_failed_or_limit_exceeded"}')
 sys.exit(1)
