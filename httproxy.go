package main

import (
    "bytes"
    "flag"
    "fmt"
    "io"
    "log"
    "net/http"
    "net/http/httputil"
    "net/url"
    "os"
    "regexp"
    "strconv"
    "strings"
	"encoding/hex"
)

type Config struct {
    listenPort string
    targetURL  string
    reqHeader  []string
    reqBody    []string
    respHeader []string
    respBody   []string
}

type RegexReplacement struct {
	id	int
    Pattern     *regexp.Regexp
    Replacement string
    Count       int // -1 表示替换所有匹配项
    HeaderKey   string
}

var (
    config           Config
    reqHeaderRules   []RegexReplacement
    reqBodyRules     []RegexReplacement
    respHeaderRules  []RegexReplacement
    respBodyRules    []RegexReplacement

	DEBUG	*int

)

func customUsage() {
    fmt.Fprintf(os.Stderr, "\n")
    fmt.Fprintf(os.Stderr, "简单HTTP反向代理\n用法：%s [参数...]\n", os.Args[0])
    flag.PrintDefaults()
    fmt.Fprintf(os.Stderr, "\n示例:\n  %s -listen :8080 -target https://wishub-x1.ctyun.cn -req-body \"\\\"stream\\\":?true::\\\"stream\\\": false\" -resp-body \"\\\"message\\\"::\\\"delta\\\"\"\n", os.Args[0])
    fmt.Fprintf(os.Stderr, "  %s -listen :8080 -target https://wishub-x1.ctyun.cn -resp-body \"data:{::data: {\"\n", os.Args[0])
}

