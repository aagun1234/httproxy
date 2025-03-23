used for dify to access ctyun api, change stream to false from true

./httproxy  -listen :8081 -target https://wishub-x1.ctyun.cn -req-body "\"stream\":?true::\"stream\": false" -resp-body "\"message\"::\"delta\""