func main() {
    flag.Usage = customUsage
	DEBUG = flag.Int("debug", 1, "输出调试信息等级")
    flag.StringVar(&config.listenPort, "listen", ":8080", "本地HTTP侦听端口 (e.g., :8080)")
    flag.StringVar(&config.targetURL, "target", "", "反向代理目标 (e.g., https://wishub-x1.ctyun.cn)")
    flag.Func("req-header", "请求头替换规则 (key:pattern:replacement)", func(s string) error {
        config.reqHeader = append(config.reqHeader, s)
        return nil
    })
    flag.Func("req-body", "请求体替换规则 (pattern::replacement)", func(s string) error {
        config.reqBody = append(config.reqBody, s)
        return nil
    })
    flag.Func("resp-header", "应答头替换规则 (key::pattern::replacement)", func(s string) error {
        config.respHeader = append(config.respHeader, s)
        return nil
    })
    flag.Func("resp-body", "应答体替换规则 (pattern::replacement)", func(s string) error {
        config.respBody = append(config.respBody, s)
        return nil
    })

    flag.Parse()

    if config.targetURL == "" {
        log.Fatal("反向代理目标不能为空")
    }

    // 解析目标URL
    target, err := url.Parse(config.targetURL)
    if err != nil {
        log.Fatalf("目标URL格式不对: %v", err)
    }

    // 解析替换规则
    parseRules(config.reqHeader, &reqHeaderRules, true)
    parseRules(config.reqBody, &reqBodyRules, false)
    parseRules(config.respHeader, &respHeaderRules, true)
    parseRules(config.respBody, &respBodyRules, false)

	if *DEBUG>=3 { 
		fmt.Printf("DEBUG req-header : %v\n",reqHeaderRules)
		fmt.Printf("DEBUG req-body : %v\n",reqBodyRules)
		fmt.Printf("DEBUG resp-header : %v\n",respHeaderRules)
		fmt.Printf("DEBUG resp-body : %v\n",respBodyRules)
	}

    // 创建反向代理
    proxy := &httputil.ReverseProxy{
        Director: func(req *http.Request) {

            if *DEBUG>=2 {
                log.Println("=== 收到的请求头 ===")
				fmt.Printf("PROTO: %s\n", req.Proto)
				fmt.Printf("METHOD: %s\n", req.Method)
				fmt.Printf("HOST: %s\n", req.Host)
				fmt.Printf("URI: %s\n", req.RequestURI)
                req.Header.Write(os.Stdout)
                fmt.Println()
            }
            req.URL.Scheme = target.Scheme
            req.URL.Host = target.Host
            req.Host = target.Host

            if len(reqHeaderRules) > 0 {
                modifyHeaders(req.Header, reqHeaderRules)
                if *DEBUG>=2 {
                    fmt.Println("=== 修改后的请求头 ===")
					fmt.Printf("PROTO: %s\n", req.Proto)
					fmt.Printf("METHOD: %s\n", req.Method)
					fmt.Printf("HOST: %s\n", req.Host)
					fmt.Printf("URI: %s\n", req.RequestURI)
                   req.Header.Write(os.Stdout)
                    fmt.Println()
                }
            } else {
                if *DEBUG>=2 {
                    fmt.Println("=== 请求头无需修改 ===")
                    fmt.Println()
                }
            }
			
        },
        ModifyResponse: func(resp *http.Response) error {

            // 如果是流式响应，使用自定义 Body 处理
            if resp.Header.Get("Transfer-Encoding") == "chunked" || strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
                resp.Body = &streamingBody{
                    reader:    resp.Body,
					debug:	*DEBUG,
                    rules:     respBodyRules,
                    origBody:  new(bytes.Buffer),
                    totalSize: 0,

                }
            } else {
                // 非流式响应，整体修改
                body, err := io.ReadAll(resp.Body)
                if err != nil {
                    return err
                }
                resp.Body.Close()

                if *DEBUG>=2 {
                    log.Println("=== 收到的应答体 ===")
                    body1, _ := replaceStrBytes(body, "\n\n", "**\n\n")
                    fmt.Println(string(body1))
                    fmt.Println("=========")
                }

                modifiedBody := body
                if len(respBodyRules) > 0 {
                    modifiedBody = modifyContent(body, respBodyRules)
                    if *DEBUG>=2 {
                        fmt.Println("=== 修改后的应答体 ===")
                        fmt.Println(string(modifiedBody))
                        fmt.Println("=========")
                    }
                } else {
                    if *DEBUG>=2 {
                        fmt.Println("=== 应答体无需修改 ===")
                        fmt.Println()
                    }
                }

                resp.Body = io.NopCloser(bytes.NewReader(modifiedBody))
                resp.ContentLength = int64(len(modifiedBody))
            }
			
            if *DEBUG>=2 {
                log.Println("=== 收到的应答头 ===")
				fmt.Printf("STATUS: %s\n", resp.Status)
				fmt.Printf("PROTO: %s\n", resp.Proto)
                resp.Header.Write(os.Stdout)
                fmt.Println("=========")
            }
            if len(respHeaderRules) > 0 {
                modifyHeaders(resp.Header, respHeaderRules)
                if *DEBUG>=2 {
                    fmt.Println("=== 修改后的应答头 ===")
                    resp.Header.Write(os.Stdout)
                    fmt.Println("=========")
                }
            } else {
                if *DEBUG>=2 {
                    fmt.Println("=== 应答头无需修改 ===")
                    fmt.Println()
                }
            }
			
            return nil
        },
        Transport: &http.Transport{
            Proxy: http.ProxyFromEnvironment,
        },
        FlushInterval: -1, // 禁用缓冲，确保流式转发
    }

    handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        body, err := io.ReadAll(r.Body)
        if err != nil {
            http.Error(w, "请求体出错", http.StatusInternalServerError)
            return
        }
        r.Body.Close()
        if *DEBUG>=2 {
            log.Println("=== 收到的请求体 ===")
            fmt.Println(string(body))
            fmt.Println("=========")
			if *DEBUG>=3 { fmt.Println(hexDump(body)) }
        }

        modifiedBody := body
        if len(reqBodyRules) > 0 {
            modifiedBody = modifyContent(body, reqBodyRules)
            if *DEBUG>=2 {
                fmt.Println("=== 修改后的请求体 ===")
                fmt.Println(string(modifiedBody))
                fmt.Println("=========")
 				if *DEBUG>=3 { fmt.Println(hexDump(modifiedBody))}
           }
        } else {
            if *DEBUG>=2 {
                fmt.Println("=== 请求体无需修改 ===")
                fmt.Println()
            }
        }
        r.Body = io.NopCloser(bytes.NewReader(modifiedBody))
        r.ContentLength = int64(len(modifiedBody))

        // 如果客户端支持流式响应，确保实时转发
        flusher, ok := w.(http.Flusher)
        if ok {
            w.Header().Set("Transfer-Encoding", "chunked")
			w.Header().Set("Content-Type:", "text/event-stream")
            w.Header().Del("Content-Length") // 移除 Content-Length 以支持 chunked
			//w.Header().Del("Transfer-Encoding")
        }
        proxy.ServeHTTP(&streamingResponseWriter{
            ResponseWriter: w,
            flusher:        flusher,
			debug:	*DEBUG,
			
        }, r)
    })

    log.Printf("HTTP侦听端口 %s, 转发到 %s", config.listenPort, config.targetURL)
    log.Fatal(http.ListenAndServe(config.listenPort, handler))
}

// streamingBody 自定义 Body，用于流式读取、修改和输出
type streamingBody struct {
    reader    io.ReadCloser
	debug		int
    rules     []RegexReplacement
    origBody  *bytes.Buffer // 记录原始完整内容
    totalSize int           // 记录总大小
}

func hexDump(data []byte) string {
	return hex.Dump(data)
}


func (s *streamingBody) Read(p []byte) (n int, err error) {
    n, err = s.reader.Read(p)
    if n > 0 {
        s.origBody.Write(p[:n]) // 记录原始内容
        s.totalSize += n

        if s.debug>=2 {
            log.Printf("=== 从远端收到的流式应答内容 (chunk 大小: %d 字节) ===\n", n)
            fmt.Println(string(p[:n]))
			fmt.Println("=========")
			if s.debug>=3 { fmt.Println(hexDump(p[:n])) }
        }

        // 修改 chunk 内容
		if len(s.rules)>0 {
			modifiedChunk := modifyContent(p[:n], s.rules)

			copy(p, modifiedChunk)
            if s.debug>=2 {
				fmt.Println("=== 修改后的应答体 ===")
				fmt.Println(string(modifiedChunk))
				fmt.Println("=========")
				if s.debug>=3 { fmt.Println(hexDump(modifiedChunk)) }
			}
			n=len(modifiedChunk)
        } else {
			if s.debug>=2 {
				fmt.Println("=== 应答体无需修改 ===")
				fmt.Println()
			}
		}
    }
    return n, err
}

func (s *streamingBody) Close() error {
    if s.debug>=1 {
        fmt.Println("=== 流式应答接收完成，总大小: ", s.totalSize, "字节 ===\n")
    }
    return s.reader.Close()
}

// streamingResponseWriter 自定义 ResponseWriter，用于流式转发
type streamingResponseWriter struct {
    http.ResponseWriter
    flusher http.Flusher
	debug 	int
}

func (w *streamingResponseWriter) Write(b []byte) (int, error) {
    n, err := w.ResponseWriter.Write(b)
    if n > 0 && w.flusher != nil {
        w.flusher.Flush() // 实时推送给客户端
        if w.debug>=1 {
            log.Printf("=== 向客户端发送应答内容 (%d 字节) ===\n", n)
        }
    }
    return n, err
}

func (w *streamingResponseWriter) WriteHeader(statusCode int) {
    w.ResponseWriter.WriteHeader(statusCode)
}

// 解析替换规则，支持通配符和替换次数
func parseRules(rules []string, target *[]RegexReplacement, isHeader bool) {
    for i, rule := range rules {
        parts := strings.SplitN(rule, "::", 4)
        var pattern, replacement, headerKey string
        var count int = -1

        if isHeader {
            if len(parts) < 3 {
                log.Printf("头部改写规则格式不对: %s (例 key:pattern:replacement)", rule)
                continue
            }
            headerKey = parts[0]
            pattern = parts[1]
            replacement = parts[2]
            if len(parts) == 4 && parts[3] != "" {
				var err error
                count, err = strconv.Atoi(parts[3])
                if err != nil {
                    log.Printf("替换次数字段格式不对 %s: %v", rule, err)
                    continue
                }
            }
        } else {
            if len(parts) < 2 {
                log.Printf("内容改写规则格式不对: %s (例 pattern:replacement)", rule)
                continue
            }
            pattern = parts[0]
            replacement = parts[1]
            if len(parts) == 3 && parts[2] != "" {
				var err error
                count, err = strconv.Atoi(parts[2])
                if err != nil {
                    log.Printf("替换次数字段格式不对 %s: %v", rule, err)
                    continue
                }
            }
        }

        // 将通配符转换为正则表达式
        pattern = regexp.QuoteMeta(pattern)
        pattern = strings.ReplaceAll(pattern, `\*`, ".*")
        pattern = strings.ReplaceAll(pattern, `\?`, "[.\\s]")
        pattern = strings.ReplaceAll(pattern, `\\n`, "\\n")
		replacement=strings.ReplaceAll(replacement, `\n`, "\n")
        if *DEBUG>=3 { log.Printf("DEBUG 正则匹配 %s，替换 %s\n", pattern, replacement) }

		
        regex, err := regexp.Compile(pattern)
        if err != nil {
            log.Printf("字符串匹配异常 %s: %v", pattern, err)
            continue
        }

        *target = append(*target, RegexReplacement{
			id: i,
            Pattern:     regex,
            Replacement: replacement,
            Count:       count,
            HeaderKey:   headerKey,
        })
    }
}

// 修改headers
func modifyHeaders(headers http.Header, rules []RegexReplacement) {
    for _, rule := range rules {
		if *DEBUG>=3 { log.Printf("DEBUG Checking Header rule: %v", rule)}
        if rule.HeaderKey == "" {
            continue
        }
        values, ok := headers[rule.HeaderKey]
		if *DEBUG>=3 { log.Printf("DEBUG Matched Header values: %v", values)}
        if !ok {
			if *DEBUG>=3 { log.Printf("DEBUG Add Header %s : %v", rule.HeaderKey, rule.Replacement)}
			headers.Set(rule.HeaderKey,rule.Replacement)
            continue
        }
		if rule.Replacement=="" {
			if *DEBUG>=3 { log.Printf("DEBUG Delete Header %s", rule.HeaderKey)}
			headers.Del(rule.HeaderKey)
			continue
		}
        for i, value := range values {
            if rule.Count == -1 {
                values[i] = rule.Pattern.ReplaceAllString(value, rule.Replacement)
            } else {
                remaining := rule.Count
                values[i] = rule.Pattern.ReplaceAllStringFunc(value, func(s string) string {
                    if remaining > 0 {
                        remaining--
                        return rule.Replacement
                    }
                    return s
                })
            }
        }
		if *DEBUG>=3 { log.Printf("Modified DEBUG Header %s values: %v", rule.HeaderKey, values)}
        headers[rule.HeaderKey] = values
    }
}

// 修改内容
func modifyContent(content []byte, rules []RegexReplacement) []byte {
    result := content
    for _, rule := range rules {
        if rule.Count == -1 {
            result = rule.Pattern.ReplaceAll(result, []byte(rule.Replacement))
        } else {
            remaining := rule.Count
            result = rule.Pattern.ReplaceAllFunc(result, func(b []byte) []byte {
                if remaining > 0 {
                    remaining--
                    return []byte(rule.Replacement)
                }
                return b
            })
        }
    }
    return result
}

func replaceStrBytes(strBytes []byte, oldStr, newStr string) ([]byte, error) {
    str := string(strBytes)
    replacedStr := strings.ReplaceAll(str, oldStr, newStr)
    return []byte(replacedStr), nil
}

func keywdStrBytes(strBytes []byte, kwdStr string) bool {
    str := string(strBytes)
    return strings.Contains(str, kwdStr)
}